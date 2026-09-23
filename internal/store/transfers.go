package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/mozer/tether-risk/internal/chain"
)

// TransferWriter inserts transfers into ClickHouse.
//
// ---------------------------------------------------------------------------
// Why this type deduplicates before inserting
// ---------------------------------------------------------------------------
// `edges` is an AggregatingMergeTree maintained by a materialized view over
// `transfers`, and all traversal reads `edges` (SPEC.md §4). A ClickHouse
// materialized view fires on the rows of each INSERT, not on the rows that
// survive deduplication. So inserting an already-known transfer a second time
// adds its value to `edges` a second time, permanently, even though
// ReplacingMergeTree will later collapse the duplicate in `transfers` itself.
//
// The failure is silent: no error, no constraint violation, just edge values
// that drift upward every time a fetch is retried, and every score computed
// from them quietly wrong.
//
// SPEC.md §5 requires ingestion be idempotent and safely re-runnable. That
// cannot be delegated to the table engine, so it is enforced here.
// See docs/DECISIONS.md D2.
type TransferWriter struct {
	ch *sql.DB
	pg *sql.DB
}

func NewTransferWriter(ch, pg *sql.DB) *TransferWriter {
	return &TransferWriter{ch: ch, pg: pg}
}

// WriteResult reports what a write actually did. The gap between Fetched and
// Inserted is the expected steady state when re-fetching an address, not a
// fault.
type WriteResult struct {
	Fetched     int
	Inserted    int
	Duplicates  int
	ContentHash string
	SkippedPage bool // the ingest ledger already recorded this page
}

// WritePage writes one page of an address's history, deduplicating against
// what is already stored and recording the batch in the ingest ledger.
//
// Both defences matter and neither is redundant:
//   - the ledger check avoids the work entirely when a whole page is replayed,
//   - the key check catches overlapping pages, reorgs, and any transfer that
//     arrived by another route.
func (w *TransferWriter) WritePage(ctx context.Context, address, pageKey string, transfers []chain.Transfer) (WriteResult, error) {
	res := WriteResult{Fetched: len(transfers)}
	if len(transfers) == 0 {
		return res, nil
	}

	chainID := transfers[0].Chain
	for _, t := range transfers {
		if err := t.Validate(); err != nil {
			return res, fmt.Errorf("refusing to write invalid transfer: %w", err)
		}
		if t.Chain != chainID {
			return res, fmt.Errorf("page mixes chains %q and %q", chainID, t.Chain)
		}
	}

	// Defence 2: has this exact page already been written?
	already, err := w.pageAlreadyWritten(ctx, chainID, address, pageKey)
	if err != nil {
		return res, err
	}
	if already {
		res.SkippedPage = true
		return res, nil
	}

	// Defence 1: drop transfers already present in ClickHouse, then insert
	// the rest, with no other writer between the two. A transfer between two
	// addresses appears in both addresses' histories. Fetched at the same
	// time, by two workers or a worker and a screen, both pages found it
	// missing and both inserted it, and the edge views counted it twice. A
	// job lease is per address, so it did not prevent this; measured
	// 2026-09-23 as 1,493 extra transfers in the edges, 0.009% (D37).
	unlock, err := w.lockWrites(ctx, chainID)
	if err != nil {
		return res, err
	}
	fresh, err := w.filterExisting(ctx, chainID, transfers)
	if err != nil {
		unlock()
		return res, err
	}
	res.Duplicates = len(transfers) - len(fresh)
	res.ContentHash = contentHash(fresh)

	if len(fresh) > 0 {
		if err := w.insert(ctx, fresh); err != nil {
			unlock()
			return res, err
		}
	}
	unlock()
	res.Inserted = len(fresh)

	// Record the batch only after the insert succeeded. Recording first would
	// mean a crash mid-insert left the page marked done with its rows missing,
	// and nothing would ever fetch them again.
	if err := w.recordBatch(ctx, chainID, address, pageKey, res); err != nil {
		return res, err
	}
	return res, nil
}

// lockWrites takes the chain's write lock, a PostgreSQL advisory lock that
// every process writing transfers shares, and returns its release. The lock
// is held for one page's check and insert, well under a second.
func (w *TransferWriter) lockWrites(ctx context.Context, chainID string) (func(), error) {
	conn, err := w.pg.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("write lock: %w", err)
	}
	key := "transfers:" + chainID
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext($1))`, key); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write lock: %w", err)
	}
	return func() {
		// Released even when ctx has ended, or the session would keep the
		// lock until the pool closed the connection.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, key)
		conn.Close()
	}, nil
}

func (w *TransferWriter) pageAlreadyWritten(ctx context.Context, chainID, address, pageKey string) (bool, error) {
	var exists bool
	err := w.pg.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM ingest_batches
			WHERE chain = $1 AND address = $2 AND page_key = $3
		)`, chainID, address, pageKey).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check ingest ledger: %w", err)
	}
	return exists, nil
}

