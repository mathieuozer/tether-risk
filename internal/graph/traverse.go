// Package graph performs breadth-first traversal over the aggregated edge
// table and applies haircut propagation.
//
// SPEC.md §7 defines the rules. Three of them carry most of the weight:
//
//   - Stop expanding at any labelled address. Without this, everything reaches
//     a major exchange within six hops and every score becomes noise.
//   - Cap fan-out and record when the cap was hit, so a truncated result says
//     so instead of looking complete.
//   - Exclude inbound dust from expansion entirely, because anyone can send an
//     unsolicited transfer to any address.
package graph

import (
	"context"
	"fmt"
	"sort"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/labels"
	"github.com/shopspring/decimal"
)

// Direction of travel. SPEC.md §1 requires inbound and outbound be computed
// separately: where funds came from and where they went are different
// questions with different answers.
type Direction string

const (
	Inbound  Direction = "inbound"
	Outbound Direction = "outbound"
)

// Neighbour is one aggregated edge from a node.
type Neighbour struct {
	Address       string
	USDValue      decimal.Decimal
	TransferCount uint64
	UnpricedCount uint64
}

// EdgeSource supplies neighbours. An interface so traversal is testable
// against fixtures without a database (SPEC.md §9).
type EdgeSource interface {
	Neighbours(ctx context.Context, chainID, address string, dir Direction) ([]Neighbour, error)
}

// HistorySource reports which addresses have had their own history fetched.
// An address that has not is known only through the transfers stored for
// other addresses, so its edges are a sample, not its history.
type HistorySource interface {
	Fetched(ctx context.Context, chainID string, addresses []string) (map[string]bool, error)
}

// LabelSource resolves labels in batch.
type LabelSource interface {
	Lookup(ctx context.Context, chainID string, addresses []string) (map[string]labels.Resolution, error)
}

// Terminal describes where a path stopped and why.
type Terminal struct {
	Address    string
	Entity     string
	Category   string
	Source     string
	Confidence float64

	// Reason is "labelled", "hop_limit", "fanout_cap", "dust", or "dead_end".
	// Only "labelled" and "dust" count as attributed; the rest are unknown
	// exposure and reduce coverage.
	Reason string
}

// Attributed reports whether this terminal contributes to attributed value.
func (t Terminal) Attributed() bool {
	return t.Reason == "labelled" || t.Reason == "dust"
}

// Path is one traced route with the arithmetic that produced its weight.
type Path struct {
	Hops   []string
	Shares []decimal.Decimal

	// Contribution is the product of the shares and the per-hop decay, as
	// SPEC.md §7 defines it.
	Contribution decimal.Decimal

	// USDValue is the contribution applied to the origin's total traced value.
	USDValue decimal.Decimal

	Decay    decimal.Decimal
	Terminal Terminal
}

// HopCount is the number of edges traversed.
func (p Path) HopCount() int { return len(p.Shares) }

// Result is a completed traversal in one direction.
type Result struct {
	Chain     string
	Address   string
	Direction Direction

	Paths []Path

	TotalTraced decimal.Decimal
	Attributed  decimal.Decimal

	NodesVisited    int
	EdgesConsidered int
	MaxHopReached   int

	// Honesty flags. SPEC.md §7 requires recording when the cap was hit so the
	// result is honest about being truncated.
	FanoutCapped    bool
	HopLimitReached bool

	// UnpricedTransfers counts transfers on edges that carried no USD value.
	//
	// Without this an address with real history but no prices loaded reports
	// "no traced value", which reads as "this address never moved funds". The
	// two are entirely different findings and the second one is wrong. An
	// unpriced edge contributes nothing to the split, so it is invisible in
	// every other number on the result.
	UnpricedTransfers uint64
}

// HasUnpricedData reports that edges existed but carried no USD value, so any
// emptiness in this result is a pricing gap rather than an absence of activity.
func (r Result) HasUnpricedData() bool {
	return r.UnpricedTransfers > 0 && !r.TotalTraced.IsPositive()
}

