package graph

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mozer/tether-risk/internal/config"
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

// A simple graph: the origin sends to a mixer and an exchange, 50/50 by value.
//
//	TOrigin --500--> TMixer    (labelled mixer)
//	TOrigin --500--> TExchange (labelled exchange)
func simpleGraph() (*fakeEdges, *fakeLabels) {
	edges := newFakeEdges().
		addEdge("TOrigin", "TMixer", 500, 5).
		addEdge("TOrigin", "TExchange", 500, 5)
	lbls := newFakeLabels().
		add("TMixer", "Some Mixer", "mixer", "curated", 0.95).
		add("TExchange", "Some Exchange", "exchange", "curated", 0.95)
	return edges, lbls
}

func TestTraverseSplitsValueProportionally(t *testing.T) {
	edges, lbls := simpleGraph()
	tr := New(edges, lbls, testConfig(t))

	res, err := tr.Traverse(context.Background(), "tron", "TOrigin", Outbound)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 2 {
		t.Fatalf("paths = %d, want 2", len(res.Paths))
	}

	// Each path: share 0.5, one hop, decay 0.5 -> contribution 0.25.
	for _, p := range res.Paths {
		if !p.Contribution.Equal(decimal.NewFromFloat(0.25)) {
			t.Errorf("path to %s: contribution = %s, want 0.25 (0.5 share x 0.5 decay)",
				p.Terminal.Address, p.Contribution)
		}
		if p.HopCount() != 1 {
			t.Errorf("path to %s: hops = %d, want 1", p.Terminal.Address, p.HopCount())
		}
	}

	// Both terminals are labelled, so coverage is complete.
	if !res.Coverage().Equal(decimal.NewFromInt(1)) {
		t.Errorf("coverage = %s, want 1 (both terminals labelled)", res.Coverage())
	}
}

// SPEC.md §7: stop expanding at any labelled address. "Without this rule
// everything reaches Binance in six hops and every score becomes noise."
func TestLabelledAddressesAreTerminal(t *testing.T) {
	// TOrigin -> TExchange -> TSanctioned. The exchange is labelled, so the
	// walk must stop there and never see the sanctioned address beyond it.
	edges := newFakeEdges().
		addEdge("TOrigin", "TExchange", 1000, 10).
		addEdge("TExchange", "TSanctioned", 1000, 10)
	lbls := newFakeLabels().
		add("TExchange", "Some Exchange", "exchange", "curated", 0.95).
		add("TSanctioned", "Bad Actor", "sanctions", "ofac", 1.0)

	tr := New(edges, lbls, testConfig(t))
	res, err := tr.Traverse(context.Background(), "tron", "TOrigin", Outbound)
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range res.Paths {
		if p.Terminal.Address == "TSanctioned" {
			t.Error("traversal continued past a labelled exchange and reached the " +
				"sanctioned address behind it; every score would become noise")
		}
	}
	if len(res.Paths) != 1 || res.Paths[0].Terminal.Category != "exchange" {
		t.Errorf("expected one path terminating at the exchange, got %d paths", len(res.Paths))
	}
}

// SPEC.md §7: inbound dust must never meaningfully move the score. Anyone can
// send an unsolicited transfer to any address.
func TestInboundDustIsClassifiedAndNotExpanded(t *testing.T) {
	// A dusting sender: 96 transfers worth $0.0002 each.
	edges := newFakeEdges().
		addEdge("TDuster", "TVictim", 0.02, 96).
		addEdge("TRealSender", "TVictim", 1000, 3).
		// Behind the duster sits a sanctioned address. If dust were expanded,
		// this would contaminate the victim's score — which is exactly the
		// attack the rule prevents.
		addEdge("TSanctioned", "TDuster", 5000, 10)

	lbls := newFakeLabels().
		add("TRealSender", "Some Exchange", "exchange", "curated", 0.95).
		add("TSanctioned", "Bad Actor", "sanctions", "ofac", 1.0)

	tr := New(edges, lbls, testConfig(t))
	res, err := tr.Traverse(context.Background(), "tron", "TVictim", Inbound)
	if err != nil {
		t.Fatal(err)
	}

	var sawDust, sawSanctions bool
	for _, p := range res.Paths {
		switch p.Terminal.Category {
		case "dust":
			sawDust = true
		case "sanctions":
			sawSanctions = true
		}
	}
	if !sawDust {
		t.Error("the dusting edge was not classified as dust")
	}
	if sawSanctions {
		t.Error("traversal expanded through dust and reached the sanctioned address " +
			"behind it; anyone could then reshape a stranger's score for the price " +
			"of 96 transactions")
	}
}

// An outbound transfer of any size is a deliberate act by the address owner,
// so it is never dust.
func TestOutboundSmallValueIsNotDust(t *testing.T) {
	edges := newFakeEdges().addEdge("TOrigin", "TMixer", 0.02, 96)
	lbls := newFakeLabels().add("TMixer", "Some Mixer", "mixer", "curated", 0.95)

	tr := New(edges, lbls, testConfig(t))
	res, err := tr.Traverse(context.Background(), "tron", "TOrigin", Outbound)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Paths {
		if p.Terminal.Category == "dust" {
			t.Error("an outbound transfer was classified as dust; sending is a " +
				"deliberate act regardless of size")
		}
	}
}

