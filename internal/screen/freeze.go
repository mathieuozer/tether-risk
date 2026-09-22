package screen

import (
	"context"
	"sort"
	"time"

	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/shopspring/decimal"
)

// frozenContact is a counterparty Tether has frozen, with the screened
// address's flow to and from it in the window before the freeze
// (docs/DECISIONS.md D34).
type frozenContact struct {
	Address  string
	FrozenAt time.Time
	// ReceivedUSD: what the screened address received from it; PaidUSD:
	// what the screened address paid it. Both within the window.
	ReceivedUSD, PaidUSD float64
	// LastFlow is the latest transfer between them before the freeze.
	LastFlow time.Time
}

// recentlyFrozen is every address Tether froze at or after since, as the
// label snapshot records it.
func (s *Service) recentlyFrozen(ctx context.Context, chainID string, snapshotID int64, since time.Time) (map[string]time.Time, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT address, (evidence->>'added_at')::timestamptz FROM labels
		WHERE chain = $1 AND source = 'tether_blacklist'
		  AND valid_from_snapshot <= $2 AND (valid_to_snapshot IS NULL OR $2 < valid_to_snapshot)
		  AND (evidence->>'added_at')::timestamptz >= $3`, chainID, snapshotID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var a string
		var t time.Time
		if err := rows.Scan(&a, &t); err != nil {
			return nil, err
		}
		out[a] = t.UTC()
	}
	return out, rows.Err()
}

// frozenContacts lists which of the frozen addresses had at least minUSD of
// flow with address in the windowDays before their freeze.
func (s *Service) frozenContacts(ctx context.Context, chainID, address string, frozen map[string]time.Time,
	windowDays int, minUSD float64) ([]frozenContact, error) {
	if len(frozen) == 0 {
		return nil, nil
	}
	cps := make([]string, 0, len(frozen))
	for a := range frozen {
		cps = append(cps, a)
	}
	// One read of the raw transfers between address and any of them; the
	// window differs per counterparty, so it is applied here.
	rows, err := s.ch.QueryContext(ctx, `
		SELECT to_address, 'out', toFloat64(ifNull(usd_value, 0)), block_time FROM transfers
		WHERE chain = ? AND from_address = ? AND to_address IN (?)
		UNION ALL
		SELECT from_address, 'in', toFloat64(ifNull(usd_value, 0)), block_time FROM transfers_by_to
		WHERE chain = ? AND to_address = ? AND from_address IN (?)`,
		chainID, address, cps, chainID, address, cps)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	window := time.Duration(windowDays) * 24 * time.Hour
	by := map[string]*frozenContact{}
	for rows.Next() {
		var cp, dir string
		var v float64
		var at time.Time
		if err := rows.Scan(&cp, &dir, &v, &at); err != nil {
			return nil, err
		}
		fz := frozen[cp]
		if at.Before(fz.Add(-window)) || !at.Before(fz) {
			continue
		}
		c := by[cp]
		if c == nil {
			c = &frozenContact{Address: cp, FrozenAt: fz}
			by[cp] = c
		}
		if dir == "in" {
			c.ReceivedUSD += v
		} else {
			c.PaidUSD += v
		}
		if at.After(c.LastFlow) {
			c.LastFlow = at.UTC()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []frozenContact
	for _, c := range by {
		if c.ReceivedUSD+c.PaidUSD >= minUSD {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FrozenAt.Equal(out[j].FrozenAt) {
			return out[i].FrozenAt.After(out[j].FrozenAt)
		}
		return out[i].Address < out[j].Address
	})
	return out, nil
}

// frozenContactFlag notes a wallet Tether froze in the last few days that
// paid address shortly before the freeze (docs/DECISIONS.md D34). Tether
// freezes in clusters: such a wallet is about 25 times likelier than an
// ordinary one to be frozen too, but only for about three days, so the note
// is shown in that time and not after. Like the poisoning note it is a
// warning, not a finding: it changes neither verdict nor confidence.
func (s *Service) frozenContactFlag(ctx context.Context, chainID, address string, snapshotID int64,
	now time.Time) (scoring.Flag, bool, error) {
	rule := s.cfg.Weights.Behaviour.FrozenContact
	if rule.WindowDays <= 0 || rule.MaxDaysSince <= 0 {
		return scoring.Flag{}, false, nil
	}
	since := now.Add(-time.Duration(rule.MaxDaysSince) * 24 * time.Hour)
	recent, err := s.recentlyFrozen(ctx, chainID, snapshotID, since)
	if err != nil || len(recent) == 0 {
		return scoring.Flag{}, false, err
	}
	contacts, err := s.frozenContacts(ctx, chainID, address, recent, rule.WindowDays, rule.MinReceivedUSD)
	if err != nil {
		return scoring.Flag{}, false, err
	}
	f := scoring.Flag{Code: "frozen_contact"}
	// contacts come latest freeze first.
	for _, c := range contacts {
		if c.FrozenAt.Before(since) || c.FrozenAt.After(now) || c.ReceivedUSD < rule.MinReceivedUSD {
			continue
		}
		if f.Count == 0 {
			f.Address, f.AmountUSD = c.Address, decimal.NewFromFloat(c.ReceivedUSD)
		}
		f.Count++
	}
	return f, f.Count > 0, nil
}
