// Package indexer connects pkg/tronindex to this engine: which transfers and
// events to keep, how to name and price them, and where to write them
// (docs/INDEXER_PLAN.md).
package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/mozer/tether-risk/internal/chain"
	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/store"
	"github.com/mozer/tether-risk/pkg/tronindex"
	"github.com/shopspring/decimal"
)

// Events we keep, by topic 0. Each hash was read from a real transaction's
// log on 2026-09-23, matched to the event TronGrid names for it, not
// recalled: AddedBlackList in ffc697…9fc2, Snapshot in 8aa1cd…7239, whose
// trx_balance 1,922,943,807,301 and token_balance 659,892,579,675 are the
// topics' values.
var Events = map[string]string{
	"42e160154868087d6bfdc0ca23d96a1c1cfa32f1b72ba9ba27b69b98a0d819dc": "AddedBlackList",
	"d7e9ec6e6ecd65492dce6bf513cd6867560d49544421d0783ddf06e76c24470c": "RemovedBlackList",
	"61e6e66b0d6339b2980aecc6ccc0039736791f0ccde9ed512e789a7fbdd698c6": "DestroyedBlackFunds",
	"cc7244d3535e7639366f8c5211527112e01de3ec7449ee3a6e66b007f4065a70": "Snapshot",
}

// JustSwapTRXUSDT is the pool D41 prices TRX from.
const JustSwapTRXUSDT = "TQn9Y2khEsLJW1ChVWFMSMeRDow5KcbLSE"

// Config is what the engine keeps: TRX, the recognised stablecoins, and the
// blacklist and price events. KeyTronGrid keeps rows identical to those
// fetched from TronGrid.
func Config() tronindex.Config {
	tokens := map[string]bool{}
	for contract := range tron.CanonicalTokens() {
		tokens[contract] = true
	}
	var filters []tronindex.EventFilter
	for topic, name := range Events {
		contract := tron.USDTContract
		if name == "Snapshot" {
			contract = JustSwapTRXUSDT
		}
		filters = append(filters, tronindex.EventFilter{Contract: contract, Topic0: topic})
	}
	return tronindex.Config{Tokens: tokens, NativeTRX: true, Events: filters, Keys: tronindex.KeyTronGrid}
}

// Pricer values a transfer as it is written; pricing.Pricer implements it.
type Pricer interface {
	Price(ctx context.Context, asset string, raw *big.Int, at time.Time) (*decimal.Decimal, string, error)
}

// Sink writes blocks into a ClickHouse database with this engine's schema.
type Sink struct {
	CH     *sql.DB
	Writer *store.TransferWriter
	Pricer Pricer
}

// WriteBlocks writes the blocks not yet in indexed_blocks: their transfers,
// their events, and then the blocks themselves. A crash after the transfers
// and before the block rows would write that batch's transfers twice on the
// retry; audit-edges (D37) finds and repairs that.
func (s *Sink) WriteBlocks(ctx context.Context, blocks []tronindex.BlockData) error {
	if len(blocks) == 0 {
		return nil
	}
	done, err := s.written(ctx, blocks[0].Number, blocks[len(blocks)-1].Number)
	if err != nil {
		return err
	}
	var transfers []chain.Transfer
	var todo []tronindex.BlockData
	for _, b := range blocks {
		if done[b.Number] {
			continue
		}
		todo = append(todo, b)
		for _, t := range b.Transfers {
			ct, err := s.transfer(ctx, t)
			if err != nil {
				return err
			}
			transfers = append(transfers, ct)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	if err := s.Writer.InsertRaw(ctx, transfers); err != nil {
		return err
	}
	if err := s.writeEvents(ctx, todo); err != nil {
		return err
	}
	return s.markWritten(ctx, todo)
}

func (s *Sink) transfer(ctx context.Context, t tronindex.Transfer) (chain.Transfer, error) {
	asset := "TRX"
	if t.Token != "" {
		asset = tron.AssetForContract(t.Token)
	}
	ct := chain.Transfer{
		Chain: "tron", TxHash: t.TxID, LogIndex: t.Index,
		// The node knows the block, unlike TronGrid's TRC-20 endpoint (D12).
		BlockNumber: t.Block, BlockTime: t.Time,
		FromAddress: t.From, ToAddress: t.To, Asset: asset, RawValue: t.Value, PriceBasis: "unpriced",
	}
	if s.Pricer != nil {
		v, basis, err := s.Pricer.Price(ctx, asset, t.Value, t.Time)
		if err != nil {
			return ct, err
		}
		ct.USDValue, ct.PriceBasis = v, basis
	}
	return ct, nil
}

func (s *Sink) written(ctx context.Context, lo, hi uint64) (map[uint64]bool, error) {
	rows, err := s.CH.QueryContext(ctx, `SELECT block FROM indexed_blocks WHERE chain = 'tron' AND block BETWEEN ? AND ?`, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint64]bool{}
	for rows.Next() {
		var n uint64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

func (s *Sink) writeEvents(ctx context.Context, blocks []tronindex.BlockData) error {
	var any bool
	for _, b := range blocks {
		if len(b.Events) > 0 {
			any = true
		}
	}
	if !any {
		return nil
	}
	tx, err := s.CH.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO contract_events
		(chain, contract, event, tx_hash, position, block_number, block_time, topics, data)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, b := range blocks {
		for _, e := range b.Events {
			name := "unknown"
			if len(e.Topics) > 0 {
				if n, ok := Events[strings.ToLower(strings.TrimPrefix(e.Topics[0], "0x"))]; ok {
					name = n
				}
			}
			if _, err := stmt.ExecContext(ctx, "tron", e.Contract, name, e.TxID, uint32(e.Position),
				e.Block, e.Time, e.Topics, e.Data); err != nil {
				tx.Rollback()
				return fmt.Errorf("event %s/%d: %w", e.TxID, e.Position, err)
			}
		}
	}
	return tx.Commit()
}

func (s *Sink) markWritten(ctx context.Context, blocks []tronindex.BlockData) error {
	tx, err := s.CH.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO indexed_blocks (chain, block, block_time, transfers)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, b := range blocks {
		if _, err := stmt.ExecContext(ctx, "tron", b.Number, b.Time, uint32(len(b.Transfers))); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// Cursors keeps indexer cursors in PostgreSQL.
type Cursors struct{ PG *sql.DB }

func (c Cursors) Load(ctx context.Context, name string) (uint64, bool, error) {
	var n int64
	err := c.PG.QueryRowContext(ctx, `SELECT block FROM indexer_cursors WHERE name = $1`, name).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return uint64(n), err == nil, err
}

func (c Cursors) Save(ctx context.Context, name string, block uint64) error {
	_, err := c.PG.ExecContext(ctx, `
		INSERT INTO indexer_cursors (name, block, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (name) DO UPDATE SET block = EXCLUDED.block, updated_at = now()`, name, int64(block))
	return err
}