// filterExisting removes transfers whose natural key is already in ClickHouse.
func (w *TransferWriter) filterExisting(ctx context.Context, chainID string, transfers []chain.Transfer) ([]chain.Transfer, error) {
	// Query by (tx_hash, log_index) pairs. Chunked so a large page does not
	// build an unbounded IN list.
	const chunk = 2000

	seen := make(map[chain.Key]bool, len(transfers))
	for i := 0; i < len(transfers); i += chunk {
		end := min(i+chunk, len(transfers))
		batch := transfers[i:end]

		// A stored copy of a transfer has the same sender and time, so both
		// narrow the search. They lead the table's sorting key, and without
		// them the lookup read the whole table: 16 million rows and 7 s per
		// page on 2026-09-23, growing with every address fetched (D35).
		placeholders := make([]string, 0, len(batch))
		pairs := []any{}
		senders := map[string]bool{}
		lo, hi := batch[0].BlockTime, batch[0].BlockTime
		for _, t := range batch {
			pairs = append(pairs, t.TxHash, t.LogIndex)
			placeholders = append(placeholders, "(?, ?)")
			senders[t.FromAddress] = true
			if t.BlockTime.Before(lo) {
				lo = t.BlockTime
			}
			if t.BlockTime.After(hi) {
				hi = t.BlockTime
			}
		}
		from := make([]string, 0, len(senders))
		for a := range senders {
			from = append(from, a)
		}
		sort.Strings(from)

		q := fmt.Sprintf(
			`SELECT DISTINCT tx_hash, log_index FROM transfers
			 WHERE chain = ? AND from_address IN (?) AND block_time BETWEEN ? AND ?
			   AND (tx_hash, log_index) IN (%s)`,
			strings.Join(placeholders, ","))
		args := append([]any{chainID, from, lo.UTC(), hi.UTC()}, pairs...)

		rows, err := w.ch.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("check existing transfers: %w", err)
		}
		for rows.Next() {
			var txHash string
			var logIndex uint32
			if err := rows.Scan(&txHash, &logIndex); err != nil {
				rows.Close()
				return nil, err
			}
			seen[chain.Key{Chain: chainID, TxHash: txHash, LogIndex: logIndex}] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	// Deduplicate within the page too: an upstream API can return the same
	// event twice across overlapping pages, and that duplicate would inflate
	// `edges` exactly as a re-insert would.
	inPage := make(map[chain.Key]bool, len(transfers))
	out := make([]chain.Transfer, 0, len(transfers))
	for _, t := range transfers {
		k := t.Key()
		if seen[k] || inPage[k] {
			continue
		}
		inPage[k] = true
		out = append(out, t)
	}
	return out, nil
}

// InsertRaw writes transfers without the duplicate check or the ingest
// ledger. It is for a source that guarantees uniqueness itself: our own
// index writes each block once, checked against indexed_blocks
// (docs/INDEXER_PLAN.md). Anything else must use WritePage.
func (w *TransferWriter) InsertRaw(ctx context.Context, transfers []chain.Transfer) error {
	if len(transfers) == 0 {
		return nil
	}
	return w.insert(ctx, transfers)
}

func (w *TransferWriter) insert(ctx context.Context, transfers []chain.Transfer) error {
	tx, err := w.ch.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin clickhouse batch: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO transfers
			(chain, tx_hash, log_index, block_number, block_time,
			 from_address, to_address, asset, raw_value, usd_value, price_basis)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare transfer insert: %w", err)
	}

	for _, t := range transfers {
		var usd any
		if t.USDValue != nil {
			usd = *t.USDValue
		}
		basis := t.PriceBasis
		if basis == "" {
			basis = "unpriced"
		}
		if _, err := stmt.ExecContext(ctx,
			t.Chain, t.TxHash, t.LogIndex, t.BlockNumber, t.BlockTime,
			t.FromAddress, t.ToAddress, t.Asset, t.RawValue, usd, basis,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("append transfer %s: %w", t.Key(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transfer batch: %w", err)
	}
	return nil
}

func (w *TransferWriter) recordBatch(ctx context.Context, chainID, address, pageKey string, res WriteResult) error {
	_, err := w.pg.ExecContext(ctx, `
		INSERT INTO ingest_batches (chain, address, page_key, rows_fetched, rows_inserted, content_hash)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (chain, address, page_key) DO NOTHING`,
		chainID, address, pageKey, res.Fetched, res.Inserted, res.ContentHash)
	if err != nil {
		return fmt.Errorf("record ingest batch: %w", err)
	}
	return nil
}

// contentHash hashes the natural keys of a batch, so a rebuild can verify it
// reproduced exactly what was originally written. Keys are sorted first: the
// hash must describe the set, not the order the adapter happened to return.
func contentHash(transfers []chain.Transfer) string {
	keys := make([]string, 0, len(transfers))
	for _, t := range transfers {
		keys = append(keys, t.Key().String())
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Edge rebuild
// ---------------------------------------------------------------------------

// RebuildEdges recomputes `edges` and `edges_by_to` from deduplicated
// `transfers`. This is the third defence (docs/DECISIONS.md D2) and the
// ground truth the incremental materialized views are tested against.
//
// FINAL forces ReplacingMergeTree to collapse duplicates at read time, so the
// rebuild sees each transfer exactly once regardless of how many times it was
// inserted.
func (w *TransferWriter) RebuildEdges(ctx context.Context) error {
	for _, spec := range []struct{ table, orderCol string }{
		{"edges", "from_address"},
		{"edges_by_to", "to_address"},
	} {
		if _, err := w.ch.ExecContext(ctx, "TRUNCATE TABLE "+spec.table); err != nil {
			return fmt.Errorf("truncate %s: %w", spec.table, err)
		}
		q := fmt.Sprintf(`
			INSERT INTO %s
			SELECT
				chain,
				from_address,
				to_address,
				asset,
				sum(ifNull(usd_value, toDecimal64(0, 6))) AS total_usd_value,
				sum(raw_value)                            AS total_raw_value,
				count()                                   AS transfer_count,
				countIf(usd_value IS NULL)                AS unpriced_count,
				min(block_time)                           AS first_seen,
				max(block_time)                           AS last_seen
			FROM transfers FINAL
			GROUP BY chain, from_address, to_address, asset
			-- 17 million transfers group past the 6.9 GB memory limit;
			-- spill to disk instead (D41).
			SETTINGS max_bytes_before_external_group_by = 2000000000`, spec.table)
		if _, err := w.ch.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("rebuild %s: %w", spec.table, err)
		}
	}
	return nil
}

// EdgeKey names one aggregated edge.
type EdgeKey struct{ From, To, Asset string }

// RepairEdges recomputes the named edges, in both edge tables, from the
// deduplicated transfers. It is what a rewrite of a few transfers needs: the
// materialized views summed the rewritten rows a second time (D2), and a
// full RebuildEdges would empty both tables while it runs (D35).
//
// Not atomic: a transfer on the same edge inserted between the delete and
// the recompute is counted twice until the edge is next repaired or rebuilt.
// Edges being repaired are ones whose prices just arrived, so the window
// matters little; a full rebuild remains the ground truth.
func (w *TransferWriter) RepairEdges(ctx context.Context, chainID string, keys []EdgeKey) error {
	const chunk = 500
	for i := 0; i < len(keys); i += chunk {
		part := keys[i:min(i+chunk, len(keys))]
		// The senders alone, as well as the tuples: from_address leads both
		// tables' sorting keys, and a tuple IN does not use it. Without it
		// each chunk read 5.6 million rows (D37).
		tuples := make([]string, 0, len(part))
		senders := map[string]bool{}
		var pairs []any
		for _, k := range part {
			tuples = append(tuples, "(?, ?, ?)")
			pairs = append(pairs, k.From, k.To, k.Asset)
			senders[k.From] = true
		}
		from := make([]string, 0, len(senders))
		for a := range senders {
			from = append(from, a)
		}
		sort.Strings(from)
		args := append([]any{chainID, from}, pairs...)
		in := strings.Join(tuples, ",")
		for _, table := range []string{"edges", "edges_by_to"} {
			if _, err := w.ch.ExecContext(ctx, fmt.Sprintf(
				`DELETE FROM %s WHERE chain = ? AND from_address IN (?) AND (from_address, to_address, asset) IN (%s)`, table, in), args...); err != nil {
				return fmt.Errorf("delete %s: %w", table, err)
			}
			if _, err := w.ch.ExecContext(ctx, fmt.Sprintf(`
				INSERT INTO %s
				SELECT
					chain, from_address, to_address, asset,
					sum(ifNull(usd_value, toDecimal64(0, 6))) AS total_usd_value,
					sum(raw_value)                            AS total_raw_value,
					count()                                   AS transfer_count,
					countIf(usd_value IS NULL)                AS unpriced_count,
					min(block_time)                           AS first_seen,
					max(block_time)                           AS last_seen
				FROM transfers FINAL
				WHERE chain = ? AND from_address IN (?) AND (from_address, to_address, asset) IN (%s)
				GROUP BY chain, from_address, to_address, asset`, table, in), args...); err != nil {
				return fmt.Errorf("recompute %s: %w", table, err)
			}
		}
	}
	return nil
}

// AuditEdges finds edges whose count or value disagrees with the
// deduplicated transfers they aggregate, in either edge table. Addresses are
// split into buckets by hash so each comparison fits in memory; the whole
// table at once exceeded ClickHouse's 6.9 GB limit (docs/DECISIONS.md D37).
// An edge written to while the audit reads it can show up as a false
// mismatch; repairing it is harmless.
func (w *TransferWriter) AuditEdges(ctx context.Context, chainID string, buckets int) ([]EdgeKey, error) {
	seen := map[EdgeKey]bool{}
	var out []EdgeKey
	for b := 0; b < buckets; b++ {
		for _, table := range []string{"edges", "edges_by_to"} {
			rows, err := w.ch.QueryContext(ctx, fmt.Sprintf(`
				WITH e AS (
					SELECT from_address f, to_address t, asset a,
					       sum(transfer_count) n, sum(total_usd_value) v
					FROM %s WHERE chain = ? AND cityHash64(from_address) %% ? = ?
					GROUP BY f, t, a),
				x AS (
					SELECT fa f, ta t, aa a, count() n, sum(ifNull(u, toDecimal64(0, 6))) v
					FROM (
						SELECT tx_hash, log_index,
						       any(from_address) fa, any(to_address) ta, any(asset) aa,
						       argMax(usd_value, ingested_at) u
						FROM transfers WHERE chain = ? AND cityHash64(from_address) %% ? = ?
						GROUP BY tx_hash, log_index)
					GROUP BY f, t, a)
				SELECT if(e.f = '', x.f, e.f), if(e.f = '', x.t, e.t), if(e.f = '', x.a, e.a)
				FROM e FULL OUTER JOIN x ON e.f = x.f AND e.t = x.t AND e.a = x.a
				WHERE e.n != x.n OR e.v != x.v
				SETTINGS join_algorithm = 'grace_hash', join_use_nulls = 0`, table),
				chainID, buckets, b, chainID, buckets, b)
			if err != nil {
				return nil, fmt.Errorf("audit %s bucket %d: %w", table, b, err)
			}
			for rows.Next() {
				var k EdgeKey
				if err := rows.Scan(&k.From, &k.To, &k.Asset); err != nil {
					rows.Close()
					return nil, err
				}
				if seen[k] {
					continue
				}
				seen[k] = true
				out = append(out, k)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
		}
	}
	return out, nil
}

// Edge is one aggregated address-to-address flow.
type Edge struct {
	Chain         string
	FromAddress   string
	ToAddress     string
	Asset         string
	TotalUSDValue chain.Decimal
	TotalRawValue *big.Int
	TransferCount uint64
	UnpricedCount uint64
}

// EdgeTotals returns the aggregated edges for one address in one direction,
// reading through the deduplicating view rather than the raw table.
func (w *TransferWriter) EdgeTotals(ctx context.Context, chainID, address, direction string) ([]Edge, error) {
	var view, col string
	switch direction {
	case "outbound":
		view, col = "edges_current", "from_address"
	case "inbound":
		view, col = "edges_by_to_current", "to_address"
	default:
		return nil, fmt.Errorf("unknown direction %q", direction)
	}

	q := fmt.Sprintf(`
		SELECT chain, from_address, to_address, asset,
		       total_usd_value, total_raw_value, transfer_count, unpriced_count
		FROM %s
		WHERE chain = ? AND %s = ?
		ORDER BY total_usd_value DESC, to_address ASC, from_address ASC, asset ASC`, view, col)

	rows, err := w.ch.QueryContext(ctx, q, chainID, address)
	if err != nil {
		return nil, fmt.Errorf("read edges: %w", err)
	}
	defer rows.Close()

	var out []Edge
	for rows.Next() {
		var e Edge
		var raw big.Int
		if err := rows.Scan(&e.Chain, &e.FromAddress, &e.ToAddress, &e.Asset,
			&e.TotalUSDValue, &raw, &e.TransferCount, &e.UnpricedCount); err != nil {
			return nil, err
		}
		e.TotalRawValue = &raw
		out = append(out, e)
	}
	return out, rows.Err()
}
