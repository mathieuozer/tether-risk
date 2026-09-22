package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
)

// A comparison sample for a competitor (SPEC.md §9.5): addresses drawn from
// every kind a customer brings, so agreement is measured where it is easy
// (listed addresses) and where it is hard (ordinary wallets, services). The
// owner screens each in the competitor's tool and fills in the columns;
// `validate compare` then explains every divergence. Weights are never tuned
// to match.

// compareColumns are the template's columns. The first two are ours; the
// rest the owner copies from the competitor's report, in percent.
var compareColumns = []string{"address", "stratum", "vendor", "score", "band",
	"exchange", "high_risk_exchange", "sanctions", "scam", "stolen_funds", "darknet", "mixer", "gambling",
	"frozen", "other_risk", "notes"}

type stratum struct {
	name string
	n    int
	pick func(ctx context.Context, n int) ([]string, error)
}

// writeCompareSample writes a stratified list of n addresses to path. The
// draw is seeded, so the same data gives the same list.
func writeCompareSample(ctx context.Context, ch, pg *sql.DB, chainID string, n int, path string) error {
	rng := rand.New(rand.NewSource(20260922))
	shuffled := func(q string, want int, args ...any) ([]string, error) {
		rows, err := pg.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var all []string
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				return nil, err
			}
			all = append(all, a)
		}
		sort.Strings(all)
		rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
		return all[:min(want, len(all))], rows.Err()
	}
	fetched := `address IN (SELECT address FROM address_freshness WHERE chain = $1)`

	share := func(pct int) int { return max(1, n*pct/100) }
	strata := []stratum{
		{"listed", share(15), func(ctx context.Context, k int) ([]string, error) {
			return shuffled(`SELECT DISTINCT address FROM labels WHERE chain = $1 AND valid_to_snapshot IS NULL
				AND source IN ('tether_blacklist','ofac') AND `+fetched, k, chainID)
		}},
		{"exposed", share(25), func(ctx context.Context, k int) ([]string, error) {
			listed, err := pgAddrs(ctx, pg, `SELECT address FROM labels WHERE chain = $1 AND valid_to_snapshot IS NULL
				AND source IN ('tether_blacklist','ofac') LIMIT $2`, chainID, 1<<30)
			if err != nil {
				return nil, err
			}
			all, err := exposedAddrs(ctx, ch, pg, chainID, listed, 4*k)
			if err != nil {
				return nil, err
			}
			rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
			return all[:min(k, len(all))], nil
		}},
		{"deposit", share(20), func(ctx context.Context, k int) ([]string, error) {
			return shuffled(`SELECT DISTINCT address FROM labels WHERE chain = $1 AND valid_to_snapshot IS NULL
				AND source = 'derived:deposit' AND `+fetched, k, chainID)
		}},
		{"service", share(10), func(ctx context.Context, k int) ([]string, error) {
			return shuffled(`SELECT DISTINCT address FROM labels WHERE chain = $1 AND valid_to_snapshot IS NULL
				AND source = 'derived:service'`, k, chainID)
		}},
		// Ordinary wallets: fetched, unlabelled. What most customers bring.
		{"wallet", n, func(ctx context.Context, k int) ([]string, error) {
			return shuffled(`SELECT f.address FROM address_freshness f WHERE f.chain = $1
				AND NOT EXISTS (SELECT 1 FROM labels l WHERE l.chain = f.chain AND l.address = f.address AND l.valid_to_snapshot IS NULL)`,
				k, chainID)
		}},
	}

	seen := map[string]bool{}
	var rows [][]string
	for _, s := range strata {
		want := s.n
		if s.name == "wallet" {
			want = n - len(rows)
		}
		got, err := s.pick(ctx, want*2)
		if err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		k := 0
		for _, a := range got {
			if k == want || seen[a] {
				continue
			}
			seen[a] = true
			rows = append(rows, append([]string{a, s.name}, make([]string, len(compareColumns)-2)...))
			k++
		}
		fmt.Printf("%-8s %3d of %d\n", s.name, k, want)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write(compareColumns)
	_ = w.WriteAll(rows)
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	fmt.Printf("%d addresses written to %s\n", len(rows), path)
	return nil
}
