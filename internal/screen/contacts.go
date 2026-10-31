package screen

import (
	"context"
	"fmt"
	"sort"

	"github.com/mozer/tether-risk/internal/graph"
	"github.com/mozer/tether-risk/internal/labels"
	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/shopspring/decimal"
)

// Counterparties' own contacts with listed addresses (docs/DECISIONS.md
// D48): what the wallets that paid, or were paid by, the screened address
// themselves received from or sent to sanctioned, frozen and similar
// addresses. Read from stored edges, judged by scoring.JudgeContacts.

// listedSet is every address in the contact categories at one snapshot,
// resolved to its winning label. Loaded once per snapshot: 8,000 rows that
// every screen would otherwise re-read.
type listedSet struct {
	snapshot int64
	chain    string
	by       map[string]scoring.ListedAddress
	addrs    []string
}

// listedChunk bounds how many listed addresses go into one IN clause. The
// query text is limited to 256 KB; 4,000 addresses of 34 bytes is 150 KB.
const listedChunk = 4000

func (s *Service) listed(ctx context.Context, chainID string, snapshotID int64, categories []string) (*listedSet, error) {
	s.listedMu.Lock()
	defer s.listedMu.Unlock()
	if c := s.listedCache; c != nil && c.snapshot == snapshotID && c.chain == chainID {
		return c, nil
	}
	ls, err := s.store.ByCategories(ctx, snapshotID, chainID, categories)
	if err != nil {
		return nil, fmt.Errorf("listed addresses: %w", err)
	}
	byAddr := map[string][]labels.Label{}
	for _, l := range ls {
		byAddr[l.Address] = append(byAddr[l.Address], l)
	}
	want := map[string]bool{}
	for _, c := range categories {
		want[c] = true
	}
	set := &listedSet{snapshot: snapshotID, chain: chainID, by: map[string]scoring.ListedAddress{}}
	for addr, group := range byAddr {
		r := s.resolver.Resolve(chainID, addr, group)
		// The winner may be a label outside the categories (a poisoning
		// label outranking nothing, say); only a listed winner counts.
		if !r.Labelled || !want[r.Category] {
			continue
		}
		set.by[addr] = scoring.ListedAddress{Category: r.Category, Entity: r.Entity}
		set.addrs = append(set.addrs, addr)
	}
	sort.Strings(set.addrs)
	s.listedCache = set
	return set, nil
}

