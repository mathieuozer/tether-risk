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
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/shopspring/decimal"
)

const disclaimer = "This report is automated triage and pre-screening built on open data. " +
	"It is not a regulated AML determination, does not constitute advice, and must not be " +
	"used as the sole basis for any decision about a person or account. Every figure is " +
	"reconstructible from the stored path set identified by the run reference below."

// Render writes a one-page PDF report for a screening result.
func Render(w io.Writer, res *scoring.Result, generatedAt time.Time) error {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(15, 15, 15)
	pdf.SetAutoPageBreak(true, 15)
	pdf.AddPage()

	const width = 180

	// --- header ---
	pdf.SetFont("Helvetica", "B", 16)
	pdf.Cell(width, 8, "Address Risk Screening")
	pdf.Ln(9)

	pdf.SetFont("Helvetica", "", 9)
	pdf.SetTextColor(90, 90, 90)
	pdf.Cell(width, 5, "Triage report - not a regulated AML determination")
	pdf.Ln(8)
	pdf.SetTextColor(0, 0, 0)

	line(pdf, width)
	pdf.Ln(4)

	// --- subject ---
	pdf.SetFont("Courier", "", 10)
	pdf.Cell(width, 5, res.Address)
	pdf.Ln(5)
	pdf.SetFont("Helvetica", "", 9)
	pdf.SetTextColor(90, 90, 90)
	pdf.Cell(width, 5, "Chain: "+res.Chain)
	pdf.Ln(8)
	pdf.SetTextColor(0, 0, 0)

	// --- verdict: the answer first (docs/DECISIONS.md D29) ---
	if v := res.Verdict; v != nil {
		vr, vg, vb := verdictColour(v.Level)
		pdf.SetFillColor(vr, vg, vb)
		pdf.SetTextColor(255, 255, 255)
		pdf.SetFont("Helvetica", "B", 13)
		l := newLoc("en")
		title := map[string]string{"clear": "LOOKS CLEAN", "caution": "CAUTION", "high_risk": "HIGH RISK"}[v.Level]
		pdf.CellFormat(width, 11, fmt.Sprintf("  %s  -  confidence: %s", title, l.f("conf_"+v.Confidence)), "", 0, "L", true, 0, "")
		pdf.Ln(13)
		pdf.SetTextColor(0, 0, 0)
		pdf.SetFont("Helvetica", "", 9.5)
		for _, r := range v.Reasons {
			pdf.MultiCell(width, 5, "- "+verdictReasonText(l, r), "", "L", false)
		}
		pdf.Ln(3)
	}

	// --- headline: band and coverage together ---
	// Deliberately side by side. A score without its coverage is not
	// actionable, and separating them lets the number be read alone.
	r, g, b := bandColour(res.Band)
	pdf.SetFillColor(r, g, b)
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFont("Helvetica", "B", 14)
	// Labelled as exposure so it is not read against the verdict above it.
	pdf.CellFormat(55, 14, "EXPOSURE "+strings.ToUpper(res.Band), "", 0, "C", true, 0, "")

	pdf.SetTextColor(0, 0, 0)
	pdf.SetFont("Helvetica", "B", 13)
	pdf.CellFormat(60, 14, "Exposure score "+res.Score.StringFixed(1)+" / 100", "", 0, "C", false, 0, "")

	coveragePct := res.Coverage.Mul(decimal.NewFromInt(100))
	if res.LowConfidence {
		pdf.SetTextColor(180, 60, 0)
	}
	pdf.CellFormat(65, 14, "Coverage "+coveragePct.StringFixed(1)+"%", "", 0, "C", false, 0, "")
	pdf.SetTextColor(0, 0, 0)
	pdf.Ln(16)

	// --- warnings ---
	// A direct listing on the queried address is shown first. It is a finding
	// independent of any traced exposure, and a listed address with little
	// traced value scores near zero — so a report showing only the band would
	// omit the most important fact on the page.
	if l := res.OwnLabel; l != nil && !res.SanctionsOverride {
		conflict := ""
		if l.Conflicted {
			conflict = " Sources disagree about this address; the conflict is recorded for review."
		}
		warning(pdf, width, "THIS ADDRESS IS DIRECTLY LISTED",
			fmt.Sprintf("%s - categorised %s by %s (confidence %.2f). This is a direct "+
				"listing, not exposure traced through other addresses.%s",
				l.Entity, l.Category, l.Source, l.Confidence, conflict))
	}
	if res.SanctionsOverride {
		if l := res.OwnLabel; l != nil && l.Category != "sanctions" {
			warning(pdf, width, "DIRECT LISTING: "+strings.ToUpper(newLoc("en").category(l.Category)),
				fmt.Sprintf("%s. The band is set to High regardless of the computed score.", l.Entity))
		} else {
			warning(pdf, width, "DIRECT SANCTIONS MATCH",
				"This address appears on a sanctions list. The band is set to High regardless "+
					"of the computed score.")
		}
	}
	if res.LowConfidence {
		warning(pdf, width, "LOW CONFIDENCE - READ BEFORE USING",
			fmt.Sprintf("Only %s%% of traced value could be attributed to a known entity. "+
				"The score describes that portion only. The remaining %s%% is unknown, "+
				"not clean, and this report makes no claim about it.",
				coveragePct.StringFixed(1),
				decimal.NewFromInt(100).Sub(coveragePct).StringFixed(1)))
	}
	if res.BandCappedByAbuseRule {
		warning(pdf, width, "BAND CAPPED",
			"The only evidence for this address is unverified community abuse reports. "+
				"The band is capped accordingly and should not be read as a cleared result.")
	}

	// --- breakdowns ---
	direction(pdf, width, "INBOUND - where funds came from", res.Inbound)
	direction(pdf, width, "OUTBOUND - where funds went", res.Outbound)

	// --- paths ---
	paths(pdf, width, res)

	// --- provenance ---
	pdf.Ln(2)
	line(pdf, width)
	pdf.Ln(3)
	pdf.SetFont("Helvetica", "", 8)
	pdf.SetTextColor(90, 90, 90)
	pdf.Cell(width, 4, fmt.Sprintf(
		"Generated %s   |   Label snapshot %d   |   Config version %s",
		generatedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
		res.LabelSnapshotID, res.ConfigVersion))
	pdf.Ln(4)
	pdf.Cell(width, 4, "Methodology: docs/METHODOLOGY.md")
	pdf.Ln(6)

	// --- disclaimer ---
	pdf.SetFont("Helvetica", "I", 7.5)
	pdf.MultiCell(width, 3.4, disclaimer, "", "L", false)

	return pdf.Output(w)
}

