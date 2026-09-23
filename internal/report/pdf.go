// Package report renders a screening result as a one-page PDF.
//
// SPEC.md §8: "Compliance teams buy the report, not the number — treat it as a
// first-class deliverable rather than an afterthought."
//
// The layout is built around that. The reader is a compliance analyst deciding
// whether an address needs investigation, so the page leads with the band and
// the coverage figure together: a score without its coverage is not
// actionable, and presenting the number alone invites it to be read as more
// certain than it is.
package report

import (
	_ "embed"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/go-pdf/fpdf"
	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/shopspring/decimal"
)

// The PDF core fonts cover only Western European text, so a Cyrillic or
// Turkish letter, in the verdict or in an entity name, would print as a wrong
// glyph. Noto Sans covers Latin, Cyrillic and Greek; it is under the SIL Open
// Font License 1.1 (fonts/OFL.txt). Only the glyphs a report uses are embedded.
const sans = "NotoSans"

var (
	//go:embed fonts/NotoSans-Regular.ttf
	fontRegular []byte
	//go:embed fonts/NotoSans-Bold.ttf
	fontBold []byte
	//go:embed fonts/NotoSans-Italic.ttf
	fontItalic []byte
)

// Render writes a one-page PDF report for a screening result in lang ("en",
// "tr" or "ru"). Category keys, entity names and paths' addresses stay as
// the data has them.
func Render(w io.Writer, res *scoring.Result, generatedAt time.Time, lang string) error {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.AddUTF8FontFromBytes(sans, "", fontRegular)
	pdf.AddUTF8FontFromBytes(sans, "B", fontBold)
	pdf.AddUTF8FontFromBytes(sans, "I", fontItalic)
	pdf.SetMargins(15, 15, 15)
	pdf.SetAutoPageBreak(true, 15)
	pdf.AddPage()

	const width = 180
	l := newLoc(lang)

	// --- header ---
	pdf.SetFont(sans, "B", 16)
	pdf.Cell(width, 8, l.f("pdf_title"))
	pdf.Ln(9)

	pdf.SetFont(sans, "", 9)
	pdf.SetTextColor(90, 90, 90)
	pdf.Cell(width, 5, l.f("pdf_subtitle"))
	pdf.Ln(8)
	pdf.SetTextColor(0, 0, 0)

	line(pdf, width)
	pdf.Ln(4)

	// --- subject ---
	pdf.SetFont("Courier", "", 10)
	pdf.Cell(width, 5, res.Address)
	pdf.Ln(5)
	pdf.SetFont(sans, "", 9)
	pdf.SetTextColor(90, 90, 90)
	pdf.Cell(width, 5, l.f("pdf_chain", chainName(res.Chain)))
	pdf.Ln(8)
	pdf.SetTextColor(0, 0, 0)

	// --- verdict: the answer first (docs/DECISIONS.md D29) ---
	if v := res.Verdict; v != nil {
		vr, vg, vb := verdictColour(v.Level)
		pdf.SetFillColor(vr, vg, vb)
		pdf.SetTextColor(255, 255, 255)
		pdf.SetFont(sans, "B", 13)
		pdf.CellFormat(width, 11, "  "+pdfVerdictHead(v, l), "", 0, "L", true, 0, "")
		pdf.Ln(13)
		pdf.SetTextColor(0, 0, 0)
		pdf.SetFont(sans, "", 9.5)
		cv := &ConnectionsVerdict{Level: v.Level, Confidence: v.Confidence, ConfidencePct: v.ConfidencePct, Insufficient: v.Insufficient}
		for _, r := range v.Reasons {
			cv.Reasons = append(cv.Reasons, ConnectionsVerdictReason{Code: r.Code, Category: r.Category, Flag: r.Flag, Address: r.Address, Pct: r.Pct})
		}
		pdf.MultiCell(width, 5, strings.TrimSpace(whyText(cv, l)), "", "L", false)
		pdf.Ln(3)
	}

	// --- headline: band and coverage together ---
	// Deliberately side by side. A score without its coverage is not
	// actionable, and separating them lets the number be read alone.
	r, g, b := bandColour(res.Band)
	pdf.SetFillColor(r, g, b)
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFont(sans, "B", 14)
	// Labelled as exposure so it is not read against the verdict above it.
	fitCell(pdf, 55, 14, l.f("pdf_exposure_band", upper(l, l.band(res.Band))), 14, true)

	pdf.SetTextColor(0, 0, 0)
	pdf.SetFont(sans, "B", 13)
	fitCell(pdf, 60, 14, l.f("pdf_exposure_score", decText(l, res.Score, 1)), 13, false)

	coveragePct := res.Coverage.Mul(decimal.NewFromInt(100))
	if res.LowConfidence {
		pdf.SetTextColor(180, 60, 0)
	}
	fitCell(pdf, 65, 14, l.f("pdf_coverage", pctText(l, coveragePct, 1)), 13, false)
	pdf.SetTextColor(0, 0, 0)
	pdf.Ln(16)

	// --- warnings ---
	// A direct listing on the queried address is shown first. It is a finding
	// independent of any traced exposure, and a listed address with little
	// traced value scores near zero — so a report showing only the band would
	// omit the most important fact on the page.
	if o := res.OwnLabel; o != nil && !res.SanctionsOverride {
		conflict := ""
		if o.Conflicted {
			conflict = l.f("pdf_conflict")
		}
		warning(pdf, width, l.f("pdf_listed_title"),
			l.f("pdf_listed_body", o.Entity, l.category(o.Category), o.Source, fmt.Sprintf("%.2f", o.Confidence), conflict))
	}
	if res.SanctionsOverride {
		if o := res.OwnLabel; o != nil && o.Category != "sanctions" {
			warning(pdf, width, l.f("pdf_direct_listing", upper(l, l.category(o.Category))), l.f("pdf_band_forced", o.Entity))
		} else {
			warning(pdf, width, l.f("pdf_sanctions_title"), l.f("pdf_sanctions_body"))
		}
	}
	if res.LowConfidence {
		warning(pdf, width, l.f("pdf_lowconf_title"), l.f("pdf_lowconf_body",
			pctText(l, coveragePct, 1), pctText(l, decimal.NewFromInt(100).Sub(coveragePct), 1)))
	}
	if res.BandCappedByAbuseRule {
		warning(pdf, width, l.f("pdf_capped_title"), l.f("pdf_capped_body"))
	}

	// --- breakdowns ---
	direction(pdf, width, l, l.f("pdf_inbound"), res.Inbound)
	direction(pdf, width, l, l.f("pdf_outbound"), res.Outbound)

	// --- paths ---
	paths(pdf, width, l, res)

	// --- provenance ---
	pdf.Ln(2)
	line(pdf, width)
	pdf.Ln(3)
	pdf.SetFont(sans, "", 8)
	pdf.SetTextColor(90, 90, 90)
	pdf.Cell(width, 4, l.f("pdf_generated",
		generatedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
		res.LabelSnapshotID, res.ConfigVersion))
	pdf.Ln(4)
	pdf.Cell(width, 4, l.f("pdf_methodology"))
	pdf.Ln(6)

	// --- disclaimer ---
	pdf.SetFont(sans, "I", 7.5)
	pdf.MultiCell(width, 3.4, l.f("pdf_disclaimer"), "", "L", false)

	return pdf.Output(w)
}

