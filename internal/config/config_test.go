package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoConfigDir locates the real config/ directory so the tests exercise the
// configuration that actually ships, not a fixture that can drift from it.
func repoConfigDir(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "config")
}

func TestLoadRealConfig(t *testing.T) {
	c, err := Load(repoConfigDir(t))
	if err != nil {
		t.Fatalf("the shipped configuration must load and validate: %v", err)
	}
	if c.Version == "" {
		t.Fatal("version stamp is empty; results would not be reconstructible")
	}

	// SPEC.md §7 starting weights. These are the published methodology; a
	// change here is a deliberate act that must be reflected in
	// docs/METHODOLOGY.md, so the test pins them.
	want := map[string]float64{
		"sanctions": 100, "terrorist_financing": 100,
		"darknet": 90, "stolen_funds": 85,
		"mixer": 70, "scam": 70,
		"high_risk_exchange": 40, "gambling": 25,
		"unnamed_service": 15, "dust": 5,
		"exchange": 2, "dex": 5,
	}
	for cat, w := range want {
		got, ok := c.CategoryWeight(cat)
		if !ok {
			t.Errorf("category %q missing from weights.yaml", cat)
			continue
		}
		if got != w {
			t.Errorf("category %q weight = %v, SPEC.md §7 says %v", cat, got, w)
		}
	}
	if len(c.Weights.Categories) != len(want) {
		t.Errorf("category count = %d, want %d; a new category needs a weight justification in METHODOLOGY.md",
			len(c.Weights.Categories), len(want))
	}

	if c.Weights.Traversal.MaxHops != 5 {
		t.Errorf("max_hops = %d, SPEC.md §7 default is 5", c.Weights.Traversal.MaxHops)
	}
	if c.Weights.Traversal.Decay != 0.5 {
		t.Errorf("decay = %v, SPEC.md §7 default is 0.5", c.Weights.Traversal.Decay)
	}
	if c.Weights.Traversal.MaxNeighboursPerNode != 5000 {
		t.Errorf("max_neighbours_per_node = %d, SPEC.md §7 default is 5000",
			c.Weights.Traversal.MaxNeighboursPerNode)
	}
	if c.Weights.Dust.USDThreshold != 1.0 {
		t.Errorf("dust threshold = %v, SPEC.md §7 default is $1", c.Weights.Dust.USDThreshold)
	}
	if c.Weights.Coverage.LowConfidenceThreshold != 0.40 {
		t.Errorf("coverage threshold = %v, SPEC.md §7 says 40%%",
			c.Weights.Coverage.LowConfidenceThreshold)
	}
	if c.Weights.DerivedDeposit.MinTransfers != 3 {
		t.Errorf("derived_deposit.min_transfers = %d, SPEC.md §6 starts at N=3",
			c.Weights.DerivedDeposit.MinTransfers)
	}
	if c.Weights.DerivedDeposit.Confidence != 0.6 {
		t.Errorf("derived_deposit.confidence = %v, SPEC.md §6 says 0.6",
			c.Weights.DerivedDeposit.Confidence)
	}
}

// SPEC.md §6.2 directs us to flag anything prohibiting redistribution and stop
// rather than work around it. This test is the enforcement: it fails if a
// blocked source ever becomes ingestible without the reason being removed
// deliberately.
func TestBlockedSourcesStayBlocked(t *testing.T) {
	c, err := Load(repoConfigDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"etherscan", "bscscan", "tronscan"} {
		s, ok := c.Source(id)
		if !ok {
			t.Errorf("source %q must remain recorded even though it is not ingested, "+
				"so the decision stays visible", id)
			continue
		}
		if s.Ingestible() {
			t.Errorf("source %q is marked ingestible; SPEC.md §6.2 requires we stop "+
				"rather than work around its terms", id)
		}
		if s.BlockedReason == "" {
			t.Errorf("source %q is blocked without a recorded reason", id)
		}
	}
}

