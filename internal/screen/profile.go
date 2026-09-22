package screen

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/shopspring/decimal"
)

// profiledCategories are the categories whose connections get a profile. A
// named entity is identified by its label. An unnamed service is identified
// only by what it does, so what it does is shown: volume, reach and dates.
var profiledCategories = map[string]bool{"unnamed_service": true}

// profile fills in Connection.Profile from stored chain data. Only addresses
// whose own history has been fetched are profiled. For any other address the
// stored edges are just the transfers seen from the other side, and a
// profile built from those would understate it without saying so. Only
// assets carrying value are listed; unrecognised tokens are typically spam.
func (s *Service) profile(ctx context.Context, chainID string, res *scoring.Result) error {
	want := map[string][]*scoring.Connection{}
	for _, d := range []*scoring.DirectionResult{res.Inbound, res.Outbound} {
		if d == nil {
			continue
		}
		for i := range d.Connections {
			c := &d.Connections[i]
			if profiledCategories[c.Category] {
				want[c.Address] = append(want[c.Address], c)
			}
		}
	}
	if len(want) == 0 {
		return nil
	}
	addrs := make([]string, 0, len(want))
	for a := range want {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)

	partial := map[string]bool{}
	rows, err := s.pg.QueryContext(ctx,
		`SELECT address, truncated FROM address_freshness WHERE chain = $1 AND address = ANY($2)`,
		chainID, addrs)
	if err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	for rows.Next() {
		var a string
		var truncated bool
		if err := rows.Scan(&a, &truncated); err != nil {
			rows.Close()
			return fmt.Errorf("profile: %w", err)
		}
		partial[a] = truncated
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	fetched := make([]string, 0, len(partial))
	for _, a := range addrs {
		if _, ok := partial[a]; ok {
			fetched = append(fetched, a)
		}
	}
	if len(fetched) == 0 {
		return nil
	}

	crows, err := s.ch.QueryContext(ctx, `
		SELECT addr, sum(usd), sum(n), uniqExact(cp), min(first), max(last), arraySort(groupUniqArrayIf(asset, usd > 0))
		FROM (
			SELECT from_address AS addr, to_address AS cp, asset,
			       total_usd_value AS usd, transfer_count AS n, first_seen AS first, last_seen AS last
			FROM edges_current WHERE chain = ? AND from_address IN (?)
			UNION ALL
			SELECT to_address, from_address, asset,
			       total_usd_value, transfer_count, first_seen, last_seen
			FROM edges_by_to_current WHERE chain = ? AND to_address IN (?))
		GROUP BY addr`,
		chainID, fetched, chainID, fetched)
	if err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	defer crows.Close()
	for crows.Next() {
		var (
			a           string
			vol         decimal.Decimal
			n, cps      uint64
			first, last time.Time
			assets      []string
		)
		if err := crows.Scan(&a, &vol, &n, &cps, &first, &last, &assets); err != nil {
			return fmt.Errorf("profile: %w", err)
		}
		p := &scoring.Profile{
			VolumeUSD: vol, Transfers: n, Counterparties: cps,
			FirstSeen: first, LastSeen: last, Assets: assets, Partial: partial[a],
		}
		for _, c := range want[a] {
			c.Profile = p
		}
	}
	return crows.Err()
}
