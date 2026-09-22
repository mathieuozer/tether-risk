// Package scoring turns traced paths into a composite risk score.
//
// SPEC.md §7:
//
//	score = sum(category_pct * category_weight) / 100
//
// with bands Low 0-30, Medium 30-60, High 60-100, and any direct sanctions hit
// reported as High regardless of the computed score.
package scoring

import (
	"fmt"
	"sort"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/graph"
	"github.com/shopspring/decimal"
)

// CategoryShare is one row of the exposure breakdown.
type CategoryShare struct {
	Category string
	USDValue decimal.Decimal

	// Pct is the share of total traced value, 0-100.
	Pct decimal.Decimal

	Weight decimal.Decimal

	// Contribution is Pct * Weight / 100, the category's addition to the score.
	Contribution decimal.Decimal

	PathCount int
}

// DirectionResult is the scored breakdown for one direction.
type DirectionResult struct {
	Direction  graph.Direction
	Categories []CategoryShare

	// Unattributed is the share of traced value that reached no known entity.
	// SPEC.md §7 is explicit that this is shown, not hidden.
	UnattributedPct decimal.Decimal
	Coverage        decimal.Decimal

	Score decimal.Decimal

	TotalTraced decimal.Decimal
	Attributed  decimal.Decimal

	TopPaths []graph.Path

	// Connections aggregates attributed paths by the labelled address they
	// ended at, largest share first: who, not just which category.
	Connections []Connection

	// UnattributedReasons splits the unattributed share by why tracing
	// stopped, so "unknown" can be told apart from "not traced yet".
	UnattributedReasons []ReasonShare

	FanoutCapped    bool
	HopLimitReached bool
	NodesVisited    int

	// UnpricedTransfers is how many transfers carried no USD value. When this
	// is non-zero and nothing was traced, the result is empty because prices
	// are missing, not because the address is inactive.
	UnpricedTransfers uint64
	HasUnpricedData   bool
}

// Result is the full screening outcome.
type Result struct {
	Chain   string
	Address string

	Inbound  *DirectionResult
	Outbound *DirectionResult

	// Score is the higher of the two directions' scores.
	//
	// Not an average: averaging lets clean inbound history dilute outbound
	// exposure to a mixer, which is precisely the case a screening tool exists
	// to surface. The direction that looks worse is the one that matters.
	Score decimal.Decimal
	Band  string

	Coverage      decimal.Decimal
	LowConfidence bool

	// SanctionsOverride records that SPEC.md §7's rule fired: a direct
	// sanctions hit reports High regardless of the computed score.
	SanctionsOverride bool

	// BandCappedByAbuseRule records SPEC.md §6.4 limiting the band because the
	// only evidence was unverified abuse reports.
	BandCappedByAbuseRule bool

	ConfigVersion   string
	LabelSnapshotID int64

	// OwnLabel describes a label carried by the queried address itself, as
	// opposed to exposure reached through it.
	//
	// This must be surfaced, not merely used internally. An address that is
	// itself on a scam blacklist but has little traced value scores near zero,
	// and reporting only "LOW" for it would be actively misleading — the most
	// important fact known about the address would be the one fact omitted.
	// A direct hit is a finding in its own right, independent of any score.
	OwnLabel *OwnLabel

	// Activity is filled by the caller that holds the transfer store; nil
	// when it was not computed.
	Activity *Activity

	// Depth reports how much of the neighbourhood was stored when this was
	// scored. Nil when not computed.
	Depth *DepthStatus
}

