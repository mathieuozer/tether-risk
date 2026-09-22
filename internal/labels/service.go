package labels

import (
	"context"
	"fmt"
	"sort"

	"github.com/mozer/tether-risk/internal/config"
)

// Behavioural service detection.
//
// An address transacting with hundreds of distinct counterparties, still
// paginating after a deep sample, is a service: an exchange, a payment
// processor, a desk, a bridge. Nothing else produces that shape.
//
// The behaviour proves *that* it is a service. It does not reveal *which*, and
// this package does not pretend otherwise. Matches are labelled
// `unnamed_service`, never `exchange` — SPEC.md §7 weights those 15 and 2
// respectively, because reaching a regulated exchange is close to reassuring
// while an unidentified service might be a no-KYC swapper. Claiming the wrong
// one of those is not a rounding error.
//
// This exists because no citable source for named TRON exchange hot wallets is
// available within the project's constraints: block-explorer terms forbid it
// (docs/DECISIONS.md F2), the Dune Spellbook holds no address data (D14), and
// exchanges' proof-of-reserves pages gate their address lists. Behaviour is
// what remains, and behaviour is at least independently reproducible.

// Sample is what a sampler observed about one address.
type Sample struct {
	// Transfers is how many transfers were examined.
	Transfers int
	// Counterparties is the number of distinct addresses seen opposite.
	Counterparties int
	// MorePages reports that the sample did not exhaust the address's history.
	MorePages bool
	// PagesFetched records the cost paid, for the evidence record.
	PagesFetched int
}

// Sampler retrieves an activity sample for an address.
//
// An interface so detection is testable without network access, and so a
// second chain can supply its own implementation.
type Sampler interface {
	Sample(ctx context.Context, address string, pages, pageSize int) (Sample, error)
}

// ServiceCandidate is one judged address, including the rejections.
//
// Rejections are returned rather than discarded so the thresholds can be
// tuned against real outcomes instead of guessed at.
type ServiceCandidate struct {
	Address string
	Sample  Sample

	Accepted bool
	Rejected string
}

// DetectServices judges candidate addresses and emits labels for those that
// qualify.
//
// Candidates are supplied by the caller rather than discovered here, because
// sampling costs an API call per page and the set worth spending that on
// depends on what has been ingested.
func DetectServices(ctx context.Context, sampler Sampler, cfg *config.Config,
	chainID string, candidates []string) ([]ServiceCandidate, []Label, error) {

	rules := cfg.Weights.DerivedService
	if !rules.Enabled {
		return nil, nil, nil
	}
	if rules.MinCounterparties <= 0 || rules.SamplePages <= 0 {
		return nil, nil, fmt.Errorf(
			"derived_service is enabled but its thresholds are unset; " +
				"an unbounded detector would label every address it saw")
	}

	// Deterministic order, so a run over the same candidates produces the same
	// labels in the same sequence (docs/DECISIONS.md D6).
	ordered := append([]string(nil), candidates...)
	sort.Strings(ordered)

	var (
		out     []Label
		judged  []ServiceCandidate
		skipped = map[string]bool{}
	)

	for _, addr := range ordered {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}
		if addr == "" || skipped[addr] {
			continue
		}
		skipped[addr] = true

		s, err := sampler.Sample(ctx, addr, rules.SamplePages, rules.PageSize)
		if err != nil {
			// One unreachable address must not abandon the run. It simply
			// goes unjudged, which costs coverage rather than correctness.
			judged = append(judged, ServiceCandidate{
				Address:  addr,
				Rejected: fmt.Sprintf("could not sample: %v", err),
			})
			continue
		}

		c := ServiceCandidate{Address: addr, Sample: s}

		switch {
		case s.Transfers < rules.MinTransfersSampled:
			c.Rejected = fmt.Sprintf("sampled %d transfers, need %d",
				s.Transfers, rules.MinTransfersSampled)

		case s.Counterparties < rules.MinCounterparties:
			c.Rejected = fmt.Sprintf("%d distinct counterparties, need %d",
				s.Counterparties, rules.MinCounterparties)

		case rules.RequireMorePages && !s.MorePages:
			// An address whose entire history fits in the sample is not a
			// service however diverse it looks. Small samples are trivially
			// diverse: a 39-transfer wallet measured 0.795 distinct-per-
			// transfer during calibration, higher than some real services.
			c.Rejected = "history exhausted within the sample, so this is not a high-volume address"

		default:
			c.Accepted = true
		}

		judged = append(judged, c)
		if !c.Accepted {
			continue
		}

		out = append(out, Label{
			Chain:   chainID,
			Address: addr,
			Entity:  "Unidentified high-volume service",
			// Never `exchange`. The behaviour does not identify the operator,
			// and claiming it did would understate risk by a factor of seven
			// in the weight table.
			Category:   "unnamed_service",
			Confidence: rules.Confidence,
			Source:     "derived:service",
			Evidence: map[string]any{
				"heuristic":               "high_volume_service",
				"transfers_sampled":       s.Transfers,
				"distinct_counterparties": s.Counterparties,
				"pages_fetched":           s.PagesFetched,
				"history_exhausted":       !s.MorePages,
				"thresholds": map[string]any{
					"min_transfers_sampled": rules.MinTransfersSampled,
					"min_counterparties":    rules.MinCounterparties,
					"require_more_pages":    rules.RequireMorePages,
				},
				"note": "Behavioural evidence that this address is a service. It does " +
					"not identify which service. A verified name from the curated " +
					"source outranks this label and should replace it.",
				"reproduce": fmt.Sprintf(
					"https://api.trongrid.io/v1/accounts/%s/transactions/trc20?limit=%d",
					addr, rules.PageSize),
			},
		})
	}

	return judged, out, nil
}

