package labels

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/mozer/tether-risk/internal/chain/tron"
)

// Sanctions lists that carry crypto addresses only in free text
// (docs/DECISIONS.md D34).
//
// OFAC gives each address a typed field. The UK Sanctions List puts them in
// a designation's <OtherInformation>, and the EU consolidated list in
// <remark> elements, as prose such as "USDT: TGJVc3…" or "TRX: TA1hsi…". So
// every address-shaped token in a designation is taken, and kept only when
// it decodes: a TRON address must pass its base58check checksum, which
// ordinary words cannot.

// FreeTextFormat names a list layout.
type FreeTextFormat string

const (
	FormatUK FreeTextFormat = "uk" // UK Sanctions List (FCDO) XML
	FormatEU FreeTextFormat = "eu" // EU consolidated financial sanctions XML
)

// FreeTextResult reports what a parse found.
type FreeTextResult struct {
	Designations int // records read
	WithAddress  int // records naming at least one address we can use
	ByChain      map[string]int
}

var (
	tronToken = regexp.MustCompile(`\bT[1-9A-HJ-NP-Za-km-z]{33}\b`)
	evmToken  = regexp.MustCompile(`\b0x[0-9a-fA-F]{40}\b`)
)

// designation is one record's name, id, category hint and text.
type designation struct {
	id, name  string
	terrorism bool
	text      strings.Builder
}

// ParseFreeTextSanctions reads a UK or EU sanctions XML file into labels.
func ParseFreeTextSanctions(ctx context.Context, r io.Reader, format FreeTextFormat, source string,
	confidence float64) (*FreeTextResult, []Label, error) {
	record := map[FreeTextFormat]string{FormatUK: "Designation", FormatEU: "sanctionEntity"}[format]
	if record == "" {
		return nil, nil, fmt.Errorf("unknown sanctions format %q", format)
	}
	res := &FreeTextResult{ByChain: map[string]int{}}
	seen := map[string]bool{}
	var out []Label

	dec := xml.NewDecoder(r)
	var cur *designation
	var path []string
	// UK: the primary name's parts, gathered until </Name>.
	var nameParts []string
	var nameType string
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s sanctions xml: %w", format, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			path = append(path, t.Name.Local)
			if t.Name.Local == record {
				cur = &designation{}
				if format == FormatEU {
					for _, a := range t.Attr {
						if a.Name.Local == "euReferenceNumber" {
							cur.id = a.Value
						}
					}
				}
			}
			if cur == nil {
				continue
			}
			for _, a := range t.Attr {
				cur.text.WriteString(" " + a.Value)
			}
			switch {
			case format == FormatEU && t.Name.Local == "nameAlias" && cur.name == "":
				for _, a := range t.Attr {
					if a.Name.Local == "wholeName" {
						cur.name = strings.TrimSpace(a.Value)
					}
				}
			case format == FormatEU && t.Name.Local == "regulation":
				for _, a := range t.Attr {
					if a.Name.Local == "programme" && (a.Value == "TERR" || a.Value == "TAQA") {
						cur.terrorism = true
					}
				}
			case format == FormatUK && t.Name.Local == "Name":
				nameParts, nameType = nil, ""
			}
		case xml.CharData:
			if cur == nil || len(path) == 0 {
				continue
			}
			s := string(t)
			cur.text.WriteString(" " + s)
			if format != FormatUK {
				continue
			}
			switch el := path[len(path)-1]; {
			case el == "UniqueID":
				cur.id = strings.TrimSpace(s)
			case el == "RegimeName" && strings.Contains(s, "Terrorism"):
				cur.terrorism = true
			case el == "NameType":
				nameType = strings.TrimSpace(s)
			case strings.HasPrefix(el, "Name") && len(el) == 5 && strings.TrimSpace(s) != "":
				nameParts = append(nameParts, strings.TrimSpace(s))
			}
		case xml.EndElement:
			if len(path) > 0 {
				path = path[:len(path)-1]
			}
			if cur == nil {
				continue
			}
			if format == FormatUK && t.Name.Local == "Name" && cur.name == "" && nameType == "Primary Name" {
				cur.name = strings.Join(nameParts, " ")
			}
			if t.Name.Local != record {
				continue
			}
			res.Designations++
			ls := designationLabels(cur, format, source, confidence)
			if len(ls) > 0 {
				res.WithAddress++
			}
			for _, l := range ls {
				k := l.Chain + "|" + l.Address
				if seen[k] {
					continue
				}
				seen[k] = true
				res.ByChain[l.Chain]++
				out = append(out, l)
			}
			cur = nil
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Chain != out[j].Chain {
			return out[i].Chain < out[j].Chain
		}
		return out[i].Address < out[j].Address
	})
	return res, out, nil
}

func designationLabels(d *designation, format FreeTextFormat, source string, confidence float64) []Label {
	text := d.text.String()
	category := "sanctions"
	if d.terrorism {
		category = "terrorist_financing"
	}
	name := d.name
	if name == "" {
		name = strings.ToUpper(string(format)) + " sanctions " + d.id
	}
	ev := func(raw string) map[string]any {
		return map[string]any{"designation_id": d.id, "list": string(format), "as_listed": raw}
	}
	var out []Label
	for _, raw := range tronToken.FindAllString(text, -1) {
		a, err := tron.Normalise(raw)
		if err != nil || a == "" {
			continue
		}
		out = append(out, Label{Chain: "tron", Address: a, Entity: name, Category: category,
			Confidence: confidence, Source: source, Evidence: ev(raw)})
	}
	// A key that controls an EVM address controls it on every EVM chain, and
	// the text often does not say which one it means.
	for _, raw := range evmToken.FindAllString(text, -1) {
		for _, chainID := range []string{"ethereum", "bsc"} {
			out = append(out, Label{Chain: chainID, Address: strings.ToLower(raw), Entity: name, Category: category,
				Confidence: confidence, Source: source, Evidence: ev(raw)})
		}
	}
	return out
}
