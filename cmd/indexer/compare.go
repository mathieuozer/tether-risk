package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/mozer/tether-risk/internal/chain"
	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/store"
)

// compare checks the index against TronGrid (the plan's phase 2 check): for
// n addresses active in the window the index covers, every transfer TronGrid
// lists must be in the index, and nothing else. Half the addresses are drawn
// from USDT transfers, half from all. It reads TronGrid through the
// adapter's own parsing, so the keys compared are the keys stored.
func compare(ctx context.Context, db string, n int, log *slog.Logger) error {
	os.Setenv("CLICKHOUSE_DB", db)
	ch, err := store.OpenClickHouse(ctx)
	if err != nil {
		return err
	}
	defer ch.Close()

	var first, last uint64
	var lo, hi time.Time
	var blocks uint64
	if err := ch.QueryRowContext(ctx, `
		SELECT min(block), argMin(block_time, block), max(block), argMax(block_time, block), uniqExact(block)
		FROM indexed_blocks WHERE chain = 'tron'`).Scan(&first, &lo, &last, &hi, &blocks); err != nil {
		return err
	}
	if blocks == 0 {
		return fmt.Errorf("the index is empty")
	}
	if missing := last - first + 1 - blocks; missing > 0 {
		return fmt.Errorf("%d blocks missing between %d and %d; a comparison would count them as differences", missing, first, last)
	}
	// The first and last seconds may hold blocks only partly indexed from
	// TronGrid's view (it filters by timestamp); keep whole seconds inside.
	lo, hi = lo.Add(time.Second).UTC(), hi.Add(-time.Minute).UTC()
	if !hi.After(lo) {
		return fmt.Errorf("the index covers too little time (%s to %s)", lo, hi)
	}
	fmt.Printf("window: blocks %d..%d, %s to %s\n", first, last, lo.Format(time.RFC3339), hi.Format(time.RFC3339))

	addrs, err := sampleAddresses(ctx, ch, lo, hi, n)
	if err != nil {
		return err
	}

	key := os.Getenv("TRONGRID_API_KEY")
	ad := tron.NewAdapter(tron.NewClient(tron.Options{APIKey: key, RequestsPerSecond: 2, MaxRetries: 8, Logger: log}))
	kept := map[string]bool{"TRX": true}
	for _, asset := range tron.CanonicalTokens() {
		kept[asset] = true
	}

	var same, differ, gridRows, indexRows int
	for i, a := range addrs {
		grid, err := fromTronGrid(ctx, ad, a, lo, hi, kept)
		if err != nil {
			return fmt.Errorf("%s: %w", a, err)
		}
		idx, err := fromIndex(ctx, ch, a, lo, hi)
		if err != nil {
			return err
		}
		gridRows += len(grid)
		indexRows += len(idx)
		onlyGrid, onlyIndex := diff(grid, idx), diff(idx, grid)
		if len(onlyGrid) == 0 && len(onlyIndex) == 0 {
			same++
			continue
		}
		differ++
		fmt.Printf("\n%d. %s: TronGrid %d, index %d\n", i+1, a, len(grid), len(idx))
		for _, k := range first5(onlyGrid) {
			fmt.Printf("   only in TronGrid: %s\n", k)
		}
		for _, k := range first5(onlyIndex) {
			fmt.Printf("   only in index:    %s\n", k)
		}
	}
	fmt.Printf("\n%d addresses: %d identical, %d different; %d transfers from TronGrid, %d from the index\n",
		len(addrs), same, differ, gridRows, indexRows)
	if differ > 0 {
		return fmt.Errorf("%d addresses differ", differ)
	}
	return nil
}

func sampleAddresses(ctx context.Context, ch *sql.DB, lo, hi time.Time, n int) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, assetCond := range []string{"asset = 'USDT'", "1"} {
		rows, err := ch.QueryContext(ctx, fmt.Sprintf(`
			SELECT a FROM (
				SELECT from_address AS a FROM transfers WHERE chain = 'tron' AND block_time BETWEEN ? AND ? AND %[1]s
				UNION ALL
				SELECT to_address AS a FROM transfers_by_to WHERE chain = 'tron' AND block_time BETWEEN ? AND ? AND %[1]s)
			GROUP BY a HAVING count() <= 150 ORDER BY rand() LIMIT ?`, assetCond), lo, hi, lo, hi, n)
		if err != nil {
			return nil, err
		}
		for rows.Next() && len(out) < n {
			var a string
			if err := rows.Scan(&a); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
			if assetCond != "1" && len(out) >= n/2 {
				break
			}
		}
		rows.Close()
	}
	return out, nil
}

func transferKey(t chain.Transfer) string {
	return fmt.Sprintf("%s/%d %s %s->%s %s", t.TxHash, t.LogIndex, t.Asset, t.FromAddress, t.ToAddress, t.RawValue)
}

func fromTronGrid(ctx context.Context, ad *tron.Adapter, addr string, lo, hi time.Time, kept map[string]bool) (map[string]bool, error) {
	out := map[string]bool{}
	cur := ad.WindowCursor(addr, lo, hi)
	for {
		page, err := ad.FetchAddress(ctx, addr, cur)
		if err != nil {
			return nil, err
		}
		for _, t := range page.Transfers {
			if kept[t.Asset] && !t.BlockTime.Before(lo) && !t.BlockTime.After(hi) {
				out[transferKey(t)] = true
			}
		}
		if page.Next.Done {
			return out, nil
		}
		cur = page.Next
	}
}

func fromIndex(ctx context.Context, ch *sql.DB, addr string, lo, hi time.Time) (map[string]bool, error) {
	rows, err := ch.QueryContext(ctx, `
		SELECT tx_hash, log_index, asset, from_address, to_address, toString(raw_value) FROM transfers FINAL
		WHERE chain = 'tron' AND from_address = ? AND block_time BETWEEN ? AND ?
		UNION DISTINCT
		SELECT tx_hash, log_index, asset, from_address, to_address, toString(raw_value) FROM transfers_by_to FINAL
		WHERE chain = 'tron' AND to_address = ? AND block_time BETWEEN ? AND ?`, addr, lo, hi, addr, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var tx, asset, from, to, raw string
		var n uint32
		if err := rows.Scan(&tx, &n, &asset, &from, &to, &raw); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%s/%d %s %s->%s %s", tx, n, asset, from, to, raw)] = true
	}
	return out, rows.Err()
}

func diff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func first5(s []string) []string {
	if len(s) > 5 {
		return s[:5]
	}
	return s
}