// DepthStatus says how far stored history reached for this result, so a
// shallow answer is never presented as a finished one.
type DepthStatus struct {
	// Fetched reports that this screen fetched or refreshed the address.
	Fetched bool
	// FetchError is set when fetching failed and stored data was used.
	FetchError string
	// StillFetching reports that the address's own history did not finish
	// fetching within the screen's time budget; the background worker is
	// completing it.
	StillFetching bool
	// HistoryTruncated reports that the address has more history than the
	// per-address page limit fetches, so its own activity is partial.
	HistoryTruncated bool
	// Counterparties is how many counterparties are queued for tracing (the
	// most active, up to the enqueue cap); Traced is how many of them have
	// their own history stored.
	Counterparties int
	Traced         int
	// TotalCounterparties is every distinct counterparty, which can exceed
	// the enqueue cap.
	TotalCounterparties int

	// Frontier is where traversal stopped at an address with no stored
	// history beyond it. FrontierQueued of FrontierPending such addresses
	// were queued this time, most unattributed value first; FrontierEnded
	// were already fetched, so the trail genuinely ends there.
	FrontierPending int
	FrontierQueued  int
	FrontierEnded   int
}

// Complete reports whether every queued counterparty has been traced.
func (d *DepthStatus) Complete() bool {
	return d == nil || (!d.StillFetching && d.Traced >= d.Counterparties && d.FrontierPending == 0)
}

// OwnLabel is a label on the queried address itself.
type OwnLabel struct {
	Entity     string
	Category   string
	Source     string
	Confidence float64

	// Conflicted reports that sources disagreed about what this address is.
	Conflicted bool
}

// Connection is one identified counterparty, reached directly or through
// intermediate hops.
type Connection struct {
	Address    string
	Entity     string
	Category   string
	Source     string
	Confidence float64

	// Pct is the share of this direction's traced value, 0-100.
	Pct decimal.Decimal

	// MinHops is the shortest path length at which it was reached.
	MinHops int
	Paths   int
}

// ReasonShare is the part of the unattributed share with one stopping reason.
type ReasonShare struct {
	// Reason is graph.Terminal.Reason: "dead_end", "hop_limit", "fanout_cap",
	// or "unlabelled_category" for a label with no category.
	Reason string
	Pct    decimal.Decimal
	Paths  int
}

// Activity summarises an address's own stored history. It is independent of
// traversal: what the address itself did, before any attribution.
type Activity struct {
	InUSD, OutUSD                       decimal.Decimal
	InTransfers, OutTransfers           uint64
	InCounterparties, OutCounterparties uint64
	FirstSeen, LastSeen                 time.Time
	Assets                              []AssetFlow

	// UnpricedTransfers are transfers of tokens we do not value, typically
	// spam or airdrop tokens, in UnpricedTokens distinct contracts. Counted,
	// never valued: pricing them reopens docs/DECISIONS.md D18.
	UnpricedTransfers uint64
	UnpricedTokens    uint64
}

// AssetFlow is one asset's share of an address's activity.
type AssetFlow struct {
	Asset         string
	InUSD, OutUSD decimal.Decimal
	Transfers     uint64
}

// Scorer applies the configured weights.
type Scorer struct{ cfg *config.Config }

func New(cfg *config.Config) *Scorer { return &Scorer{cfg: cfg} }