// SPEC.md §7: cycle detection on the visited set per traversal.
func TestCycleDetection(t *testing.T) {
	edges := newFakeEdges().
		addEdge("TA", "TB", 100, 1).
		addEdge("TB", "TC", 100, 1).
		addEdge("TC", "TA", 100, 1) // back to the origin

	tr := New(edges, newFakeLabels(), testConfig(t))

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := tr.Traverse(context.Background(), "tron", "TA", Outbound); err != nil {
			t.Errorf("traverse: %v", err)
		}
	}()

	select {
	case <-done:
	case <-timeout():
		t.Fatal("traversal did not terminate on a cyclic graph")
	}
}

// SPEC.md §7: guard against fan-out, and record when the cap was hit so the
// result is honest about being truncated.
func TestFanoutCapIsRecorded(t *testing.T) {
	edges := newFakeEdges()
	for i := 0; i < 20; i++ {
		edges.addEdge("TOrigin", fmt.Sprintf("TNeighbour%02d", i), float64(100-i), 1)
	}

	cfg := testConfig(t)
	cfg.Weights.Traversal.MaxNeighboursPerNode = 5 // tighten for the test

	tr := New(edges, newFakeLabels(), cfg)
	res, err := tr.Traverse(context.Background(), "tron", "TOrigin", Outbound)
	if err != nil {
		t.Fatal(err)
	}
	if !res.FanoutCapped {
		t.Error("the fan-out cap was hit but not recorded; the result would look complete")
	}
}

// The cap must keep the highest-value neighbours, not an arbitrary five.
// Which counterparties get traced cannot depend on database row order.
func TestFanoutCapKeepsHighestValue(t *testing.T) {
	edges := newFakeEdges()
	for i := 0; i < 20; i++ {
		edges.addEdge("TOrigin", fmt.Sprintf("TNeighbour%02d", i), float64(100-i), 1)
	}
	lbls := newFakeLabels()
	for i := 0; i < 20; i++ {
		lbls.add(fmt.Sprintf("TNeighbour%02d", i), "E", "exchange", "curated", 0.9)
	}

	cfg := testConfig(t)
	cfg.Weights.Traversal.MaxNeighboursPerNode = 3

	tr := New(edges, lbls, cfg)
	res, err := tr.Traverse(context.Background(), "tron", "TOrigin", Outbound)
	if err != nil {
		t.Fatal(err)
	}

	kept := map[string]bool{}
	for _, p := range res.Paths {
		if p.Terminal.Reason == "labelled" {
			kept[p.Terminal.Address] = true
		}
	}
	// Values descend from TNeighbour00 (100) to TNeighbour19 (81).
	for _, want := range []string{"TNeighbour00", "TNeighbour01", "TNeighbour02"} {
		if !kept[want] {
			t.Errorf("%s has the highest value but was dropped by the cap", want)
		}
	}
	if kept["TNeighbour19"] {
		t.Error("the lowest-value neighbour survived the cap")
	}
}

// SPEC.md §7: value that runs out of hops is unknown, not absent, and must
// count against coverage rather than quietly disappearing.
func TestHopLimitReducesCoverage(t *testing.T) {
	// A chain longer than the hop limit, with nothing labelled along it.
	edges := newFakeEdges()
	prev := "TOrigin"
	for i := 0; i < 10; i++ {
		next := fmt.Sprintf("THop%02d", i)
		edges.addEdge(prev, next, 1000, 5)
		prev = next
	}

	tr := New(edges, newFakeLabels(), testConfig(t))
	res, err := tr.Traverse(context.Background(), "tron", "TOrigin", Outbound)
	if err != nil {
		t.Fatal(err)
	}

	if !res.HopLimitReached {
		t.Error("the hop limit was reached but not recorded")
	}
	if res.Coverage().IsPositive() {
		t.Errorf("coverage = %s, want 0: nothing on this path was ever identified",
			res.Coverage())
	}
	if !res.TotalTraced.IsPositive() {
		t.Error("unattributed value must still be traced, or it vanishes from the denominator")
	}
}

// SPEC.md §2: identical input plus identical snapshot must produce identical
// output. This is the Phase 3 acceptance criterion.
func TestTraversalIsDeterministic(t *testing.T) {
	edges := newFakeEdges()
	// Deliberately include equal-value edges, which is where an unstable sort
	// would show up.
	edges.addEdge("TOrigin", "TEqualA", 100, 1).
		addEdge("TOrigin", "TEqualB", 100, 1).
		addEdge("TOrigin", "TEqualC", 100, 1).
		addEdge("TEqualA", "TDeep1", 50, 1).
		addEdge("TEqualB", "TDeep2", 50, 1)

	lbls := newFakeLabels().
		add("TEqualC", "C", "gambling", "curated", 0.9).
		add("TDeep1", "D1", "mixer", "curated", 0.9).
		add("TDeep2", "D2", "exchange", "curated", 0.9)

	cfg := testConfig(t)
	var first string

	for run := 0; run < 20; run++ {
		tr := New(edges, lbls, cfg)
		res, err := tr.Traverse(context.Background(), "tron", "TOrigin", Outbound)
		if err != nil {
			t.Fatal(err)
		}
		got := render(res)
		if run == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d differs from run 0:\n first: %s\n  got:  %s", run, first, got)
		}
	}
}