// pdfVerdictHead is the chat headline without its emoji, which the font
// does not have: "RISKY · confidence 92%", "НЕТ РИСКА · уверенность 7% ·
// недостаточно данных".
func pdfVerdictHead(v *scoring.Verdict, l loc) string {
	level := v.Level
	if level != "clear" && level != scoring.VerdictHighRisk {
		level = "caution"
	}
	_, head, _ := strings.Cut(strings.TrimSuffix(l.f("v_"+level, v.ConfidencePct), "\n"), " ")
	if v.Insufficient {
		head += strings.TrimSuffix(l.f("v_insufficient"), "\n")
	}
	return head
}

func direction(pdf *fpdf.Fpdf, width float64, l loc, title string, d *scoring.DirectionResult) {
	pdf.Ln(2)
	pdf.SetFont(sans, "B", 10)
	pdf.Cell(width, 6, title)
	pdf.Ln(6)

	if d == nil || (len(d.Categories) == 0 && d.UnattributedPct.IsZero()) {
		pdf.SetFont(sans, "I", 9)
		pdf.SetTextColor(120, 120, 120)
		pdf.Cell(width, 5, l.f("pdf_no_value"))
		pdf.Ln(6)
		pdf.SetTextColor(0, 0, 0)
		return
	}

	pdf.SetFont(sans, "B", 8)
	pdf.SetFillColor(240, 240, 240)
	pdf.CellFormat(70, 5, l.f("pdf_col_category"), "", 0, "L", true, 0, "")
	pdf.CellFormat(28, 5, l.f("pdf_col_share"), "", 0, "R", true, 0, "")
	pdf.CellFormat(28, 5, l.f("pdf_col_weight"), "", 0, "R", true, 0, "")
	pdf.CellFormat(54, 5, l.f("pdf_col_contribution"), "", 0, "R", true, 0, "")
	pdf.Ln(5)

	pdf.SetFont(sans, "", 8)
	for _, c := range d.Categories {
		pdf.CellFormat(70, 4.6, " "+l.category(c.Category), "", 0, "L", false, 0, "")
		pdf.CellFormat(28, 4.6, pctText(l, c.Pct, 2), "", 0, "R", false, 0, "")
		pdf.CellFormat(28, 4.6, c.Weight.StringFixed(0), "", 0, "R", false, 0, "")
		pdf.CellFormat(54, 4.6, decText(l, c.Contribution, 2), "", 0, "R", false, 0, "")
		pdf.Ln(4.6)
	}

	// Unattributed is shown as a row of its own, in the same table. Putting it
	// in a footnote would let a reader skim the categories and take them for
	// the whole picture.
	if d.UnattributedPct.IsPositive() {
		pdf.SetFont(sans, "B", 8)
		pdf.SetTextColor(180, 60, 0)
		pdf.CellFormat(70, 4.6, l.f("pdf_unattributed"), "", 0, "L", false, 0, "")
		pdf.CellFormat(28, 4.6, pctText(l, d.UnattributedPct, 2), "", 0, "R", false, 0, "")
		pdf.CellFormat(28, 4.6, "-", "", 0, "R", false, 0, "")
		pdf.CellFormat(54, 4.6, l.f("pdf_not_scored"), "", 0, "R", false, 0, "")
		pdf.Ln(4.6)
		pdf.SetTextColor(0, 0, 0)
	}

	// SPEC.md §7: the result must be honest about being truncated.
	var notes []string
	if d.FanoutCapped {
		notes = append(notes, l.f("pdf_fanout"))
	}
	if d.HopLimitReached {
		notes = append(notes, l.f("pdf_hoplimit"))
	}
	if len(notes) > 0 {
		pdf.SetFont(sans, "I", 7.5)
		pdf.SetTextColor(120, 120, 120)
		pdf.Cell(width, 4, l.f("pdf_note", strings.Join(notes, "; ")))
		pdf.Ln(5)
		pdf.SetTextColor(0, 0, 0)
	}
}