// ScoreDirection converts one traversal into a scored breakdown.
func (s *Scorer) ScoreDirection(tr *graph.Result) (*DirectionResult, error) {
	out := &DirectionResult{
		Direction:         tr.Direction,
		TotalTraced:       tr.TotalTraced,
		Attributed:        tr.Attributed,
		Coverage:          tr.Coverage(),
		FanoutCapped:      tr.FanoutCapped,
		HopLimitReached:   tr.HopLimitReached,
		NodesVisited:      tr.NodesVisited,
		Score:             decimal.Zero,
		UnattributedPct:   decimal.Zero,
		UnpricedTransfers: tr.UnpricedTransfers,
		HasUnpricedData:   tr.HasUnpricedData(),
	}

	if !tr.TotalTraced.IsPositive() {
		return out, nil
	}

	hundred := decimal.NewFromInt(100)

	// Accumulate by category. Unattributed paths are gathered separately: they
	// are not a category, they are the absence of one, and folding them into
	// a bucket with a weight would turn "we do not know" into a risk claim.
	byCategory := map[string]decimal.Decimal{}
	pathCount := map[string]int{}
	unattributed := decimal.Zero

	for _, p := range tr.Paths {
		if !p.Terminal.Attributed() {
			unattributed = unattributed.Add(p.Contribution)
			continue
		}
		cat := p.Terminal.Category
		if cat == "" {
			unattributed = unattributed.Add(p.Contribution)
			continue
		}
		byCategory[cat] = byCategory[cat].Add(p.Contribution)
		pathCount[cat]++
	}

	// Sorted for determinism: Go map iteration order must never reach output
	// (docs/DECISIONS.md D6).
	cats := make([]string, 0, len(byCategory))
	for c := range byCategory {
		cats = append(cats, c)
	}
	sort.Strings(cats)

	for _, cat := range cats {
		value := byCategory[cat]
		pct := value.Div(tr.TotalTraced).Mul(hundred)

		w, ok := s.cfg.CategoryWeight(cat)
		if !ok {
			// Scoring may never invent a weight. A category reaching here that
			// is not in the vocabulary is a bug upstream, and guessing would
			// bury it.
			return nil, fmt.Errorf("category %q has no configured weight; "+
				"a label was written outside the controlled vocabulary", cat)
		}
		weight := decimal.NewFromFloat(w)
		contribution := pct.Mul(weight).Div(hundred)

		out.Categories = append(out.Categories, CategoryShare{
			Category:     cat,
			USDValue:     value,
			Pct:          pct,
			Weight:       weight,
			Contribution: contribution,
			PathCount:    pathCount[cat],
		})
		out.Score = out.Score.Add(contribution)
	}

	out.UnattributedPct = unattributed.Div(tr.TotalTraced).Mul(hundred)

	// Largest contributors first — these are what the explanation names.
	sort.SliceStable(out.Categories, func(i, j int) bool {
		if !out.Categories[i].Contribution.Equal(out.Categories[j].Contribution) {
			return out.Categories[i].Contribution.GreaterThan(out.Categories[j].Contribution)
		}
		return out.Categories[i].Category < out.Categories[j].Category
	})

	out.TopPaths = topAttributedPaths(tr.Paths, 10)
	out.Connections = connections(tr.Paths, tr.TotalTraced, 10)
	out.UnattributedReasons = unattributedReasons(tr.Paths, tr.TotalTraced)
	return out, nil
}

