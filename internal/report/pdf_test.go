package report

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/shopspring/decimal"
)

// hasGlyph reports whether a TrueType font maps r to a glyph other than
// .notdef, reading its Windows Unicode BMP cmap (format 4).
func hasGlyph(font []byte, r rune) bool {
	u16 := func(off int) int { return int(binary.BigEndian.Uint16(font[off:])) }
	u32 := func(off int) int { return int(binary.BigEndian.Uint32(font[off:])) }
	cmap := -1
	for i := 0; i < u16(4); i++ {
		if rec := 12 + 16*i; string(font[rec:rec+4]) == "cmap" {
			cmap = u32(rec + 8)
		}
	}
	if cmap < 0 || r > 0xFFFF {
		return false
	}
	for i := 0; i < u16(cmap+2); i++ {
		rec := cmap + 4 + 8*i
		sub := cmap + u32(rec+4)
		if u16(rec) != 3 || u16(rec+2) != 1 || u16(sub) != 4 {
			continue
		}
		segs := u16(sub+6) / 2
		ends, starts := sub+14, sub+16+2*segs
		deltas, offsets := starts+2*segs, starts+4*segs
		for s := 0; s < segs; s++ {
			if int(r) > u16(ends+2*s) || int(r) < u16(starts+2*s) {
				continue
			}
			glyph := 0
			if ro := u16(offsets + 2*s); ro == 0 {
				glyph = (int(r) + u16(deltas+2*s)) & 0xFFFF
			} else if g := u16(offsets + 2*s + ro + 2*(int(r)-u16(starts+2*s))); g != 0 {
				glyph = (g + u16(deltas+2*s)) & 0xFFFF
			}
			return glyph != 0
		}
	}
	return false
}

// Every letter the Russian and Turkish catalogues can put on the page has a
// glyph in the embedded fonts, and a Russian report renders.
func TestPDFFontsCoverEveryLanguage(t *testing.T) {
	if !hasGlyph(fontRegular, 'Ж') || !hasGlyph(fontRegular, 'ı') || hasGlyph(fontRegular, '中') {
		t.Fatal("hasGlyph does not read the font")
	}
	var text []string
	for _, m := range []map[string]string{enText, trText, ruText, categoryNames, trCategoryNames, ruCategoryNames} {
		for _, s := range m {
			text = append(text, s)
		}
	}
	for _, forms := range ruNouns {
		text = append(text, forms[:]...)
	}
	for _, font := range map[string][]byte{"regular": fontRegular, "bold": fontBold, "italic": fontItalic} {
		for _, s := range text {
			for _, r := range s {
				// Emoji are chat-only; the PDF strips them.
				if r >= 0x2190 || unicode.IsSpace(r) || unicode.IsControl(r) {
					continue
				}
				if !hasGlyph(font, r) {
					t.Fatalf("no glyph for %q (U+%04X) in %q", r, r, s)
				}
			}
		}
	}

	res := &scoring.Result{Address: "TBkgVghdGoFYpP3EajvuNt8j8qX4xEEtN8", Chain: "tron", Band: "high",
		Score: decimal.NewFromFloat(63.9), Coverage: decimal.NewFromFloat(0.918),
		Verdict: &scoring.Verdict{Level: scoring.VerdictHighRisk, ConfidencePct: 92, Reasons: []scoring.VerdictReason{
			{Code: "band_high"}, {Code: "exposure", Category: "frozen_funds", Pct: 54.05}}}}
	var buf bytes.Buffer
	if err := Render(&buf, res, time.Unix(0, 0), "ru"); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("%PDF")) {
		t.Fatal("not a PDF")
	}
	if got := pdfVerdictHead(res.Verdict, newLoc("ru")); got != "ЕСТЬ РИСК · уверенность 92%" {
		t.Errorf("head = %q", got)
	}
	v := &scoring.Verdict{Level: scoring.VerdictCaution, ConfidencePct: 7, Insufficient: true}
	if got := pdfVerdictHead(v, newLoc("en")); got != "NOT RISKY · confidence 7% · not enough data" {
		t.Errorf("head = %q", got)
	}
	if strings.Contains(pdfVerdictHead(v, newLoc("ru")), "🟡") {
		t.Error("emoji left in the PDF headline")
	}
}
