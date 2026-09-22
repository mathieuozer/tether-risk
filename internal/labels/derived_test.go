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

// Only directly established hot wallets anchor the deposit heuristic. A label
// the heuristic produced must never anchor it, or each run would label the
// senders of the previous run's deposit wallets and the error would compound.
func TestDepositAnchorsExcludeDerivedDeposits(t *testing.T) {
	got := DepositAnchors([]Label{
		{ID: 1, Address: "THot", Category: "exchange", Source: "curated", Confidence: 0.95},
		{ID: 2, Address: "TDeposit", Category: "exchange", Source: "derived:deposit", Confidence: 0.8},
		{ID: 3, Address: "TRisky", Category: "high_risk_exchange", Source: "curated", Confidence: 0.95},
		{ID: 4, Address: "TScam", Category: "scam", Source: "scamsniffer", Confidence: 0.5},
	}, nil)
	if len(got) != 2 {
		t.Fatalf("got %d anchors, want 2: %v", len(got), got)
	}
	for _, addr := range []string{"THot", "TRisky"} {
		if _, ok := got[addr]; !ok {
			t.Errorf("missing anchor %s", addr)
		}
	}
	if _, ok := got["TDeposit"]; ok {
		t.Error("a derived deposit wallet must not anchor the heuristic")
	}
}

func TestDepositAnchorsPickMostConfidentLabel(t *testing.T) {
	got := DepositAnchors([]Label{
		{ID: 7, Address: "THot", Category: "exchange", Source: "curated", Confidence: 0.95, Entity: "Curated name"},
		{ID: 3, Address: "THot", Category: "high_risk_exchange", Source: "dune", Confidence: 0.8, Entity: "Dune name"},
	}, nil)
	if got["THot"].Entity != "Curated name" {
		t.Errorf("picked %q, want the more confident label", got["THot"].Entity)
	}
}

// An exchange's own reserve list anchors the heuristic even though its labels
// are unnamed_service: it proves who controls the address. A derived service
// label in the same category does not, because it names nobody.
func TestDepositAnchorsIncludeReserveLists(t *testing.T) {
	got := DepositAnchors([]Label{
		{ID: 1, Address: "THtx", Category: "unnamed_service", Source: "htx_por", Confidence: 0.9, Entity: "HTX"},
		{ID: 2, Address: "TBusy", Category: "unnamed_service", Source: "derived:service", Confidence: 0.75,
			Entity: "Unidentified high-volume service"},
		{ID: 3, Address: "TNoName", Category: "unnamed_service", Source: "htx_por", Confidence: 0.9},
	}, []string{"htx_por", "poloniex_por"})
	if len(got) != 1 || got["THtx"].Entity != "HTX" {
		t.Fatalf("got %v, want only THtx anchored as HTX", got)
	}
}

func TestDepositEntityNamesWhatWasObserved(t *testing.T) {
	sources := []string{"htx_por", "poloniex_por"}
	for _, tc := range []struct {
		anchor Label
		want   string
	}{
		{Label{Entity: "Poloniex (proof-of-reserves wallet)", Source: "poloniex_por"}, "Poloniex (sends to its reserves)"},
		{Label{Entity: "Binance hot wallet", Source: "curated"}, "Binance hot wallet (deposit wallet)"},
		{Label{Source: "curated"}, "THot (deposit wallet)"},
	} {
		if got := depositEntity(tc.anchor, "THot", sources); got != tc.want {
			t.Errorf("depositEntity(%+v) = %q, want %q", tc.anchor, got, tc.want)
		}
	}
}