// render produces a stable textual form of a result, for determinism testing.
func render(res *Result) string {
	out := fmt.Sprintf("traced=%s attributed=%s capped=%v hoplimit=%v\n",
		res.TotalTraced, res.Attributed, res.FanoutCapped, res.HopLimitReached)
	for _, p := range res.Paths {
		out += fmt.Sprintf("  %v -> %s [%s/%s] contribution=%s shares=%v\n",
			p.Hops, p.Terminal.Address, p.Terminal.Category, p.Terminal.Reason,
			p.Contribution, p.Shares)
	}
	return out
}

// Contributions must fall off with distance. A mixer five hops away is not the
// same finding as a mixer one hop away, and the decay is what encodes that.
func TestDecayReducesDistantContributions(t *testing.T) {
	edges := newFakeEdges().
		addEdge("TOrigin", "TNear", 1000, 1).
		addEdge("TOrigin", "TMid", 1000, 1).
		addEdge("TMid", "TFar", 1000, 1)

	lbls := newFakeLabels().
		add("TNear", "Near Mixer", "mixer", "curated", 0.9).
		add("TFar", "Far Mixer", "mixer", "curated", 0.9)

	tr := New(edges, lbls, testConfig(t))
	res, err := tr.Traverse(context.Background(), "tron", "TOrigin", Outbound)
	if err != nil {
		t.Fatal(err)
	}

	var near, far decimal.Decimal
	for _, p := range res.Paths {
		switch p.Terminal.Address {
		case "TNear":
			near = p.Contribution
		case "TFar":
			far = p.Contribution
		}
	}
	if !near.IsPositive() || !far.IsPositive() {
		t.Fatalf("expected both terminals to be reached: near=%s far=%s", near, far)
	}
	if !near.GreaterThan(far) {
		t.Errorf("near contribution %s should exceed far %s; decay is not applied", near, far)
	}
}

func timeout() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		ctx, cancel := context.WithTimeout(context.Background(), timeoutDuration)
		defer cancel()
		<-ctx.Done()
	}()
	return ch
}

const timeoutDuration = 5 * time.Second

// A node expanded that leads nowhere must have its value recorded as
// unattributed, not dropped.
//
// This was a real bug: dead-end value vanished from both numerator and
// denominator, so an address whose counterparties were all unlabelled and
// un-ingested reported "no traced value" with 100% coverage over whatever
// scraps remained. The failure looked like a clean result, which is the worst
// possible shape for it to take.
func TestDeadEndsAreCountedAsUnattributed(t *testing.T) {
	// TOrigin sends to two unlabelled addresses about which we know nothing
	// further — the ordinary state of affairs before their history is ingested.
	edges := newFakeEdges().
		addEdge("TOrigin", "TUnknownA", 600, 3).
		addEdge("TOrigin", "TUnknownB", 400, 2)

	tr := New(edges, newFakeLabels(), testConfig(t))
	res, err := tr.Traverse(context.Background(), "tron", "TOrigin", Outbound)
	if err != nil {
		t.Fatal(err)
	}

	if !res.TotalTraced.IsPositive() {
		t.Fatal("dead-end value vanished; the result would claim there was nothing to trace")
	}
	if res.Coverage().IsPositive() {
		t.Errorf("coverage = %s, want 0: nothing here was ever identified", res.Coverage())
	}

	var deadEnds int
	for _, p := range res.Paths {
		if p.Terminal.Reason == "dead_end" {
			deadEnds++
		}
		if p.Terminal.Attributed() {
			t.Errorf("path to %s is marked attributed but nothing was labelled", p.Terminal.Address)
		}
	}
	if deadEnds != 2 {
		t.Errorf("dead-end paths = %d, want 2", deadEnds)
	}
}

// The mixed case: some counterparties identified, others dead ends. Coverage
// must reflect the proportion actually attributed.
func TestPartialCoverageWithDeadEnds(t *testing.T) {
	edges := newFakeEdges().
		addEdge("TOrigin", "TExchange", 500, 1).
		addEdge("TOrigin", "TUnknown", 500, 1)
	lbls := newFakeLabels().add("TExchange", "Some Exchange", "exchange", "curated", 0.95)

	tr := New(edges, lbls, testConfig(t))
	res, err := tr.Traverse(context.Background(), "tron", "TOrigin", Outbound)
	if err != nil {
		t.Fatal(err)
	}

	// Half the value reached a known entity, half did not.
	want := decimal.NewFromFloat(0.5)
	if !res.Coverage().Equal(want) {
		t.Errorf("coverage = %s, want %s", res.Coverage(), want)
	}
}
