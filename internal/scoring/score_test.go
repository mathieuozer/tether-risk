package scoring

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/graph"
	"github.com/shopspring/decimal"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func path(terminal, category, reason string, contribution float64) graph.Path {
	return graph.Path{
		Hops:         []string{terminal},
		Shares:       []decimal.Decimal{decimal.NewFromFloat(1)},
		Contribution: decimal.NewFromFloat(contribution),
		Terminal: graph.Terminal{
			Address: terminal, Category: category, Reason: reason, Entity: terminal,
		},
	}
}

func result(paths ...graph.Path) *graph.Result {
	r := &graph.Result{
		Chain: "tron", Address: "TOrigin", Direction: graph.Outbound,
		Paths: paths, TotalTraced: decimal.Zero, Attributed: decimal.Zero,
	}
	for _, p := range paths {
		r.TotalTraced = r.TotalTraced.Add(p.Contribution)
		if p.Terminal.Attributed() {
			r.Attributed = r.Attributed.Add(p.Contribution)
		}
	}
	return r
}

// SPEC.md §7: score = sum(category_pct * category_weight) / 100
func TestScoreFormula(t *testing.T) {
	s := New(testConfig(t))

	// Half the traced value to a mixer (weight 70), half to an exchange
	// (weight 2). Expected: 50*70/100 + 50*2/100 = 35 + 1 = 36.
	d, err := s.ScoreDirection(result(
		path("TMixer", "mixer", "labelled", 0.5),
		path("TExchange", "exchange", "labelled", 0.5),
	))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Score.Equal(decimal.NewFromInt(36)) {
		t.Errorf("score = %s, want 36 (50%%x70 + 50%%x2)/100", d.Score)
	}
	if !d.Coverage.Equal(decimal.NewFromInt(1)) {
		t.Errorf("coverage = %s, want 1", d.Coverage)
	}
}

// SPEC.md §7: coverage = attributed / total traced, reported always.
// Unattributed value must never be dropped from the denominator — doing so
// would inflate every percentage and hide the unknown.
func TestUnattributedValueLowersCoverageNotPercentages(t *testing.T) {
	s := New(testConfig(t))

	// 20% to a mixer, 80% unidentified.
	d, err := s.ScoreDirection(result(
		path("TMixer", "mixer", "labelled", 0.2),
		path("TUnknown", "", "hop_limit", 0.8),
	))
	if err != nil {
		t.Fatal(err)
	}

	if len(d.Categories) != 1 {
		t.Fatalf("categories = %d, want 1", len(d.Categories))
	}
	if !d.Categories[0].Pct.Equal(decimal.NewFromInt(20)) {
		t.Errorf("mixer pct = %s, want 20; unattributed value must stay in the "+
			"denominator or every percentage is inflated", d.Categories[0].Pct)
	}
	if !d.UnattributedPct.Equal(decimal.NewFromInt(80)) {
		t.Errorf("unattributed = %s, want 80", d.UnattributedPct)
	}
	if !d.Coverage.Equal(decimal.NewFromFloat(0.2)) {
		t.Errorf("coverage = %s, want 0.2", d.Coverage)
	}
	// Score reflects only what was actually attributed: 20 * 70 / 100 = 14.
	if !d.Score.Equal(decimal.NewFromInt(14)) {
		t.Errorf("score = %s, want 14", d.Score)
	}
}

// SPEC.md §7: under 40% coverage, mark the score low-confidence.
func TestLowCoverageIsMarkedLowConfidence(t *testing.T) {
	s := New(testConfig(t))
	cfg := testConfig(t)

	low, _ := s.ScoreDirection(result(
		path("TMixer", "mixer", "labelled", 0.3),
		path("TUnknown", "", "hop_limit", 0.7),
	))
	res := s.Combine("tron", "TOrigin", nil, low, false, false, 1)
	if !res.LowConfidence {
		t.Errorf("coverage %s is below the %v threshold but was not marked low-confidence",
			res.Coverage, cfg.Weights.Coverage.LowConfidenceThreshold)
	}

	high, _ := s.ScoreDirection(result(
		path("TMixer", "mixer", "labelled", 0.9),
		path("TUnknown", "", "hop_limit", 0.1),
	))
	res = s.Combine("tron", "TOrigin", nil, high, false, false, 1)
	if res.LowConfidence {
		t.Errorf("coverage %s is above the threshold but was marked low-confidence", res.Coverage)
	}
}

