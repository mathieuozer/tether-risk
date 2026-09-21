// Package labels resolves addresses to entities and categories.
//
// SPEC.md §6 defines the resolution rules: highest confidence wins, ties
// broken by source priority order in config, sanctions labels always win
// regardless of confidence, and conflicting categories are recorded rather
// than silently merged.
package labels

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mozer/tether-risk/internal/config"
)

// Label is one claim about an address from one source.
type Label struct {
	ID         int64
	Chain      string
	Address    string
	Entity     string
	Category   string
	Confidence float64
	Source     string
	Evidence   map[string]any
}

// Resolution is the outcome of applying SPEC.md §6's rules to the set of
// labels an address carries.
type Resolution struct {
	Address  string
	Chain    string
	Entity   string
	Category string

	// Confidence of the winning label.
	Confidence float64
	Source     string

	// Competing holds every label that did not win. SPEC.md §6 requires that
	// conflicting categories are recorded rather than silently merged, so the
	// losers travel with the winner rather than being discarded.
	Competing []Label

	// Conflicted is true when a losing label asserted a *different category*,
	// not merely a lower confidence for the same one. Two sources agreeing on
	// "exchange" with different confidence is not a conflict; one saying
	// "exchange" and another "mixer" is, and a human should see it.
	Conflicted bool

	// CappedByAbuseRule records that SPEC.md §6.4's protection applied: an
	// address whose only evidence is unverified abuse reports must not reach a
	// high-risk band on that basis alone.
	CappedByAbuseRule bool

	// Labelled is false when the address carries no labels at all. Traversal
	// treats a labelled address as terminal (SPEC.md §7), so this distinction
	// drives the walk, not just the report.
	Labelled bool
}

// Resolver applies the §6 rules using the configured priorities.
type Resolver struct{ cfg *config.Config }

func NewResolver(cfg *config.Config) *Resolver { return &Resolver{cfg: cfg} }

// Resolve picks the winning label for one address.
//
// The ordering is total and deterministic. SPEC.md §2 requires identical input
// to produce identical output, and a resolution that depended on the order
// rows came back from the database would break that quietly, only for
// addresses that happen to carry several labels.
func (r *Resolver) Resolve(chainID, address string, in []Label) Resolution {
	res := Resolution{Chain: chainID, Address: address}
	if len(in) == 0 {
		return res
	}
	res.Labelled = true

	labels := append([]Label(nil), in...)
	sort.SliceStable(labels, func(i, j int) bool {
		a, b := labels[i], labels[j]

		// SPEC.md §6: sanctions labels always win regardless of confidence.
		// This is not a tie-break; it outranks confidence entirely. A 0.5
		// sanctions hit must beat a 0.9 exchange label, because being wrong
		// about an exchange costs a false positive while being wrong about a
		// sanctions hit costs a missed one.
		aWins, bWins := r.cfg.AlwaysWins(a.Category), r.cfg.AlwaysWins(b.Category)
		if aWins != bWins {
			return aWins
		}

		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}

		// Ties broken by source priority order from config.
		pa, pb := r.cfg.SourcePriority(a.Source), r.cfg.SourcePriority(b.Source)
		if pa != pb {
			return pa < pb
		}

		// Final tie-breaks so the ordering is total rather than merely
		// mostly-defined. Without these, two labels identical in confidence
		// and source would resolve by input order.
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.Entity != b.Entity {
			return a.Entity < b.Entity
		}
		return a.ID < b.ID
	})

	winner := labels[0]
	res.Entity = winner.Entity
	res.Category = winner.Category
	res.Confidence = winner.Confidence
	res.Source = winner.Source
	res.Competing = labels[1:]

	for _, l := range res.Competing {
		if l.Category != winner.Category {
			res.Conflicted = true
			break
		}
	}

	r.applyAbuseCap(&res, labels)
	return res
}

// applyAbuseCap enforces SPEC.md §6.4: never let a single abuse report alone
// push an address into a high-risk band.
//
// Abuse feeds are unverified user submissions. One report is an accusation,
// not evidence. The rule only bites when abuse reports are the *only* thing
// saying an address is risky — a sanctions listing or a corroborating
// higher-confidence source lifts the cap, because then the abuse report is no
// longer carrying the conclusion on its own.
func (r *Resolver) applyAbuseCap(res *Resolution, all []Label) {
	rules := r.cfg.Weights.Labels.AbuseReports
	if len(rules.Sources) == 0 || rules.MinIndependentReports <= 0 {
		return
	}

	isAbuse := make(map[string]bool, len(rules.Sources))
	for _, s := range rules.Sources {
		isAbuse[s] = true
	}

	// If the winner is not itself from an abuse feed, nothing to cap.
	if !isAbuse[res.Source] {
		return
	}

	// Count distinct abuse sources that independently flagged this address.
	// Distinct sources, not distinct reports: ten reports on one platform are
	// one platform's opinion.
	distinct := map[string]bool{}
	for _, l := range all {
		if isAbuse[l.Source] {
			distinct[l.Source] = true
		}
	}

	// Any non-abuse corroboration means the conclusion is not resting on
	// unverified reports alone.
	for _, l := range all {
		if !isAbuse[l.Source] {
			return
		}
	}

	if len(distinct) < rules.MinIndependentReports {
		res.CappedByAbuseRule = true
	}
}

// BandCap returns the highest band this resolution may reach, and whether a
// cap applies at all.
func (r *Resolver) BandCap(res Resolution) (string, bool) {
	if res.CappedByAbuseRule {
		return r.cfg.Weights.Labels.AbuseReports.CappedBand, true
	}
	return "", false
}

// ConflictRecord is the row written to label_conflicts when categories
// disagree.
type ConflictRecord struct {
	Chain            string
	Address          string
	ResolvedCategory string
	ResolvedSource   string
	Competing        []CompetingClaim
}

type CompetingClaim struct {
	Source     string  `json:"source"`
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
	Entity     string  `json:"entity"`
}

// Conflict builds the record for a conflicted resolution, or nil when there is
// nothing to record.
func (res Resolution) Conflict() *ConflictRecord {
	if !res.Conflicted {
		return nil
	}
	claims := make([]CompetingClaim, 0, len(res.Competing))
	for _, l := range res.Competing {
		claims = append(claims, CompetingClaim{
			Source: l.Source, Category: l.Category,
			Confidence: l.Confidence, Entity: l.Entity,
		})
	}
	// Stable order so the same conflict produces the same stored row.
	sort.Slice(claims, func(i, j int) bool {
		if claims[i].Source != claims[j].Source {
			return claims[i].Source < claims[j].Source
		}
		return claims[i].Category < claims[j].Category
	})
	return &ConflictRecord{
		Chain:            res.Chain,
		Address:          res.Address,
		ResolvedCategory: res.Category,
		ResolvedSource:   res.Source,
		Competing:        claims,
	}
}

// ValidateCategory checks a category against the controlled vocabulary.
//
// SPEC.md §4 calls the category list a controlled vocabulary, and scoring may
// never invent a weight (see config.CategoryWeight). An ingester emitting an
// unknown category is a bug that must surface at ingest time, not silently
// contribute zero weight to every score thereafter.
func (r *Resolver) ValidateCategory(category string) error {
	if _, ok := r.cfg.CategoryWeight(category); !ok {
		return fmt.Errorf("category %q is not in the controlled vocabulary (%s)",
			category, strings.Join(r.cfg.Categories(), ", "))
	}
	return nil
}
