package labels

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestMeasurePrecision(t *testing.T) {
	candidates := []DepositCandidate{
		{Address: "A", Accepted: true},  // true positive
		{Address: "B", Accepted: true},  // false positive
		{Address: "C", Accepted: false}, // false negative
		{Address: "D", Accepted: false}, // true negative
		{Address: "E", Accepted: true},  // not in truth -> unknown
	}
	truth := map[string]bool{"A": true, "B": false, "C": true, "D": false}

	p := MeasurePrecision(candidates, truth)
	if p.TruePositives != 1 || p.FalsePositives != 1 || p.FalseNegatives != 1 {
		t.Errorf("counts wrong: tp=%d fp=%d fn=%d", p.TruePositives, p.FalsePositives, p.FalseNegatives)
	}
	if p.Unknown != 1 {
		t.Errorf("unknown = %d, want 1", p.Unknown)
	}

	prec, ok := p.Value()
	if !ok || prec != 0.5 {
		t.Errorf("precision = %v (ok=%v), want 0.5", prec, ok)
	}
	rec, ok := p.Recall()
	if !ok || rec != 0.5 {
		t.Errorf("recall = %v (ok=%v), want 0.5", rec, ok)
	}
}

// An unmeasurable precision must report as unmeasurable rather than as zero.
// Reporting 0.0 for "we have no ground truth" would put a number in
// METHODOLOGY.md that looks like a measurement and is not one.
func TestPrecisionWithNoGroundTruthIsNotZero(t *testing.T) {
	p := MeasurePrecision([]DepositCandidate{{Address: "A", Accepted: true}}, nil)
	if _, ok := p.Value(); ok {
		t.Error("precision must report as unmeasurable when nothing is labelled")
	}
	if p.Unknown != 1 {
		t.Errorf("unknown = %d, want 1", p.Unknown)
	}
}

func TestPctFormatting(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.8, "80.0%"},
		{0.955, "95.5%"},
		{1.0, "100.0%"},
		{0.0, "0.0%"},
	}
	for _, tc := range cases {
		if got := pct(decimal.NewFromFloat(tc.in)); got != tc.want {
			t.Errorf("pct(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCategoryOfInheritsFromHotWallet(t *testing.T) {
	labels := map[string]Label{
		"THot":      {Category: "exchange"},
		"THighRisk": {Category: "high_risk_exchange"},
	}
	if got := categoryOf(labels, "THot"); got != "exchange" {
		t.Errorf("categoryOf = %q, want exchange", got)
	}
	// A deposit wallet of a high-risk exchange is itself high-risk exposure.
	// Flattening both to "exchange" would understate risk, which is the
	// direction that matters.
	if got := categoryOf(labels, "THighRisk"); got != "high_risk_exchange" {
		t.Errorf("categoryOf = %q, want high_risk_exchange", got)
	}
	if got := categoryOf(labels, "TUnknown"); got != "exchange" {
		t.Errorf("categoryOf for an unknown wallet = %q, want the exchange default", got)
	}
}