// Coverage is attributed over total traced value (SPEC.md §7).
func (r Result) Coverage() decimal.Decimal {
	if !r.TotalTraced.IsPositive() {
		return decimal.Zero
	}
	return r.Attributed.Div(r.TotalTraced)
}

// Traverser walks the edge graph.
type Traverser struct {
	edges   EdgeSource
	labels  LabelSource
	cfg     *config.Config
	history HistorySource // nil: every address counts as fetched
}

// WithHistory makes the traversal stop at addresses whose own history has
// not been fetched, recording them as dead ends instead of expanding them
// from the fragment of their edges other addresses revealed
// (docs/DECISIONS.md D30).
func (t *Traverser) WithHistory(h HistorySource) *Traverser {
	t.history = h
	return t
}

func New(edges EdgeSource, lbls LabelSource, cfg *config.Config) *Traverser {
	return &Traverser{edges: edges, labels: lbls, cfg: cfg}
}

// frontier is one node awaiting expansion.
type frontier struct {
	address string
	hops    []string
	shares  []decimal.Decimal
	// carried is the product of shares so far, before decay.
	carried decimal.Decimal
}

// Traverse walks from an address in one direction.
//
// Breadth-first, one level at a time, so labels can be looked up in batch per
// level rather than per node. SPEC.md §7 asks the "is this labelled?" question
// at every node, and doing it one row at a time dominates traversal on any
// real graph.
func (t *Traverser) Traverse(ctx context.Context, chainID, address string, dir Direction) (*Result, error) {
	tr := t.cfg.Weights.Traversal
	decay := decimal.NewFromFloat(tr.Decay)
	minContribution := decimal.NewFromFloat(tr.MinContribution)
	dustThreshold := decimal.NewFromFloat(t.cfg.Weights.Dust.USDThreshold)

	res := &Result{
		Chain: chainID, Address: address, Direction: dir,
		TotalTraced: decimal.Zero, Attributed: decimal.Zero,
	}

	// Cycle detection per traversal (SPEC.md §7). The origin counts as
	// visited: a path returning to it has told us nothing new.
	visited := map[string]bool{address: true}

	level := []frontier{{address: address, carried: decimal.NewFromInt(1)}}

	for hop := 0; hop < tr.MaxHops && len(level) > 0; hop++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		var next []frontier

		// Past the origin, expand only addresses whose own history is stored.
		// Expanding one that is not splits its value over the few edges other
		// addresses happened to reveal. That was a real bug: an intermediate
		// wallet whose only known inbound edges were address-poisoning dust
		// had 100% of its value classed as dust, which counts as attributed,
		// so coverage read 94.6% and confidence high for an address mostly
		// unknown (docs/DECISIONS.md D30). As a dead end it counts as unknown
		// and the screen queues it for fetching.
		if hop > 0 && t.history != nil {
			addrs := make([]string, len(level))
			for i, f := range level {
				addrs[i] = f.address
			}
			fetched, err := t.history.Fetched(ctx, chainID, addrs)
			if err != nil {
				return nil, fmt.Errorf("history: %w", err)
			}
			kept := level[:0]
			for _, f := range level {
				if fetched[f.address] {
					kept = append(kept, f)
					continue
				}
				res.Paths = append(res.Paths, Path{
					Hops: f.hops, Shares: f.shares,
					Contribution: f.carried.Mul(decayPow(decay, len(f.shares))),
					Decay:        decay,
					Terminal:     Terminal{Address: f.address, Reason: "dead_end"},
				})
			}
			level = kept
			if len(level) == 0 {
				break
			}
		}

		// Expand every node at this level, collecting the neighbours first so
		// the label lookup can be batched.
		type expansion struct {
			from       frontier
			neighbours []Neighbour
			capped     bool
		}
		var expansions []expansion
		addressSet := map[string]bool{}

		for _, f := range level {
			neighbours, err := t.edges.Neighbours(ctx, chainID, f.address, dir)
			if err != nil {
				return nil, fmt.Errorf("neighbours of %s: %w", f.address, err)
			}
			res.NodesVisited++
			res.EdgesConsidered += len(neighbours)

			// Total ordering, so which neighbours survive the cap does not
			// depend on the database's row order (docs/DECISIONS.md D6).
			sort.Slice(neighbours, func(i, j int) bool {
				if !neighbours[i].USDValue.Equal(neighbours[j].USDValue) {
					return neighbours[i].USDValue.GreaterThan(neighbours[j].USDValue)
				}
				return neighbours[i].Address < neighbours[j].Address
			})

			capped := false
			if len(neighbours) > tr.MaxNeighboursPerNode {
				neighbours = neighbours[:tr.MaxNeighboursPerNode]
				capped = true
				res.FanoutCapped = true
			}

			expansions = append(expansions, expansion{from: f, neighbours: neighbours, capped: capped})
			for _, n := range neighbours {
				addressSet[n.Address] = true
			}
		}

		addresses := sortedKeys(addressSet)
		resolved, err := t.labels.Lookup(ctx, chainID, addresses)
		if err != nil {
			return nil, fmt.Errorf("resolve labels: %w", err)
		}

		for _, ex := range expansions {
			// Denominator for the proportional split: the node's total flow in
			// this direction. Computed over the neighbours we kept, so a
			// capped node's shares still sum to one — the alternative silently
			// shrinks every downstream contribution by an unrecorded factor.
			total := decimal.Zero
			for _, n := range ex.neighbours {
				total = total.Add(n.USDValue)
				if !n.USDValue.IsPositive() {
					res.UnpricedTransfers += n.TransferCount
				}
			}

			// A node we expanded that leads nowhere — no neighbours, or none
			// carrying value. Its value has to be recorded as unattributed,
			// not dropped.
			//
			// Dropping it was a real bug: the value simply disappeared from
			// both numerator and denominator, so an address whose
			// counterparties were all unlabelled and un-ingested reported
			// "no traced value" and 100% coverage over whatever scraps
			// remained. That is precisely the hidden unknown exposure
			// SPEC.md §7 forbids — the failure looked like a clean result.
			//
			// A dead end is usually "we have not ingested this address yet"
			// rather than "this address never moved funds again", which makes
			// it unknown, and unknown counts against coverage.
			if !total.IsPositive() {
				if len(ex.from.shares) > 0 {
					res.Paths = append(res.Paths, Path{
						Hops:         ex.from.hops,
						Shares:       ex.from.shares,
						Contribution: ex.from.carried.Mul(decayPow(decay, len(ex.from.shares))),
						Decay:        decay,
						Terminal: Terminal{
							Address: ex.from.address, Reason: "dead_end",
						},
					})
				}
				continue
			}

			for _, n := range ex.neighbours {
				share := n.USDValue.Div(total)
				carried := ex.from.carried.Mul(share)

				hops := appendCopy(ex.from.hops, n.Address)
				shares := appendDecimal(ex.from.shares, share)

				// contribution = share(hop1) * ... * share(hopN) * decay^N
				contribution := carried.Mul(decayPow(decay, len(shares)))

				// --- dust ---
				// SPEC.md §7: classify inbound transfers below a USD threshold
				// as dust and exclude them from expansion entirely.
				//
				// Checked before the minimum-contribution filter, not after.
				// Dust is by definition tiny, so filtering first would discard
				// it silently and the report would show no dust row at all —
				// when what the spec asks for is that dust be *classified* and
				// carry near-zero weight. The distinction matters: "no dust"
				// and "dust worth two cents" are different findings, and the
				// second is the one that tells a reviewer the address is being
				// dusted.
				//
				// Edges aggregate many transfers, so the per-transfer rule is
				// applied to the edge's mean value. An edge of 96 transfers
				// worth $0.0002 each is dust; so is one of 1,000 transfers
				// totalling $50. Using the edge total instead would let a
				// dusting campaign escape the rule simply by being large.
				if dir == Inbound && n.TransferCount > 0 {
					mean := n.USDValue.Div(decimal.NewFromInt(int64(n.TransferCount)))
					if mean.LessThan(dustThreshold) {
						res.Paths = append(res.Paths, Path{
							Hops: hops, Shares: shares,
							Contribution: contribution,
							Decay:        decay,
							Terminal: Terminal{
								Address: n.Address, Category: "dust", Reason: "dust",
							},
						})
						continue
					}
				}

				if contribution.LessThan(minContribution) {
					// Too small to matter, and expanding it would multiply
					// path count without moving the score.
					continue
				}

				// --- labelled: terminal ---
				if r, ok := resolved[n.Address]; ok && r.Labelled {
					res.Paths = append(res.Paths, Path{
						Hops: hops, Shares: shares,
						Contribution: contribution,
						Decay:        decay,
						Terminal: Terminal{
							Address: n.Address, Entity: r.Entity, Category: r.Category,
							Source: r.Source, Confidence: r.Confidence, Reason: "labelled",
						},
					})
					continue
				}

				// --- unlabelled ---
				if visited[n.Address] {
					continue // cycle
				}

				lastHop := hop == tr.MaxHops-1
				if lastHop {
					// Ran out of hops before reaching anything identifiable.
					// This value is unknown, not absent, and it counts against
					// coverage.
					res.HopLimitReached = true
					res.Paths = append(res.Paths, Path{
						Hops: hops, Shares: shares,
						Contribution: contribution,
						Decay:        decay,
						Terminal: Terminal{
							Address: n.Address, Category: "", Reason: "hop_limit",
						},
					})
					continue
				}

				visited[n.Address] = true
				next = append(next, frontier{
					address: n.Address, hops: hops, shares: shares, carried: carried,
				})
			}

			if ex.capped {
				// Record the truncation as a path so it shows up in the
				// coverage arithmetic rather than only in a boolean flag.
				res.Paths = append(res.Paths, Path{
					Hops:         ex.from.hops,
					Shares:       ex.from.shares,
					Contribution: decimal.Zero,
					Decay:        decay,
					Terminal: Terminal{
						Address: ex.from.address, Reason: "fanout_cap",
					},
				})
			}
		}

		if len(next) > 0 {
			res.MaxHopReached = hop + 1
		}
		level = next
	}

	// Any node still in the frontier when the hop budget ran out is a dead end
	// we chose not to follow.
	for _, f := range level {
		res.HopLimitReached = true
		res.Paths = append(res.Paths, Path{
			Hops: f.hops, Shares: f.shares,
			Contribution: f.carried.Mul(decayPow(decay, len(f.shares))),
			Decay:        decay,
			Terminal:     Terminal{Address: f.address, Reason: "hop_limit"},
		})
	}

	t.finalise(res)
	return res, nil
}