// SPEC.md §7: any direct sanctions hit is reported High regardless of the
// computed score.
func TestDirectSanctionsHitForcesHigh(t *testing.T) {
	s := New(testConfig(t))

	// An otherwise clean profile.
	clean, _ := s.ScoreDirection(result(path("TExchange", "exchange", "labelled", 1.0)))

	res := s.Combine("tron", "TOrigin", nil, clean, false, false, 1)
	if res.Band != "low" {
		t.Fatalf("without a direct hit the band is %q, want low", res.Band)
	}

	res = s.Combine("tron", "TOrigin", nil, clean, true, false, 1)
	if res.Band != "high" {
		t.Errorf("a direct sanctions hit must report High regardless of score, got %q", res.Band)
	}
	if !res.SanctionsOverride {
		t.Error("the override must be recorded, or the band will not match the number beside it")
	}
}

// Indirect sanctions exposure is not a direct hit. Wiring the override to
// indirect exposure would send almost every long-lived address to High and
// make the band meaningless. This mirrors the competitor result in
// testdata/external, where 0.2% sanctions exposure still scores Low.
func TestIndirectSanctionsExposureDoesNotForceHigh(t *testing.T) {
	s := New(testConfig(t))

	d, err := s.ScoreDirection(result(
		path("TSanctioned", "sanctions", "labelled", 0.002),
		path("TExchange", "exchange", "labelled", 0.998),
	))
	if err != nil {
		t.Fatal(err)
	}
	res := s.Combine("tron", "TOrigin", nil, d, false, false, 1)

	if res.SanctionsOverride {
		t.Error("indirect exposure must not trigger the direct-hit override")
	}
	if res.Band != "low" {
		t.Errorf("band = %q, want low: 0.2%% indirect sanctions exposure scores %s",
			res.Band, res.Score)
	}
}

// The worse direction sets the score. Averaging would let clean inbound
// history dilute outbound exposure to a mixer, which is the case a screening
// tool exists to surface.
func TestWorseDirectionSetsTheScore(t *testing.T) {
	s := New(testConfig(t))

	clean, _ := s.ScoreDirection(result(path("TExchange", "exchange", "labelled", 1.0)))
	clean.Direction = graph.Inbound

	dirty, _ := s.ScoreDirection(result(path("TMixer", "mixer", "labelled", 1.0)))
	dirty.Direction = graph.Outbound

	res := s.Combine("tron", "TOrigin", clean, dirty, false, false, 1)
	if !res.Score.Equal(dirty.Score) {
		t.Errorf("combined score = %s, want the worse direction's %s", res.Score, dirty.Score)
	}
	if res.Band != "high" {
		t.Errorf("band = %q, want high (mixer weight 70)", res.Band)
	}
}

// SPEC.md §6.4: unverified abuse reports alone must not reach a high band.
func TestAbuseCapLimitsTheBand(t *testing.T) {
	s := New(testConfig(t))

	d, _ := s.ScoreDirection(result(path("TScam", "scam", "labelled", 1.0)))
	uncapped := s.Combine("tron", "TOrigin", nil, d, false, false, 1)
	if uncapped.Band != "high" {
		t.Fatalf("scam at weight 70 should band high, got %q", uncapped.Band)
	}

	capped := s.Combine("tron", "TOrigin", nil, d, false, true, 1)
	if capped.Band != "medium" {
		t.Errorf("band = %q, want medium when the only evidence is a lone abuse report", capped.Band)
	}
	if !capped.BandCappedByAbuseRule {
		t.Error("the cap must be recorded")
	}
}

// A sanctions hit outranks the abuse cap.
func TestSanctionsOverrideOutranksAbuseCap(t *testing.T) {
	s := New(testConfig(t))
	d, _ := s.ScoreDirection(result(path("TScam", "scam", "labelled", 1.0)))

	res := s.Combine("tron", "TOrigin", nil, d, true, true, 1)
	if res.Band != "high" {
		t.Errorf("band = %q, want high: a direct sanctions hit outranks the abuse cap", res.Band)
	}
}

