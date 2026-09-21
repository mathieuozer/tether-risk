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

	FanoutCapped    bool
	HopLimitReached bool
	NodesVisited    int
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

// Scorer applies the configured weights.
type Scorer struct{ cfg *config.Config }

func New(cfg *config.Config) *Scorer { return &Scorer{cfg: cfg} }

// ScoreDirection converts one traversal into a scored breakdown.
func (s *Scorer) ScoreDirection(tr *graph.Result) (*DirectionResult, error) {
	out := &DirectionResult{
		Direction:       tr.Direction,
		TotalTraced:     tr.TotalTraced,
		Attributed:      tr.Attributed,
		Coverage:        tr.Coverage(),
		FanoutCapped:    tr.FanoutCapped,
		HopLimitReached: tr.HopLimitReached,
		NodesVisited:    tr.NodesVisited,
		Score:           decimal.Zero,
		UnattributedPct: decimal.Zero,
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
	return out, nil
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