func paths(pdf *fpdf.Fpdf, width float64, l loc, res *scoring.Result) {
	type entry struct {
		dir  string
		text string
	}
	var entries []entry

	for _, spec := range []struct {
		name string
		d    *scoring.DirectionResult
	}{{l.f("pdf_dir_in"), res.Inbound}, {l.f("pdf_dir_out"), res.Outbound}} {
		if spec.d == nil {
			continue
		}
		for i, p := range spec.d.TopPaths {
			if i >= 3 {
				break
			}
			// A named entity by its name; an unnamed one by its address,
			// which says more than a generic English description would.
			name := p.Terminal.Entity
			if name == "" || p.Terminal.Category == "unnamed_service" {
				name = shortAddress(p.Terminal.Address)
			}
			entries = append(entries, entry{
				dir:  spec.name,
				text: l.f("pdf_path", name, p.HopCount(), l.category(p.Terminal.Category), decText(l, p.Contribution, 4)),
			})
		}
	}

	pdf.Ln(2)
	pdf.SetFont(sans, "B", 10)
	pdf.Cell(width, 6, l.f("pdf_paths"))
	pdf.Ln(6)

	if len(entries) == 0 {
		pdf.SetFont(sans, "I", 9)
		pdf.SetTextColor(120, 120, 120)
		pdf.MultiCell(width, 4.5, l.f("pdf_no_paths"), "", "L", false)
		pdf.SetTextColor(0, 0, 0)
		return
	}

	pdf.SetFont(sans, "", 8)
	for _, e := range entries {
		pdf.CellFormat(12, 4.6, " "+e.dir, "", 0, "L", false, 0, "")
		pdf.CellFormat(width-12, 4.6, e.text, "", 0, "L", false, 0, "")
		pdf.Ln(4.6)
	}
}

