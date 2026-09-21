package labels

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/mozer/tether-risk/internal/chain/tron"
)

// OFAC SDN ingestion.
//
// SPEC.md §6.1: parse the published XML, extract crypto addresses, run daily,
// confidence 1.0. SPEC.md §9.1 makes this the highest-stakes ingester in the
// system: every OFAC address must land in the High band, and any miss is a
// build-breaking bug. A parser that silently drops an address is therefore
// worse than one that fails loudly, and this one is written accordingly.

// sdnEntry mirrors the published XML. Only the fields we use are declared;
// encoding/xml ignores the rest.
type sdnEntry struct {
	UID       string `xml:"uid"`
	FirstName string `xml:"firstName"`
	LastName  string `xml:"lastName"`
	SDNType   string `xml:"sdnType"`
	Remarks   string `xml:"remarks"`

	ProgramList struct {
		Programs []string `xml:"program"`
	} `xml:"programList"`

	IDList struct {
		IDs []struct {
			UID      string `xml:"uid"`
			IDType   string `xml:"idType"`
			IDNumber string `xml:"idNumber"`
		} `xml:"id"`
	} `xml:"idList"`
}

const digitalCurrencyPrefix = "Digital Currency Address - "

// OFACResult reports what an ingestion run found, including what it chose not
// to store. Counting skips explicitly is what makes it possible to notice that
// a feed changed shape.
type OFACResult struct {
	EntriesScanned  int
	AddressesFound  int
	LabelsProduced  int
	SkippedOffChain map[string]int // currency code -> count, chains not in scope
	Malformed       []string       // addresses that failed validation
}

// ParseOFAC streams the SDN XML and produces labels for in-scope chains.
//
// The file is around 29MB with 19,000 entries, so it is streamed rather than
// read whole.
func ParseOFAC(ctx context.Context, r io.Reader) (*OFACResult, []Label, error) {
	dec := xml.NewDecoder(r)
	// The published XML declares a default namespace; matching on local names
	// keeps the parser working if that namespace URI is ever revised.
	dec.DefaultSpace = ""

	res := &OFACResult{SkippedOffChain: map[string]int{}}
	var out []Label

	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}

		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("ofac: read xml: %w", err)
		}

		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "sdnEntry" {
			continue
		}

		var e sdnEntry
		if err := dec.DecodeElement(&e, &start); err != nil {
			return nil, nil, fmt.Errorf("ofac: decode entry: %w", err)
		}
		res.EntriesScanned++

		entity := entityName(e)
		category := ofacCategory(e.ProgramList.Programs)

		for _, id := range e.IDList.IDs {
			if !strings.HasPrefix(id.IDType, digitalCurrencyPrefix) {
				continue
			}
			res.AddressesFound++

			currency := strings.TrimSpace(strings.TrimPrefix(id.IDType, digitalCurrencyPrefix))
			address := strings.TrimSpace(id.IDNumber)

			chainID, ok := chainForAddress(currency, address)
			if !ok {
				// Out of scope for v1 (Bitcoin, Monero, Solana and so on).
				// Counted rather than dropped: if a chain later comes into
				// scope, this number says how much is waiting.
				res.SkippedOffChain[currency]++
				continue
			}

			canonical, err := canonicalise(chainID, address)
			if err != nil {
				// A sanctioned address we cannot parse must be visible, not
				// swallowed. The caller decides whether to fail the run.
				res.Malformed = append(res.Malformed,
					fmt.Sprintf("%s (%s, sdn uid %s): %v", address, currency, e.UID, err))
				continue
			}

			out = append(out, Label{
				Chain:      chainID,
				Address:    canonical,
				Entity:     entity,
				Category:   category,
				Confidence: 1.0,
				Source:     "ofac",
				Evidence: map[string]any{
					"sdn_uid":       e.UID,
					"id_uid":        id.UID,
					"programs":      e.ProgramList.Programs,
					"sdn_type":      e.SDNType,
					"currency_code": currency,
					"listed_as":     address,
					"remarks":       e.Remarks,
				},
			})
			res.LabelsProduced++
		}
	}

	return res, out, nil
}