func direction(pdf *fpdf.Fpdf, width float64, title string, d *scoring.DirectionResult) {
	pdf.Ln(2)
	pdf.SetFont("Helvetica", "B", 10)
	pdf.Cell(width, 6, title)
	pdf.Ln(6)

	if d == nil || (len(d.Categories) == 0 && d.UnattributedPct.IsZero()) {
		pdf.SetFont("Helvetica", "I", 9)
		pdf.SetTextColor(120, 120, 120)
		pdf.Cell(width, 5, "No traced value in this direction.")
		pdf.Ln(6)
		pdf.SetTextColor(0, 0, 0)
		return
	}

	pdf.SetFont("Helvetica", "B", 8)
	pdf.SetFillColor(240, 240, 240)
	pdf.CellFormat(70, 5, " Category", "", 0, "L", true, 0, "")
	pdf.CellFormat(28, 5, "Share", "", 0, "R", true, 0, "")
	pdf.CellFormat(28, 5, "Weight", "", 0, "R", true, 0, "")
	pdf.CellFormat(54, 5, "Contribution to score", "", 0, "R", true, 0, "")
	pdf.Ln(5)

	pdf.SetFont("Helvetica", "", 8)
	for _, c := range d.Categories {
		pdf.CellFormat(70, 4.6, " "+c.Category, "", 0, "L", false, 0, "")
		pdf.CellFormat(28, 4.6, c.Pct.StringFixed(2)+"%", "", 0, "R", false, 0, "")
		pdf.CellFormat(28, 4.6, c.Weight.StringFixed(0), "", 0, "R", false, 0, "")
		pdf.CellFormat(54, 4.6, c.Contribution.StringFixed(2), "", 0, "R", false, 0, "")
		pdf.Ln(4.6)
	}

	// Unattributed is shown as a row of its own, in the same table. Putting it
	// in a footnote would let a reader skim the categories and take them for
	// the whole picture.
	if d.UnattributedPct.IsPositive() {
		pdf.SetFont("Helvetica", "B", 8)
		pdf.SetTextColor(180, 60, 0)
		pdf.CellFormat(70, 4.6, " unattributed", "", 0, "L", false, 0, "")
		pdf.CellFormat(28, 4.6, d.UnattributedPct.StringFixed(2)+"%", "", 0, "R", false, 0, "")
		pdf.CellFormat(28, 4.6, "-", "", 0, "R", false, 0, "")
		pdf.CellFormat(54, 4.6, "not scored - unknown", "", 0, "R", false, 0, "")
		pdf.Ln(4.6)
		pdf.SetTextColor(0, 0, 0)
	}

	// SPEC.md §7: the result must be honest about being truncated.
	var notes []string
	if d.FanoutCapped {
		notes = append(notes, "neighbour cap reached; this traversal is truncated")
	}
	if d.HopLimitReached {
		notes = append(notes, "hop limit reached; value beyond it is unknown")
	}
	if len(notes) > 0 {
		pdf.SetFont("Helvetica", "I", 7.5)
		pdf.SetTextColor(120, 120, 120)
		pdf.Cell(width, 4, " Note: "+strings.Join(notes, "; "))
		pdf.Ln(5)
		pdf.SetTextColor(0, 0, 0)
	}
}