func TestEveryIngestibleSourceIsRanked(t *testing.T) {
	c, err := Load(repoConfigDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range c.Sources.Sources {
		if !s.Ingestible() {
			continue
		}
		if c.SourcePriority(s.ID) >= len(c.Weights.Labels.SourcePriority) {
			t.Errorf("ingestible source %q is unranked; label tie-breaks would be "+
				"non-deterministic", s.ID)
		}
	}
}

func TestSanctionsAlwaysWins(t *testing.T) {
	c, err := Load(repoConfigDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AlwaysWins("sanctions") {
		t.Error("sanctions must override confidence-based resolution (SPEC.md §6)")
	}
	if c.AlwaysWins("exchange") {
		t.Error("exchange must not override confidence-based resolution")
	}
}

func TestBandFor(t *testing.T) {
	c, err := Load(repoConfigDir(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		score float64
		want  string
	}{
		{0, "low"}, {15, "low"}, {29.99, "low"},
		{30, "medium"}, {45, "medium"}, {59.99, "medium"},
		{60, "high"}, {85, "high"}, {100, "high"},
	}
	for _, tc := range cases {
		if got := c.BandFor(tc.score); got != tc.want {
			t.Errorf("BandFor(%v) = %q, want %q", tc.score, got, tc.want)
		}
	}
}

// SPEC.md §2: identical input plus identical snapshot must produce identical
// output. That starts with the config version being a pure function of the
// config bytes.
func TestVersionIsDeterministic(t *testing.T) {
	dir := repoConfigDir(t)
	first, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		if again.Version != first.Version {
			t.Fatalf("version is not stable across loads: %q then %q",
				first.Version, again.Version)
		}
	}
}

func TestVersionChangesWithContent(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, filepath.Join(repoConfigDir(t), "weights.yaml"), filepath.Join(dir, "weights.yaml"))
	copyFile(t, filepath.Join(repoConfigDir(t), "sources.yaml"), filepath.Join(dir, "sources.yaml"))

	before, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Phase 3 acceptance: changing a weight in YAML must change output with no
	// code change. The version stamp moving is the first half of that.
	raw, err := os.ReadFile(filepath.Join(dir, "weights.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Match the weight regardless of the surrounding alignment, so reformatting
	// weights.yaml never silently turns this test into a no-op.
	re := regexp.MustCompile(`(gambling:\s*\{weight:\s*)25`)
	patched := re.ReplaceAllString(string(raw), "${1}26")
	if patched == string(raw) {
		t.Fatal("test fixture did not patch; the gambling weight line changed shape")
	}
	if err := os.WriteFile(filepath.Join(dir, "weights.yaml"), []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}

	after, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version == before.Version {
		t.Error("version stamp did not change after a weight change; results would be indistinguishable")
	}
	if w, _ := after.CategoryWeight("gambling"); w != 26 {
		t.Errorf("patched gambling weight = %v, want 26", w)
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	base := func(t *testing.T) *Config {
		t.Helper()
		c, err := Load(repoConfigDir(t))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	cases := []struct {
		name   string
		mutate func(*Config)
		expect string
	}{
		{
			name:   "band gap",
			mutate: func(c *Config) { c.Weights.Bands[1].Min = 35 },
			expect: "gap or overlap",
		},
		{
			name:   "bands do not reach 100",
			mutate: func(c *Config) { c.Weights.Bands[2].Max = 90 },
			expect: "must end at 100",
		},
		{
			name:   "decay out of range",
			mutate: func(c *Config) { c.Weights.Traversal.Decay = 1.5 },
			expect: "traversal.decay",
		},
		{
			name:   "zero hops",
			mutate: func(c *Config) { c.Weights.Traversal.MaxHops = 0 },
			expect: "traversal.max_hops",
		},
		{
			name:   "terminating at labelled disabled",
			mutate: func(c *Config) { c.Weights.Traversal.TerminateAtLabelled = false },
			expect: "every score noise",
		},
		{
			name:   "always_wins names an unknown category",
			mutate: func(c *Config) { c.Weights.Labels.AlwaysWins = []string{"nonsense"} },
			expect: "not a defined category",
		},
		{
			name:   "priority names an unknown source",
			mutate: func(c *Config) { c.Weights.Labels.SourcePriority = append(c.Weights.Labels.SourcePriority, "nope") },
			expect: "not a source in sources.yaml",
		},
		{
			name: "blocked source without a reason",
			mutate: func(c *Config) {
				for i := range c.Sources.Sources {
					if c.Sources.Sources[i].ID == "etherscan" {
						c.Sources.Sources[i].BlockedReason = ""
					}
				}
			},
			expect: "must record why",
		},
		{
			name:   "source without a checked date",
			mutate: func(c *Config) { c.Sources.Sources[0].Checked = "" },
			expect: "checked date is required",
		},
		{
			name: "category without a description",
			mutate: func(c *Config) {
				cat := c.Weights.Categories["mixer"]
				cat.Description = ""
				c.Weights.Categories["mixer"] = cat
			},
			expect: "description is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base(t)
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected validation to reject %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.expect)
			}
		})
	}
}

func TestUnknownCategoryHasNoWeight(t *testing.T) {
	c, err := Load(repoConfigDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.CategoryWeight("not_a_category"); ok {
		t.Error("an unknown category must not resolve to a weight; scoring may never invent one")
	}
}

func TestUnavailableChainsRecordWhy(t *testing.T) {
	c, err := Load(repoConfigDir(t))
	if err != nil {
		t.Fatal(err)
	}
	tron, ok := c.Chain("tron")
	if !ok || !tron.Available() {
		t.Error("tron must be available; it is the only chain with a live data path in v1")
	}
	for _, id := range []string{"ethereum", "bsc"} {
		ch, ok := c.Chain(id)
		if !ok {
			t.Errorf("chain %q must be declared even though it is unavailable", id)
			continue
		}
		if ch.Available() {
			t.Errorf("chain %q is marked available but has no credentials (docs/PLAN.md F1)", id)
		}
		if ch.UnavailableReason == "" {
			t.Errorf("chain %q is unavailable without a recorded reason", id)
		}
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