// entityName renders the SDN entry's name. OFAC labels read like
// "OFAC SDN #4632" in SPEC.md §4's example, but the actual name is far more
// useful in a report, so both are carried: the name here, the uid in evidence.
func entityName(e sdnEntry) string {
	first := strings.TrimSpace(e.FirstName)
	last := strings.TrimSpace(e.LastName)
	switch {
	case first != "" && last != "":
		return first + " " + last
	case last != "":
		return last
	case first != "":
		return first
	default:
		return "OFAC SDN #" + e.UID
	}
}

// ofacCategory maps sanctions programs onto the controlled vocabulary.
//
// SDGT (Specially Designated Global Terrorist), SDT and FTO designations are
// terrorist financing specifically. Both categories carry weight 100 and both
// always win resolution, so this does not change any score — it makes the
// explanation accurate, which is what the report is for.
func ofacCategory(programs []string) string {
	for _, p := range programs {
		switch strings.ToUpper(strings.TrimSpace(p)) {
		case "SDGT", "SDT", "FTO":
			return "terrorist_financing"
		}
	}
	return "sanctions"
}

// chainForAddress resolves the chain for an OFAC currency code.
//
// Several codes are ambiguous about which chain they refer to: USDT and USDC
// exist on TRON, Ethereum and others, and OFAC records only the asset. The
// address format disambiguates, so it is used rather than guessed — a USDT
// address beginning with 'T' is TRON, one beginning with '0x' is EVM.
//
// Returning false means out of scope for v1 rather than unrecognised.
func chainForAddress(currency, address string) (string, bool) {
	switch strings.ToUpper(currency) {
	case "TRX":
		return "tron", true
	case "ETH", "ETC", "ARB":
		return "ethereum", true

	case "BSC", "BNB":
		// OFAC's "BNB" covers two different chains. BNB Smart Chain uses
		// 20-byte hex addresses; Binance Beacon Chain (BEP-2) uses bech32
		// addresses beginning "bnb1", and is a separate chain we do not
		// ingest.
		//
		// Mapping every BNB listing to bsc would file a Beacon Chain address
		// as a BSC address, where it could never match anything — a silent
		// sanctions miss, which SPEC.md §9.1 calls build-breaking. Found by
		// running the parser against the full published feed, which is why
		// that test exists.
		if strings.HasPrefix(address, "0x") && len(address) == 42 {
			return "bsc", true
		}
		return "", false

	case "USDT", "USDC":
		// Ambiguous by asset; decided by address shape.
		switch {
		case strings.HasPrefix(address, "T") && len(address) == 34:
			return "tron", true
		case strings.HasPrefix(address, "0x") && len(address) == 42:
			return "ethereum", true
		default:
			return "", false
		}

	default:
		// XBT, LTC, XMR, BCH, DASH, ZEC, SOL, DOGE, XRP and the rest.
		// SPEC.md §1: no Bitcoin in v1, no UTXO clustering.
		return "", false
	}
}

// canonicalise normalises an address to the form used in `edges`. A sanctioned
// address stored in a different encoding than the chain data would never
// match, and the miss would be silent — exactly the failure SPEC.md §9.1 calls
// build-breaking.
func canonicalise(chainID, address string) (string, error) {
	switch chainID {
	case "tron":
		return tron.Normalise(address)
	case "ethereum", "bsc":
		a := strings.TrimSpace(address)
		if !strings.HasPrefix(a, "0x") || len(a) != 42 {
			return "", fmt.Errorf("not a 20-byte hex address")
		}
		// Lower-cased: EIP-55 checksummed and all-lower forms are the same
		// address, and storing both would split the label.
		return strings.ToLower(a), nil
	default:
		return "", fmt.Errorf("unknown chain %q", chainID)
	}
}