func paths(pdf *fpdf.Fpdf, width float64, res *scoring.Result) {
	type entry struct {
		dir  string
		text string
	}
	var entries []entry

	for _, spec := range []struct {
		name string
		d    *scoring.DirectionResult
	}{{"in", res.Inbound}, {"out", res.Outbound}} {
		if spec.d == nil {
			continue
		}
		for i, p := range spec.d.TopPaths {
			if i >= 3 {
				break
			}
			name := p.Terminal.Entity
			if name == "" {
				name = p.Terminal.Address
			}
			entries = append(entries, entry{
				dir: spec.name,
				text: fmt.Sprintf("%s via %d hop(s) - %s - contributes %s",
					name, p.HopCount(), p.Terminal.Category, p.Contribution.StringFixed(4)),
			})
		}
	}

	pdf.Ln(2)
	pdf.SetFont("Helvetica", "B", 10)
	pdf.Cell(width, 6, "TOP CONTRIBUTING PATHS")
	pdf.Ln(6)

	if len(entries) == 0 {
		pdf.SetFont("Helvetica", "I", 9)
		pdf.SetTextColor(120, 120, 120)
		pdf.MultiCell(width, 4.5,
			"No path reached an identified counterparty. This is a coverage gap, not "+
				"an absence of activity: flows were traced but could not be attributed "+
				"to a named entity.", "", "L", false)
		pdf.SetTextColor(0, 0, 0)
		return
	}

	pdf.SetFont("Helvetica", "", 8)
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

	pdf.SetFont("Helvetica", "B", 9)
	pdf.SetTextColor(150, 60, 0)
	pdf.MultiCell(width, 5, "  "+title, "", "L", true)
	pdf.SetFont("Helvetica", "", 8)
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

// verdictReasonText renders one verdict reason as a sentence, reusing the
// report's catalogue without its bullet and line break.
func verdictReasonText(l loc, r scoring.VerdictReason) string {
	var s string
	switch r.Code {
	case "own_listed":
		s = l.f("vr_own_listed", l.category(r.Category))
	case "exposure", "exposure_minor":
		s = l.f("vr_"+r.Code, l.pct(r.Pct), l.category(r.Category))
	case "low_coverage", "unidentified", "clean":
		s = l.f("vr_"+r.Code, l.pct(r.Pct))
	case "behaviour":
		s = l.f("vr_behaviour", l.f("flagname_"+r.Flag))
	default:
		s = l.f("vr_" + r.Code)
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "•"))
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