// contacts lists the screened address's direct counterparties' own flows
// with listed addresses.
func (s *Service) contacts(ctx context.Context, chainID, address string, snapshotID int64) ([]scoring.Contact, error) {
	rules := s.cfg.Weights.Behaviour.CounterpartyContact
	if !rules.Enabled || len(rules.Categories) == 0 {
		return nil, nil
	}
	set, err := s.listed(ctx, chainID, snapshotID, rules.Categories)
	if err != nil {
		return nil, err
	}
	if len(set.addrs) == 0 {
		return nil, nil
	}

	edges := clickhouseEdges{ch: s.ch}
	// A counterparty on both sides is judged on the side with more flow:
	// listed twice it would read as two wallets.
	byAddr := map[string]*scoring.ContactCounterparty{}
	var order []string
	for _, dir := range []graph.Direction{graph.Inbound, graph.Outbound} {
		nb, err := edges.Neighbours(ctx, chainID, address, dir)
		if err != nil {
			return nil, fmt.Errorf("contacts: %w", err)
		}
		sort.Slice(nb, func(i, j int) bool {
			if !nb[i].USDValue.Equal(nb[j].USDValue) {
				return nb[i].USDValue.GreaterThan(nb[j].USDValue)
			}
			return nb[i].Address < nb[j].Address
		})
		if rules.MaxCounterparties > 0 && len(nb) > rules.MaxCounterparties {
			nb = nb[:rules.MaxCounterparties]
		}
		for _, n := range nb {
			// A counterparty that is itself listed is exposure the score
			// already carries, not a contact.
			if _, listed := set.by[n.Address]; listed || n.Address == address {
				continue
			}
			if c, seen := byAddr[n.Address]; seen {
				if n.USDValue.GreaterThan(c.FlowUSD) {
					c.Inbound, c.FlowUSD = dir == graph.Inbound, n.USDValue
				}
				continue
			}
			byAddr[n.Address] = &scoring.ContactCounterparty{
				Address: n.Address, Inbound: dir == graph.Inbound, FlowUSD: n.USDValue, TotalUSD: decimal.Zero,
			}
			order = append(order, n.Address)
		}
	}
	if len(order) == 0 {
		return nil, nil
	}
	cps := make([]scoring.ContactCounterparty, 0, len(order))
	var inAddrs, outAddrs []string
	for _, a := range order {
		c := byAddr[a]
		cps = append(cps, *c)
		if c.Inbound {
			inAddrs = append(inAddrs, a)
		} else {
			outAddrs = append(outAddrs, a)
		}
	}

	all := append(append([]string{}, inAddrs...), outAddrs...)
	resolved, err := postgresLabels{store: s.store, resolver: s.resolver, snapshotID: snapshotID}.Lookup(ctx, chainID, all)
	if err != nil {
		return nil, fmt.Errorf("contacts: %w", err)
	}
	fetched, err := pgHistory{pg: s.pg}.Fetched(ctx, chainID, all)
	if err != nil {
		return nil, fmt.Errorf("contacts: %w", err)
	}
	totals, err := s.contactTotals(ctx, chainID, inAddrs, outAddrs)
	if err != nil {
		return nil, err
	}
	for i := range cps {
		c := &cps[i]
		if r, ok := resolved[c.Address]; ok && r.Labelled {
			c.Entity, c.Category = r.Entity, r.Category
		}
		c.Fetched = fetched[c.Address]
		if t, ok := totals[c.Address]; ok {
			c.TotalUSD = t
		}
	}

	var flows []scoring.ContactFlow
	for start := 0; start < len(set.addrs); start += listedChunk {
		end := start + listedChunk
		if end > len(set.addrs) {
			end = len(set.addrs)
		}
		chunk := set.addrs[start:end]
		if len(inAddrs) > 0 {
			fs, err := s.contactFlows(ctx, `
				SELECT to_address, from_address, sum(total_usd_value), sum(transfer_count)
				FROM edges_by_to_current
				WHERE chain = ? AND to_address IN (?) AND from_address IN (?)
				GROUP BY to_address, from_address`, chainID, inAddrs, chunk, true)
			if err != nil {
				return nil, err
			}
			flows = append(flows, fs...)
		}
		if len(outAddrs) > 0 {
			fs, err := s.contactFlows(ctx, `
				SELECT from_address, to_address, sum(total_usd_value), sum(transfer_count)
				FROM edges_current
				WHERE chain = ? AND from_address IN (?) AND to_address IN (?)
				GROUP BY from_address, to_address`, chainID, outAddrs, chunk, false)
			if err != nil {
				return nil, err
			}
			flows = append(flows, fs...)
		}
	}
	return scoring.JudgeContacts(cps, flows, set.by, rules), nil
}

// contactTotals reads each counterparty's whole volume on its risky side:
// everything an inbound counterparty received, everything an outbound one
// sent. Only meaningful for fetched addresses; the caller marks the rest
// partial.
func (s *Service) contactTotals(ctx context.Context, chainID string, inAddrs, outAddrs []string) (map[string]decimal.Decimal, error) {
	out := map[string]decimal.Decimal{}
	for _, q := range []struct {
		addrs []string
		query string
	}{
		{inAddrs, `SELECT to_address, sum(total_usd_value) FROM edges_by_to_current
		           WHERE chain = ? AND to_address IN (?) GROUP BY to_address`},
		{outAddrs, `SELECT from_address, sum(total_usd_value) FROM edges_current
		            WHERE chain = ? AND from_address IN (?) GROUP BY from_address`},
	} {
		if len(q.addrs) == 0 {
			continue
		}
		rows, err := s.ch.QueryContext(ctx, q.query, chainID, q.addrs)
		if err != nil {
			return nil, fmt.Errorf("contact totals: %w", err)
		}
		for rows.Next() {
			var a string
			var v decimal.Decimal
			if err := rows.Scan(&a, &v); err != nil {
				rows.Close()
				return nil, fmt.Errorf("contact totals: %w", err)
			}
			out[a] = v
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Service) contactFlows(ctx context.Context, query, chainID string, cps, listed []string, inbound bool) ([]scoring.ContactFlow, error) {
	rows, err := s.ch.QueryContext(ctx, query, chainID, cps, listed)
	if err != nil {
		return nil, fmt.Errorf("contact flows: %w", err)
	}
	defer rows.Close()
	var out []scoring.ContactFlow
	for rows.Next() {
		f := scoring.ContactFlow{Inbound: inbound}
		if err := rows.Scan(&f.Counterparty, &f.Listed, &f.USD, &f.Transfers); err != nil {
			return nil, fmt.Errorf("contact flows: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