// SPEC.md §7: dust carries near-zero weight and must never meaningfully move
// the score. This is the synthesised version of the dusting profile observed
// on the competitor's test address.
func TestDustBarelyMovesTheScore(t *testing.T) {
	s := New(testConfig(t))

	cleanOnly, _ := s.ScoreDirection(result(
		path("TExchange", "exchange", "labelled", 1.0),
	))

	// The same profile plus a heavy dusting campaign.
	withDust, _ := s.ScoreDirection(result(
		path("TExchange", "exchange", "labelled", 1.0),
		path("TDuster1", "dust", "dust", 0.0001),
		path("TDuster2", "dust", "dust", 0.0001),
		path("TDuster3", "dust", "dust", 0.0001),
	))

	delta := withDust.Score.Sub(cleanOnly.Score).Abs()
	if delta.GreaterThan(decimal.NewFromFloat(0.5)) {
		t.Errorf("dust moved the score by %s; SPEC.md §7 requires it never "+
			"meaningfully move the score", delta)
	}
}

// Scoring may never invent a weight. A category outside the vocabulary is a
// bug upstream, and guessing would bury it.
func TestUnknownCategoryIsAnError(t *testing.T) {
	s := New(testConfig(t))
	_, err := s.ScoreDirection(result(path("TWhat", "bridge", "labelled", 1.0)))
	if err == nil {
		t.Fatal("a category outside the controlled vocabulary must fail scoring")
	}
}

func TestEmptyTraversalScoresZero(t *testing.T) {
	s := New(testConfig(t))
	d, err := s.ScoreDirection(result())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Score.IsZero() {
		t.Errorf("score = %s, want 0", d.Score)
	}
	if !d.Coverage.IsZero() {
		t.Errorf("coverage = %s, want 0", d.Coverage)
	}
}

// Phase 3 acceptance: changing a weight in YAML changes output with no code
// change.
func TestWeightChangeAltersScoreWithoutCodeChange(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"weights.yaml", "sources.yaml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "config", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	before, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := New(before).ScoreDirection(result(path("TGambling", "gambling", "labelled", 1.0)))
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "weights.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(gambling:\s*\{weight:\s*)25`)
	patched := re.ReplaceAllString(string(raw), "${1}80")
	if patched == string(raw) {
		t.Fatal("fixture did not patch; the gambling weight line changed shape")
	}
	if err := os.WriteFile(filepath.Join(dir, "weights.yaml"), []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}

	after, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := New(after).ScoreDirection(result(path("TGambling", "gambling", "labelled", 1.0)))
	if err != nil {
		t.Fatal(err)
	}

	if !baseline.Score.Equal(decimal.NewFromInt(25)) {
		t.Errorf("baseline score = %s, want 25", baseline.Score)
	}
	if !changed.Score.Equal(decimal.NewFromInt(80)) {
		t.Errorf("score after the weight change = %s, want 80", changed.Score)
	}
	if before.Version == after.Version {
		t.Error("config version did not change with the weight")
	}
}

// SPEC.md §2: identical input plus identical snapshot produces identical
// output.
func TestScoringIsDeterministic(t *testing.T) {
	s := New(testConfig(t))
	in := result(
		path("TMixer", "mixer", "labelled", 0.3),
		path("TExchange", "exchange", "labelled", 0.3),
		path("TGambling", "gambling", "labelled", 0.3),
		path("TUnknown", "", "hop_limit", 0.1),
	)

	first, err := s.ScoreDirection(in)
	if err != nil {
		t.Fatal(err)
	}
	firstRender := renderDirection(first)

	for i := 0; i < 20; i++ {
		got, err := s.ScoreDirection(in)
		if err != nil {
			t.Fatal(err)
		}
		if renderDirection(got) != firstRender {
			t.Fatalf("run %d differs:\n first: %s\n  got:  %s", i, firstRender, renderDirection(got))
		}
	}
}

func renderDirection(d *DirectionResult) string {
	out := "score=" + d.Score.String() + " coverage=" + d.Coverage.String() + "\n"
	for _, c := range d.Categories {
		out += "  " + c.Category + " pct=" + c.Pct.String() +
			" weight=" + c.Weight.String() + " contribution=" + c.Contribution.String() + "\n"
	}
	return out
}
