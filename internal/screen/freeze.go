package screen

import (
	"context"
	"sort"
	"time"
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

// frozenContacts lists the counterparties of address that Tether froze
// after at least minUSD of flow with it in the windowDays before the freeze.
func (s *Service) frozenContacts(ctx context.Context, chainID, address string, snapshotID int64,
	windowDays int, minUSD float64) ([]frozenContact, error) {
	rows, err := s.ch.QueryContext(ctx, `
		SELECT DISTINCT cp FROM (
			SELECT to_address AS cp FROM edges_current WHERE chain = ? AND from_address = ?
			UNION ALL
			SELECT from_address AS cp FROM edges_by_to_current WHERE chain = ? AND to_address = ?
		)`, chainID, address, chainID, address)
	if err != nil {
		return nil, err
	}
	var cps []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return nil, err
		}
		cps = append(cps, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frozen := map[string]time.Time{}
	for start := 0; start < len(cps); start += 5000 {
		end := min(start+5000, len(cps))
		byAddr, err := s.store.ForAddresses(ctx, snapshotID, chainID, cps[start:end])
		if err != nil {
			return nil, err
		}
		for a, ls := range byAddr {
			for _, l := range ls {
				if l.Source != "tether_blacklist" {
					continue
				}
				if at, ok := l.Evidence["added_at"].(string); ok {
					if t, err := time.Parse(time.RFC3339, at); err == nil {
						frozen[a] = t.UTC()
					}
				}
			}
		}
	}

	var out []frozenContact
	window := time.Duration(windowDays) * 24 * time.Hour
	for cp, at := range frozen {
		c := frozenContact{Address: cp, FrozenAt: at}
		var last time.Time
		if err := s.ch.QueryRowContext(ctx, `
			SELECT sumIf(v, dir = 'in'), sumIf(v, dir = 'out'), max(block_time) FROM (
				SELECT 'out' AS dir, toFloat64(ifNull(usd_value, 0)) AS v, block_time FROM transfers
				WHERE chain = ? AND from_address = ? AND to_address = ? AND block_time >= ? AND block_time < ?
				UNION ALL
				SELECT 'in' AS dir, toFloat64(ifNull(usd_value, 0)) AS v, block_time FROM transfers
				WHERE chain = ? AND from_address = ? AND to_address = ? AND block_time >= ? AND block_time < ?
			)`,
			chainID, address, cp, at.Add(-window), at,
			chainID, cp, address, at.Add(-window), at).Scan(&c.ReceivedUSD, &c.PaidUSD, &last); err != nil {
			return nil, err
		}
		if c.ReceivedUSD+c.PaidUSD < minUSD {
			continue
		}
		c.LastFlow = last.UTC()
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FrozenAt.Equal(out[j].FrozenAt) {
			return out[i].FrozenAt.After(out[j].FrozenAt)
		}
		return out[i].Address < out[j].Address
	})
	return out, nil
}
