package scoring

import (
	"testing"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/shopspring/decimal"
)

func verdictRules() config.Verdict {
	return config.Verdict{
		RedExposure:      map[string]float64{"sanctions": 1, "frozen_funds": 5, "scam": 10},
		ClearMinCoverage: 0.8, ConfidenceHigh: 0.9, ConfidenceMedium: 0.6, MaxPendingPct: 5, MaxUnnamedPct: 50,
		ConfidenceCredit: map[string]float64{"unnamed_service": 0.5, "dust": 0}, BehaviourPenalty: 15, InsufficientBelow: 10,
	}
}

func vresult(band string, coverage float64, cats map[string]float64, done bool) *Result {
	d := &DirectionResult{TotalTraced: decimal.NewFromInt(1)}
	for c, p := range cats {
		d.Categories = append(d.Categories, CategoryShare{Category: c, Pct: decimal.NewFromFloat(p)})
	}
	r := &Result{Band: band, Coverage: decimal.NewFromFloat(coverage), Inbound: d}
	if !done {
		r.Depth = &DepthStatus{FrontierPending: 3}
		d.UnattributedReasons = []ReasonShare{{Reason: "dead_end", Pct: decimal.NewFromFloat((1 - coverage) * 100)}}
	}
	return r
}

func TestVerdict(t *testing.T) {
	rules := verdictRules()
	for _, tc := range []struct {
		name       string
		r          *Result
		level, top string
		confidence string
	}{
		// TKKPgK…dk4V as measured: 54% of traced value reaches a Tether-frozen address.
		{"frozen exposure", vresult("high", 0.918, map[string]float64{"frozen_funds": 54, "unnamed_service": 37}, true),
			VerdictHighRisk, "band_high", "high"},
		{"exposure below the red line is a caution",
			vresult("low", 0.70, map[string]float64{"unnamed_service": 66, "frozen_funds": 3.8}, true),
			VerdictCaution, "exposure_minor", "low"},
		{"clean", vresult("low", 0.999, map[string]float64{"exchange": 80, "unnamed_service": 19.9}, true),
			VerdictClear, "clean", "high"},
		// TTrcHL…BPQp once its hub was labelled: every dollar ends at one
		// unidentified service, which proves nothing about where it came from.
		{"unidentified services are not clean", vresult("low", 1, map[string]float64{"unnamed_service": 100}, true),
			VerdictCaution, "unidentified", "low"},
		{"unknown is not clean", vresult("low", 0.30, map[string]float64{"exchange": 30}, true),
			VerdictCaution, "low_coverage", "low"},
		{"unfinished tracing is not clean", vresult("low", 0.95, map[string]float64{"exchange": 95}, false),
			VerdictCaution, "tracing_incomplete", "medium"},
	} {
		v := Decide(tc.r, rules)
		if v.Level != tc.level || v.Reasons[0].Code != tc.top || v.Confidence != tc.confidence {
			t.Errorf("%s: got %s/%s/%+v", tc.name, v.Level, v.Confidence, v.Reasons)
		}
	}

	// A direct listing is high risk with high confidence, whatever the coverage.
	r := vresult("high", 0.1, nil, false)
	r.OwnLabel, r.SanctionsOverride = &OwnLabel{Category: "frozen_funds"}, true
	if v := Decide(r, rules); v.Level != VerdictHighRisk || v.Reasons[0].Code != "own_listed" || v.Confidence != "high" {
		t.Errorf("direct listing: %+v", v)
	}

	// Sanctions exposure crosses its own, lower line even with a low score.
	if v := Decide(vresult("low", 0.9, map[string]float64{"sanctions": 1.2, "unnamed_service": 88}, true), rules); v.Level != VerdictHighRisk {
		t.Errorf("1.2%% sanctions exposure: %+v", v)
	}

	// Behaviour notes make a clean result a caution.
	r = vresult("low", 0.95, map[string]float64{"exchange": 95}, true)
	r.Flags = []Flag{{Code: "pass_through"}}
	if v := Decide(r, rules); v.Level != VerdictCaution || v.Reasons[0].Code != "behaviour" {
		t.Errorf("pass-through: %+v", v)
	}
}

// One unfetched address carrying a sliver of value does not withhold an answer.
func TestVerdictIgnoresNegligiblePending(t *testing.T) {
	r := vresult("low", 0.97, map[string]float64{"exchange": 97}, true)
	r.Depth = &DepthStatus{FrontierPending: 1}
	r.Inbound.UnattributedReasons = []ReasonShare{{Reason: "dead_end", Pct: decimal.NewFromFloat(1.2)}}
	if v := Decide(r, verdictRules()); v.Level != VerdictClear {
		t.Errorf("1.2%% pending: %+v", v)
	}
}

// Confidence is a percentage of value that could be vouched for.
func TestVerdictConfidencePct(t *testing.T) {
	rules := verdictRules()
	behaviour := vresult("low", 0.99, map[string]float64{"exchange": 99}, true)
	behaviour.Flags = []Flag{{Code: "round_split"}, {Code: "parked_funds"}}
	for _, tc := range []struct {
		name string
		r    *Result
		want int
	}{
		// TPJZrw…uBhM: almost nothing traced, and a round split.
		{"not enough data", vresult("low", 0.02, map[string]float64{"exchange": 2}, true), 2},
		{"named exchanges", vresult("low", 0.99, map[string]float64{"exchange": 99}, true), 99},
		// TTrcHL…BPQp: everything at one unidentified hub is half seen.
		{"unidentified hub", vresult("low", 1, map[string]float64{"unnamed_service": 100}, true), 50},
		{"dust proves nothing", vresult("low", 0.9, map[string]float64{"dust": 60, "exchange": 30}, true), 30},
		{"behaviour notes", behaviour, 69},
		{"risk found at low coverage", vresult("high", 0.3, map[string]float64{"sanctions": 20, "exchange": 10}, true), 50},
		{"risk found at high coverage", vresult("high", 0.92, map[string]float64{"frozen_funds": 54, "exchange": 38}, true), 92},
		{"unfinished tracing is never high", vresult("low", 0.92, map[string]float64{"exchange": 92}, false), 89},
	} {
		v := Decide(tc.r, rules)
		if v.ConfidencePct != tc.want {
			t.Errorf("%s: confidence %d%%, want %d%% (%+v)", tc.name, v.ConfidencePct, tc.want, v)
		}
		// Only a not-risky answer can lack data; found risk stays found.
		if want := v.Level != VerdictHighRisk && tc.want < 10; v.Insufficient != want {
			t.Errorf("%s: insufficient %v, want %v", tc.name, v.Insufficient, want)
		}
	}
}
