package labels

import (
	"path/filepath"
	"testing"

	"github.com/mozer/tether-risk/internal/config"
)

func testResolver(t *testing.T) *Resolver {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	return NewResolver(cfg)
}

func lbl(source, category string, conf float64) Label {
	return Label{
		Chain: "tron", Address: "TAlice",
		Entity: source + "-entity", Category: category,
		Confidence: conf, Source: source,
	}
}

func TestHighestConfidenceWins(t *testing.T) {
	r := testResolver(t)
	res := r.Resolve("tron", "TAlice", []Label{
		lbl("dune", "exchange", 0.8),
		lbl("curated", "gambling", 0.95),
		lbl("scamsniffer", "scam", 0.5),
	})
	if res.Category != "gambling" {
		t.Errorf("category = %q, want gambling (highest confidence)", res.Category)
	}
	if res.Source != "curated" {
		t.Errorf("source = %q, want curated", res.Source)
	}
	if len(res.Competing) != 2 {
		t.Errorf("competing = %d, want 2; losers must be retained", len(res.Competing))
	}
}

// SPEC.md §6: sanctions labels always win regardless of confidence. Being
// wrong about an exchange costs a false positive; being wrong about a
// sanctions hit costs a missed one.
func TestSanctionsWinsRegardlessOfConfidence(t *testing.T) {
	r := testResolver(t)

	res := r.Resolve("tron", "TAlice", []Label{
		lbl("dune", "exchange", 0.99),
		lbl("ofac", "sanctions", 0.30), // deliberately low
	})
	if res.Category != "sanctions" {
		t.Errorf("category = %q, want sanctions despite lower confidence", res.Category)
	}

	// Terrorist financing carries the same override.
	res = r.Resolve("tron", "TAlice", []Label{
		lbl("dune", "exchange", 0.99),
		lbl("un", "terrorist_financing", 0.10),
	})
	if res.Category != "terrorist_financing" {
		t.Errorf("category = %q, want terrorist_financing", res.Category)
	}
}

func TestTiesBrokenBySourcePriority(t *testing.T) {
	r := testResolver(t)
	// Equal confidence; ofac outranks dune in weights.yaml source_priority.
	res := r.Resolve("tron", "TAlice", []Label{
		lbl("dune", "gambling", 0.8),
		lbl("ofac", "exchange", 0.8),
	})
	if res.Source != "ofac" {
		t.Errorf("source = %q, want ofac (higher priority on a tie)", res.Source)
	}
}

// SPEC.md §2: identical input must produce identical output. Resolution that
// depended on input order would break that quietly, and only for addresses
// carrying several labels.
func TestResolutionIsOrderIndependent(t *testing.T) {
	r := testResolver(t)
	in := []Label{
		lbl("dune", "exchange", 0.8),
		lbl("curated", "mixer", 0.8),
		lbl("cryptoscamdb", "scam", 0.5),
		lbl("ofac", "sanctions", 0.5),
	}

	want := r.Resolve("tron", "TAlice", in)

	// Every rotation must resolve identically.
	for shift := 1; shift < len(in); shift++ {
		rotated := append(append([]Label{}, in[shift:]...), in[:shift]...)
		got := r.Resolve("tron", "TAlice", rotated)
		if got.Category != want.Category || got.Source != want.Source {
			t.Errorf("rotation %d resolved to %s/%s, want %s/%s",
				shift, got.Source, got.Category, want.Source, want.Category)
		}
	}

	// And reversed.
	rev := make([]Label, len(in))
	for i, l := range in {
		rev[len(in)-1-i] = l
	}
	if got := r.Resolve("tron", "TAlice", rev); got.Category != want.Category {
		t.Errorf("reversed resolved to %s, want %s", got.Category, want.Category)
	}
}

