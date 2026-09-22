package scoring

import (
	"sort"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/shopspring/decimal"
)

// Verdict is the answer to "is this wallet clean?" (docs/DECISIONS.md D29):
// three states, the reasons, and how much to trust it. It is derived from a
// finished Result and changes nothing in it.
type Verdict struct {
	// Level is clear, caution or high_risk.
	Level string
	// Confidence is high, medium or low.
	Confidence string
	Reasons    []VerdictReason
}

// VerdictReason is one fact behind a verdict, rendered by each channel in
// its own language.
type VerdictReason struct {
	// Code: own_listed, band_high, exposure, band_medium, exposure_minor,
	// low_coverage, tracing_incomplete, behaviour, clean.
	Code     string
	Category string  // for own_listed, exposure, exposure_minor
	Pct      float64 // exposure share, or coverage for low_coverage
	Flag     string  // for behaviour
}

// Verdict levels.
const (
	VerdictClear    = "clear"
	VerdictCaution  = "caution"
	VerdictHighRisk = "high_risk"
)

// riskCategories are those whose exposure is a finding.
var riskCategories = map[string]bool{
	"sanctions": true, "terrorist_financing": true, "frozen_funds": true, "darknet": true,
	"stolen_funds": true, "mixer": true, "scam": true, "high_risk_exchange": true, "gambling": true,
}

// CombinedShares merges both directions into shares of all traced value,
// weighted by each direction's traced total, as the report does.
func CombinedShares(r *Result) map[string]float64 {
	var total decimal.Decimal
	for _, d := range []*DirectionResult{r.Inbound, r.Outbound} {
		if d != nil && d.TotalTraced.IsPositive() {
			total = total.Add(d.TotalTraced)
		}
	}
	out := map[string]float64{}
	if !total.IsPositive() {
		return out
	}
	for _, d := range []*DirectionResult{r.Inbound, r.Outbound} {
		if d == nil || !d.TotalTraced.IsPositive() {
			continue
		}
		w := d.TotalTraced.Div(total)
		for _, c := range d.Categories {
			out[c.Category] += c.Pct.Mul(w).InexactFloat64()
		}
	}
	return out
}

// Decide computes the verdict for a result.
func Decide(r *Result, rules config.Verdict) Verdict {
	shares := CombinedShares(r)
	coverage := r.Coverage.InexactFloat64()
	// Tracing is finished for the verdict when what still waits at unfetched
	// dead ends is too little to change the answer.
	done := r.Depth == nil || r.Depth.Complete() || pendingShare(r) < rules.MaxPendingPct

	var red, caution []VerdictReason
	if r.OwnLabel != nil && (riskCategories[r.OwnLabel.Category] || r.SanctionsOverride) {
		red = append(red, VerdictReason{Code: "own_listed", Category: r.OwnLabel.Category})
	}
	if r.Band == "high" && len(red) == 0 {
		red = append(red, VerdictReason{Code: "band_high"})
	}

	// Risk exposures, largest first, each either decisive or a caution.
	cats := make([]string, 0, len(shares))
	for c := range shares {
		if riskCategories[c] && shares[c] > 0 {
			cats = append(cats, c)
		}
	}
	sort.Slice(cats, func(i, j int) bool {
		if shares[cats[i]] != shares[cats[j]] {
			return shares[cats[i]] > shares[cats[j]]
		}
		return cats[i] < cats[j]
	})
	for _, c := range cats {
		limit, has := rules.RedExposure[c]
		if has && shares[c] >= limit {
			red = append(red, VerdictReason{Code: "exposure", Category: c, Pct: shares[c]})
		} else {
			caution = append(caution, VerdictReason{Code: "exposure_minor", Category: c, Pct: shares[c]})
		}
	}

	if r.Band == "medium" {
		caution = append(caution, VerdictReason{Code: "band_medium"})
	}
	if coverage < rules.ClearMinCoverage {
		caution = append(caution, VerdictReason{Code: "low_coverage", Pct: coverage * 100})
	}
	if !done {
		caution = append(caution, VerdictReason{Code: "tracing_incomplete"})
	}
	for _, f := range r.Flags {
		if f.Code == "pass_through" || f.Code == "high_volume_new" {
			caution = append(caution, VerdictReason{Code: "behaviour", Flag: f.Code})
		}
	}

	v := Verdict{Confidence: confidence(coverage, done, rules)}
	switch {
	case len(red) > 0:
		v.Level, v.Reasons = VerdictHighRisk, append(red, caution...)
		// A direct listing is certain whatever the coverage: the address
		// itself is on the list.
		if red[0].Code == "own_listed" {
			v.Confidence = "high"
		}
	case len(caution) > 0:
		v.Level, v.Reasons = VerdictCaution, caution
	default:
		v.Level = VerdictClear
		v.Reasons = []VerdictReason{{Code: "clean", Pct: coverage * 100}}
	}
	return v
}

// pendingShare is the share of traced value, in percent, stopped at dead
// ends: addresses whose history is not stored yet.
func pendingShare(r *Result) float64 {
	var total decimal.Decimal
	for _, d := range []*DirectionResult{r.Inbound, r.Outbound} {
		if d != nil && d.TotalTraced.IsPositive() {
			total = total.Add(d.TotalTraced)
		}
	}
	if !total.IsPositive() {
		return 0
	}
	var pct float64
	for _, d := range []*DirectionResult{r.Inbound, r.Outbound} {
		if d == nil || !d.TotalTraced.IsPositive() {
			continue
		}
		w := d.TotalTraced.Div(total)
		for _, rs := range d.UnattributedReasons {
			if rs.Reason == "dead_end" {
				pct += rs.Pct.Mul(w).InexactFloat64()
			}
		}
	}
	return pct
}

func confidence(coverage float64, done bool, rules config.Verdict) string {
	switch {
	case coverage >= rules.ConfidenceHigh && done:
		return "high"
	case coverage >= rules.ConfidenceMedium:
		return "medium"
	default:
		return "low"
	}
}