// finalise computes traced and attributed totals and sorts paths.
func (t *Traverser) finalise(res *Result) {
	for _, p := range res.Paths {
		res.TotalTraced = res.TotalTraced.Add(p.Contribution)
		if p.Terminal.Attributed() {
			res.Attributed = res.Attributed.Add(p.Contribution)
		}
	}

	// Stable, total ordering: contribution descending, then by terminal and
	// path so equal contributions never reorder between runs (SPEC.md §2).
	sort.SliceStable(res.Paths, func(i, j int) bool {
		a, b := res.Paths[i], res.Paths[j]
		if !a.Contribution.Equal(b.Contribution) {
			return a.Contribution.GreaterThan(b.Contribution)
		}
		if a.Terminal.Address != b.Terminal.Address {
			return a.Terminal.Address < b.Terminal.Address
		}
		return fmt.Sprint(a.Hops) < fmt.Sprint(b.Hops)
	})
}

func decayPow(decay decimal.Decimal, n int) decimal.Decimal {
	out := decimal.NewFromInt(1)
	for i := 0; i < n; i++ {
		out = out.Mul(decay)
	}
	return out
}

func appendCopy(in []string, v string) []string {
	out := make([]string, len(in), len(in)+1)
	copy(out, in)
	return append(out, v)
}

func appendDecimal(in []decimal.Decimal, v decimal.Decimal) []decimal.Decimal {
	out := make([]decimal.Decimal, len(in), len(in)+1)
	copy(out, in)
	return append(out, v)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