// AcceptedCount reports how many candidates qualified.
func AcceptedCount(candidates []ServiceCandidate) int {
	var n int
	for _, c := range candidates {
		if c.Accepted {
			n++
		}
	}
	return n
}

// StoredStats is an address's activity as stored, for addresses whose own
// history has been fetched.
type StoredStats struct {
	Address        string
	Transfers      uint64
	Counterparties uint64
	Truncated      bool
	ActiveDays     int // first to last stored transfer
}

// JudgeStoredServices decides service shape from stored history rather than
// a sample (docs/DECISIONS.md D30). Pure, so the rule is tested without a
// database or network.
func JudgeStoredServices(stats []StoredStats, cfg *config.Config, chainID string) ([]ServiceCandidate, []Label) {
	rules := cfg.Weights.DerivedService
	r := rules.Stored
	var judged []ServiceCandidate
	var out []Label
	for _, s := range stats {
		c := ServiceCandidate{Address: s.Address, Sample: Sample{Transfers: int(s.Transfers), Counterparties: int(s.Counterparties), MorePages: s.Truncated}}
		ratio := 0.0
		if s.Transfers > 0 {
			ratio = float64(s.Counterparties) / float64(s.Transfers)
		}
		switch {
		case int(s.Transfers) < r.MinTransfers:
			c.Rejected = fmt.Sprintf("%d stored transfers, need %d", s.Transfers, r.MinTransfers)
		case int(s.Counterparties) < r.MinCounterparties:
			c.Rejected = fmt.Sprintf("%d distinct counterparties, need %d", s.Counterparties, r.MinCounterparties)
		case ratio < r.MinCounterpartyRatio:
			c.Rejected = fmt.Sprintf("%.2f counterparties per transfer, need %.2f", ratio, r.MinCounterpartyRatio)
		case s.ActiveDays < r.MinActiveDays:
			c.Rejected = fmt.Sprintf("active %d days, need %d: broad but young is a transit hub more often than a service",
				s.ActiveDays, r.MinActiveDays)
		default:
			c.Accepted = true
		}
		judged = append(judged, c)
		if !c.Accepted {
			continue
		}
		out = append(out, Label{
			Chain: chainID, Address: s.Address,
			Entity:     "Unidentified high-volume service",
			Category:   "unnamed_service",
			Confidence: rules.Confidence,
			Source:     "derived:service",
			Evidence: map[string]any{
				"heuristic":               "high_volume_service_stored",
				"stored_transfers":        s.Transfers,
				"distinct_counterparties": s.Counterparties,
				"counterparty_ratio":      fmt.Sprintf("%.3f", ratio),
				"history_truncated":       s.Truncated,
				"active_days":             s.ActiveDays,
				"thresholds": map[string]any{
					"min_transfers":          r.MinTransfers,
					"min_counterparties":     r.MinCounterparties,
					"min_counterparty_ratio": r.MinCounterpartyRatio,
				},
				"note": "Judged from the address's fetched history. Behavioural evidence that this address " +
					"is a service; it does not identify which one.",
			},
		})
	}
	return judged, out
}
