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

	// Defence 1: drop transfers already present in ClickHouse.
	fresh, err := w.filterExisting(ctx, chainID, transfers)
	if err != nil {
		return res, err
	}
	res.Duplicates = len(transfers) - len(fresh)
	res.ContentHash = contentHash(fresh)

	if len(fresh) > 0 {
		if err := w.insert(ctx, fresh); err != nil {
			return res, err
		}
	}
	res.Inserted = len(fresh)

	// Record the batch only after the insert succeeded. Recording first would
	// mean a crash mid-insert left the page marked done with its rows missing,
	// and nothing would ever fetch them again.
	if err := w.recordBatch(ctx, chainID, address, pageKey, res); err != nil {
		return res, err
	}
	return res, nil
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

		placeholders := make([]string, 0, len(batch))
		args := []any{chainID}
		for _, t := range batch {
			args = append(args, t.TxHash, t.LogIndex)
			placeholders = append(placeholders, "(?, ?)")
		}

		q := fmt.Sprintf(
			`SELECT DISTINCT tx_hash, log_index FROM transfers
			 WHERE chain = ? AND (tx_hash, log_index) IN (%s)`,
			strings.Join(placeholders, ","))

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
			GROUP BY chain, from_address, to_address, asset`, spec.table)
		if _, err := w.ch.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("rebuild %s: %w", spec.table, err)
		}
	}
	return nil
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
