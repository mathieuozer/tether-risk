package screen

import (
	"context"
	"time"
)

// First-ring fetching (docs/DECISIONS.md D31).
//
// An unfetched counterparty is a dead end: its share of value is unknown
// until the worker fetches it, which a customer sees only on a rescreen.
// Coverage is decided mostly by the few largest counterparties, so the
// screen fetches those itself before answering, largest first, until its
// budget runs out. TronGrid serves about one request a second to a serial
// stream and penalises concurrency, so this is serial and bounded in time,
// not in count. Labelled counterparties are skipped: traversal stops at a
// label, so fetching one changes nothing.

// ringBudget is how long a screen spends on its first ring. Zero disables it.
const ringBudget = 20 * time.Second

// ringCandidates is how many of the largest counterparties are considered.
const ringCandidates = 20

// ringMinShare is the smallest share of the address's value, in percent, a
// counterparty must carry to be worth the wait. Measured over 20 wallets,
// fetching every unknown counterparty raised mean coverage from 12.0% to
// 15.4% for 20 s a screen, almost all of it from the few large ones.
const ringMinShare = 5.0

// ringPerAddress bounds one counterparty's fetch. An ordinary wallet takes a
// few requests; one that takes longer is too large to finish within the
// budget anyway, and waiting on it cost whole screens 20 s for nothing. The
// pages it did fetch are kept and the worker resumes it.
const ringPerAddress = 8 * time.Second

// WithRingBudget overrides the first-ring budget; zero disables it. The
// benchmark uses it to measure what the ring buys.
func (s *Service) WithRingBudget(d time.Duration) *Service {
	s.ringBudget = &d
	return s
}

func (s *Service) ring() time.Duration {
	if s.ringBudget != nil {
		return *s.ringBudget
	}
	return ringBudget
}

// fetchFirstRing fetches the largest unlabelled, unfetched direct
// counterparties of address, largest by value first, within the budget. It
// returns how many were fetched in full. Errors are not the screen's: an
// unfetched counterparty stays a dead end and is queued as before.
func (s *Service) fetchFirstRing(ctx context.Context, p Prefetcher, chainID, address string, snapshotID int64) int {
	budget := s.ring()
	if budget <= 0 {
		return 0
	}
	deadline := time.Now().Add(budget)

	rows, err := s.ch.QueryContext(ctx, `
		SELECT cp, sum(v) AS usd FROM (
			SELECT from_address AS cp, total_usd_value AS v FROM edges_by_to_current WHERE chain = ? AND to_address = ?
			UNION ALL
			SELECT to_address, total_usd_value FROM edges_current WHERE chain = ? AND from_address = ?)
		WHERE cp != ?
		GROUP BY cp ORDER BY usd DESC, cp LIMIT ?`, chainID, address, chainID, address, address, ringCandidates)
	if err != nil {
		return 0
	}
	var cands []string
	value := map[string]float64{}
	for rows.Next() {
		var a string
		var usd float64
		if rows.Scan(&a, &usd) == nil && usd > 0 {
			cands = append(cands, a)
			value[a] = usd
		}
	}
	rows.Close()
	if len(cands) == 0 {
		return 0
	}
	var total float64
	if err := s.ch.QueryRowContext(ctx, `
		SELECT sum(v) FROM (
			SELECT total_usd_value AS v FROM edges_by_to_current WHERE chain = ? AND to_address = ? AND from_address != ?
			UNION ALL
			SELECT total_usd_value FROM edges_current WHERE chain = ? AND from_address = ? AND to_address != ?)`,
		chainID, address, address, chainID, address, address).Scan(&total); err != nil || total <= 0 {
		return 0
	}

	fetched, err := pgHistory{pg: s.pg}.Fetched(ctx, chainID, cands)
	if err != nil {
		return 0
	}
	labelled, err := s.store.ForAddresses(ctx, snapshotID, chainID, cands)
	if err != nil {
		return 0
	}

	var n int
	for _, a := range cands {
		if value[a]*100/total < ringMinShare {
			break // ordered by value: the rest are smaller still
		}
		if fetched[a] || len(labelled[a]) > 0 {
			continue
		}
		left := time.Until(deadline)
		if left < 2*time.Second {
			break
		}
		fctx, cancel := context.WithTimeout(ctx, min(left, ringPerAddress))
		_, err := p.FetchAddress(fctx, a, 0)
		cancel()
		if err == nil {
			n++
		} else if fctx.Err() != nil && ctx.Err() == nil {
			_ = p.Queue(ctx, a, 0)
		}
	}
	return n
}