// SPEC.md §6: never silently merge conflicting categories — record the
// conflict.
func TestConflictingCategoriesAreRecorded(t *testing.T) {
	r := testResolver(t)

	res := r.Resolve("tron", "TAlice", []Label{
		lbl("dune", "exchange", 0.8),
		lbl("curated", "mixer", 0.95),
	})
	if !res.Conflicted {
		t.Fatal("exchange vs mixer must be recorded as a conflict")
	}
	c := res.Conflict()
	if c == nil {
		t.Fatal("Conflict() returned nil for a conflicted resolution")
	}
	if c.ResolvedCategory != "mixer" {
		t.Errorf("resolved category = %q, want mixer", c.ResolvedCategory)
	}
	if len(c.Competing) != 1 || c.Competing[0].Category != "exchange" {
		t.Errorf("competing claims not recorded correctly: %+v", c.Competing)
	}
}

// Two sources agreeing on a category with different confidence is not a
// conflict. Recording it as one would bury real disagreements in noise.
func TestSameCategoryIsNotAConflict(t *testing.T) {
	r := testResolver(t)
	res := r.Resolve("tron", "TAlice", []Label{
		lbl("dune", "exchange", 0.8),
		lbl("curated", "exchange", 0.95),
	})
	if res.Conflicted {
		t.Error("agreement on category must not be recorded as a conflict")
	}
	if res.Conflict() != nil {
		t.Error("Conflict() must return nil when categories agree")
	}
}

// SPEC.md §6.4: never let a single abuse report alone push an address into a
// high-risk band.
func TestSingleAbuseReportIsCapped(t *testing.T) {
	r := testResolver(t)

	res := r.Resolve("tron", "TAlice", []Label{
		lbl("chainabuse", "scam", 0.5),
	})
	if !res.CappedByAbuseRule {
		t.Error("a lone abuse report must be capped")
	}
	band, capped := r.BandCap(res)
	if !capped || band != "medium" {
		t.Errorf("band cap = %q/%v, want medium/true", band, capped)
	}
}

func TestTwoIndependentAbuseSourcesLiftTheCap(t *testing.T) {
	r := testResolver(t)
	res := r.Resolve("tron", "TAlice", []Label{
		lbl("chainabuse", "scam", 0.5),
		lbl("scamsniffer", "scam", 0.5),
	})
	if res.CappedByAbuseRule {
		t.Error("two independent abuse sources meet the threshold; cap must lift")
	}
}

// Corroboration from a non-abuse source means the conclusion is no longer
// resting on unverified reports alone.
func TestCorroborationLiftsTheAbuseCap(t *testing.T) {
	r := testResolver(t)
	res := r.Resolve("tron", "TAlice", []Label{
		lbl("chainabuse", "scam", 0.5),
		lbl("curated", "scam", 0.4), // lower confidence, but not an abuse feed
	})
	if res.CappedByAbuseRule {
		t.Error("a non-abuse corroborating source must lift the cap")
	}
}

// A sanctions hit must never be capped by the abuse rule.
func TestSanctionsIsNeverCapped(t *testing.T) {
	r := testResolver(t)
	res := r.Resolve("tron", "TAlice", []Label{
		lbl("chainabuse", "scam", 0.9),
		lbl("ofac", "sanctions", 0.5),
	})
	if res.Category != "sanctions" {
		t.Fatalf("category = %q, want sanctions", res.Category)
	}
	if res.CappedByAbuseRule {
		t.Error("a sanctions resolution must never be capped by the abuse rule")
	}
}

func TestUnlabelledAddress(t *testing.T) {
	r := testResolver(t)
	res := r.Resolve("tron", "TAlice", nil)
	if res.Labelled {
		t.Error("an address with no labels must not report as labelled")
	}
	if res.Category != "" {
		t.Errorf("category = %q, want empty", res.Category)
	}
}

// An ingester emitting a category outside the controlled vocabulary is a bug.
// It must fail at ingest time rather than silently contribute zero weight to
// every score thereafter.
func TestValidateCategory(t *testing.T) {
	r := testResolver(t)
	for _, good := range []string{"sanctions", "mixer", "exchange", "dust"} {
		if err := r.ValidateCategory(good); err != nil {
			t.Errorf("ValidateCategory(%q) rejected a valid category: %v", good, err)
		}
	}
	for _, bad := range []string{"bridge", "lending", "Token contract", ""} {
		if err := r.ValidateCategory(bad); err == nil {
			t.Errorf("ValidateCategory(%q) accepted a category outside the vocabulary", bad)
		}
	}
}