func warning(pdf *fpdf.Fpdf, width float64, title, body string) {
	pdf.SetFillColor(255, 244, 230)
	pdf.SetDrawColor(220, 130, 40)
	startY := pdf.GetY()

	pdf.SetFont(sans, "B", 9)
	pdf.SetTextColor(150, 60, 0)
	pdf.MultiCell(width, 5, "  "+title, "", "L", true)
	pdf.SetFont(sans, "", 8)
	pdf.SetTextColor(60, 60, 60)
	pdf.MultiCell(width, 4, "  "+body, "", "L", true)

	pdf.SetDrawColor(220, 130, 40)
	pdf.Rect(15, startY, width, pdf.GetY()-startY, "D")
	pdf.SetTextColor(0, 0, 0)
	pdf.SetDrawColor(0, 0, 0)
	pdf.Ln(3)
}

func line(pdf *fpdf.Fpdf, width float64) {
	y := pdf.GetY()
	pdf.SetDrawColor(200, 200, 200)
	pdf.Line(15, y, 15+width, y)
	pdf.SetDrawColor(0, 0, 0)
}

func verdictColour(level string) (int, int, int) {
	switch level {
	case "clear":
		return 27, 127, 70
	case "high_risk":
		return 196, 43, 43
	default:
		return 180, 110, 0
	}
}

func bandColour(band string) (int, int, int) {
	switch strings.ToLower(band) {
	case "high":
		return 190, 45, 45
	case "medium":
		return 210, 140, 30
	default:
		return 60, 140, 80
	}
}

// upper capitalises for a heading in the reader's language. Turkish has a
// dotted capital İ that strings.ToUpper does not produce.
func upper(l loc, s string) string {
	if l.lang == "tr" {
		return strings.ToUpperSpecial(unicode.TurkishCase, s)
	}
	return strings.ToUpper(s)
}

// fitCell writes a one-line box, shrinking the font from size until the
// text fits: a translated heading is often longer than the English one.
func fitCell(pdf *fpdf.Fpdf, w, h float64, text string, size float64, fill bool) {
	pdf.SetFont(sans, "B", size)
	for size > 7 && pdf.GetStringWidth(text) > w-3 {
		size -= 0.5
		pdf.SetFont(sans, "B", size)
	}
	pdf.CellFormat(w, h, text, "", 0, "C", fill, 0, "")
}

// decText is a number with the reader's decimal separator.
func decText(l loc, d decimal.Decimal, places int32) string {
	s := d.StringFixed(places)
	if l.lang != "en" {
		s = strings.Replace(s, ".", ",", 1)
	}
	return s
}

// pctText is a percentage as the reader writes it: 61.84%, %61,84, 61,84%.
func pctText(l loc, d decimal.Decimal, places int32) string {
	if l.lang == "tr" {
		return "%" + decText(l, d, places)
	}
	return decText(l, d, places) + "%"
}