// connections aggregates attributed, labelled paths by terminal address.
// Dust is excluded: it is a pattern across many addresses, not a named
// counterparty, and is already reported as its own category.
func connections(paths []graph.Path, total decimal.Decimal, n int) []Connection {
	hundred := decimal.NewFromInt(100)
	by := map[string]*Connection{}
	for _, p := range paths {
		t := p.Terminal
		if t.Reason != "labelled" || t.Category == "" {
			continue
		}
		c, ok := by[t.Address]
		if !ok {
			c = &Connection{
				Address: t.Address, Entity: t.Entity, Category: t.Category,
				Source: t.Source, Confidence: t.Confidence,
				Pct: decimal.Zero, MinHops: p.HopCount(),
			}
			by[t.Address] = c
		}
		c.Pct = c.Pct.Add(p.Contribution.Div(total).Mul(hundred))
		c.Paths++
		if p.HopCount() < c.MinHops {
			c.MinHops = p.HopCount()
		}
	}

	out := make([]Connection, 0, len(by))
	for _, c := range by {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Pct.Equal(out[j].Pct) {
			return out[i].Pct.GreaterThan(out[j].Pct)
		}
		return out[i].Address < out[j].Address
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// unattributedReasons splits the unattributed share by stopping reason, in a
// fixed order so output is deterministic.
func unattributedReasons(paths []graph.Path, total decimal.Decimal) []ReasonShare {
	hundred := decimal.NewFromInt(100)
	by := map[string]*ReasonShare{}
	for _, p := range paths {
		reason := p.Terminal.Reason
		switch {
		case p.Terminal.Attributed() && p.Terminal.Category != "":
			continue
		case p.Terminal.Attributed():
			reason = "unlabelled_category"
		}
		r, ok := by[reason]
		if !ok {
			r = &ReasonShare{Reason: reason, Pct: decimal.Zero}
			by[reason] = r
		}
		r.Pct = r.Pct.Add(p.Contribution.Div(total).Mul(hundred))
		r.Paths++
	}

	var out []ReasonShare
	for _, reason := range []string{"dead_end", "hop_limit", "fanout_cap", "unlabelled_category"} {
		if r, ok := by[reason]; ok {
			out = append(out, *r)
			delete(by, reason)
		}
	}
	rest := make([]string, 0, len(by))
	for reason := range by {
		rest = append(rest, reason)
	}
	sort.Strings(rest)
	for _, reason := range rest {
		out = append(out, *by[reason])
	}
	return out
}

// Combine produces the overall result from both directions.
//
// `directHit` reports whether the queried address itself carries a sanctions
// label. SPEC.md §7's override is about a *direct* hit; wiring it to indirect
// exposure would send almost every long-lived address to High and make the
// band meaningless.
func (s *Scorer) Combine(chainID, address string, in, outb *DirectionResult,
	directHit bool, abuseCapped bool, snapshotID int64) *Result {

	res := &Result{
		Chain: chainID, Address: address,
		Inbound: in, Outbound: outb,
		ConfigVersion:   s.cfg.Version,
		LabelSnapshotID: snapshotID,
		Score:           decimal.Zero,
		Coverage:        decimal.Zero,
	}

	// The worse direction sets the score.
	traced := decimal.Zero
	attributed := decimal.Zero
	for _, d := range []*DirectionResult{in, outb} {
		if d == nil {
			continue
		}
		if d.Score.GreaterThan(res.Score) {
			res.Score = d.Score
		}
		traced = traced.Add(d.TotalTraced)
		attributed = attributed.Add(d.Attributed)
	}

	if traced.IsPositive() {
		res.Coverage = attributed.Div(traced)
	}

	// SPEC.md §7: under 40% coverage the score is marked low-confidence and
	// the report must say so prominently.
	threshold := decimal.NewFromFloat(s.cfg.Weights.Coverage.LowConfidenceThreshold)
	res.LowConfidence = res.Coverage.LessThan(threshold)

	res.Band = s.cfg.BandFor(res.Score.InexactFloat64())

	// SPEC.md §6.4: unverified abuse reports alone must not reach a high band.
	// Applied before the sanctions override, which outranks it.
	if abuseCapped {
		capped := s.cfg.Weights.Labels.AbuseReports.CappedBand
		if capped != "" && bandRank(s.cfg, res.Band) > bandRank(s.cfg, capped) {
			res.Band = capped
			res.BandCappedByAbuseRule = true
		}
	}

	if directHit {
		res.Band = s.cfg.Weights.SanctionsOverrideBand
		res.SanctionsOverride = true
	}

	return res
}

// bandRank orders bands by their lower bound.
func bandRank(cfg *config.Config, name string) int {
	bands := append([]config.Band(nil), cfg.Weights.Bands...)
	sort.Slice(bands, func(i, j int) bool { return bands[i].Min < bands[j].Min })
	for i, b := range bands {
		if b.Name == name {
			return i
		}
	}
	return -1
}

// topAttributedPaths returns the highest-contributing attributed paths.
//
// Only attributed paths: SPEC.md §8 asks for the top contributing paths in
// plain language, and "value went somewhere we could not identify" is reported
// through the coverage figure, not as a named path.
func topAttributedPaths(paths []graph.Path, n int) []graph.Path {
	var attributed []graph.Path
	for _, p := range paths {
		if p.Terminal.Attributed() && p.Terminal.Category != "dust" {
			attributed = append(attributed, p)
		}
	}
	sort.SliceStable(attributed, func(i, j int) bool {
		if !attributed[i].Contribution.Equal(attributed[j].Contribution) {
			return attributed[i].Contribution.GreaterThan(attributed[j].Contribution)
		}
		return attributed[i].Terminal.Address < attributed[j].Terminal.Address
	})
	if len(attributed) > n {
		attributed = attributed[:n]
	}
	return attributed
}
