// Package pricing assigns USD values to transfers.
//
// docs/PLAN.md F3: no commercial price feed is available. Values come from two
// sources of very different quality — pinned stablecoins and a daily close
// series — and every transfer records which applied, so a score resting on
// imprecisely-valued native flows can be told apart from one resting on
// stablecoins.
package pricing

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/shopspring/decimal"
)

// Basis records how a USD value was arrived at.
const (
	BasisPinned     = "pinned"
	BasisDailyClose = "daily_close"
	BasisUnpriced   = "unpriced"
)

// Decimals per asset. Token amounts are integers in the asset's smallest unit,
// so a wrong exponent is a factor-of-a-million error in the score — which is
// why these are explicit rather than inferred.
var assetDecimals = map[string]int32{
	"USDT": 6,
	"USDC": 6,
	"TUSD": 18,
	"USDD": 18,
	"TRX":  6,
	"ETH":  18,
	"BNB":  18,
}

// Pricer converts raw amounts to USD.
type Pricer struct {
	cfg    *config.Config
	pg     *sql.DB
	pinned map[string]decimal.Decimal
	daily  map[string]bool

	// cache of (asset, day) -> price, so a backfill over many transfers does
	// not re-query per row.
	cache map[string]decimal.Decimal
}

func New(cfg *config.Config, pg *sql.DB) *Pricer {
	p := &Pricer{
		cfg: cfg, pg: pg,
		pinned: map[string]decimal.Decimal{},
		daily:  map[string]bool{},
		cache:  map[string]decimal.Decimal{},
	}
	for asset, v := range cfg.Weights.Pricing.Pinned {
		p.pinned[asset] = decimal.NewFromFloat(v)
	}
	for _, asset := range cfg.Weights.Pricing.DailyClose {
		p.daily[asset] = true
	}
	return p
}

// Price returns the USD value of a raw amount, and the basis used.
//
// A nil result means unpriced, which is deliberately different from zero:
// "we do not know what this was worth" and "this was worth nothing" lead to
// different conclusions, and conflating them hides exposure.
func (p *Pricer) Price(ctx context.Context, asset string, raw *big.Int, at time.Time) (*decimal.Decimal, string, error) {
	dec, ok := assetDecimals[asset]
	if !ok {
		return nil, BasisUnpriced, nil
	}
	amount := decimal.NewFromBigInt(raw, -dec)

	if unit, ok := p.pinned[asset]; ok {
		v := amount.Mul(unit)
		return &v, BasisPinned, nil
	}

	if p.daily[asset] {
		unit, found, err := p.dailyClose(ctx, asset, at)
		if err != nil {
			return nil, BasisUnpriced, err
		}
		if !found {
			// No price for that day. Unpriced rather than guessed:
			// interpolating across a gap would invent a number that looks
			// like a measurement.
			return nil, BasisUnpriced, nil
		}
		v := amount.Mul(unit)
		return &v, BasisDailyClose, nil
	}

	return nil, BasisUnpriced, nil
}

func (p *Pricer) dailyClose(ctx context.Context, asset string, at time.Time) (decimal.Decimal, bool, error) {
	day := at.UTC().Format("2006-01-02")
	key := asset + "/" + day
	if v, ok := p.cache[key]; ok {
		return v, true, nil
	}

	var usd decimal.Decimal
	err := p.pg.QueryRowContext(ctx,
		`SELECT usd FROM prices WHERE asset = $1 AND price_date = $2`, asset, day).Scan(&usd)
	if err == sql.ErrNoRows {
		return decimal.Zero, false, nil
	}
	if err != nil {
		return decimal.Zero, false, fmt.Errorf("read price %s/%s: %w", asset, day, err)
	}
	p.cache[key] = usd
	return usd, true, nil
}

// BackfillResult reports what a repricing pass did.
type BackfillResult struct {
	Scanned  int64
	Priced   int64
	Unpriced int64
	ByBasis  map[string]int64
}

// Backfill reprices stored transfers.
//
// SPEC.md §4 leaves usd_value nullable until Phase 3; this is the pass that
// fills it. It rewrites `transfers` and then rebuilds the edge tables from
// scratch, because the edge aggregates were computed from the old values and a
// materialized view cannot retroactively revise what it already summed
// (docs/DECISIONS.md D2).
func (p *Pricer) Backfill(ctx context.Context, ch *sql.DB, chainID string, log *slog.Logger) (*BackfillResult, error) {
	res := &BackfillResult{ByBasis: map[string]int64{}}

	rows, err := ch.QueryContext(ctx, `
		SELECT chain, tx_hash, log_index, block_number, block_time,
		       from_address, to_address, asset, raw_value
		FROM transfers FINAL
		WHERE chain = ?`, chainID)
	if err != nil {
		return nil, fmt.Errorf("read transfers: %w", err)
	}

	type repriced struct {
		chain       string
		txHash      string
		logIndex    uint32
		blockNumber uint64
		blockTime   time.Time
		from, to    string
		asset       string
		raw         *big.Int
		usd         *decimal.Decimal
		basis       string
	}
	var batch []repriced

	for rows.Next() {
		var r repriced
		var raw big.Int
		if err := rows.Scan(&r.chain, &r.txHash, &r.logIndex, &r.blockNumber,
			&r.blockTime, &r.from, &r.to, &r.asset, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		r.raw = &raw
		res.Scanned++

		usd, basis, err := p.Price(ctx, r.asset, r.raw, r.blockTime)
		if err != nil {
			rows.Close()
			return nil, err
		}
		r.usd, r.basis = usd, basis
		res.ByBasis[basis]++
		if usd != nil {
			res.Priced++
		} else {
			res.Unpriced++
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if len(batch) == 0 {
		return res, nil
	}

	// Replace the rows. ReplacingMergeTree collapses on the sorting key, so
	// re-inserting the same natural key with a newer ingested_at supersedes
	// the old row.
	tx, err := ch.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO transfers
			(chain, tx_hash, log_index, block_number, block_time,
			 from_address, to_address, asset, raw_value, usd_value, price_basis)`)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	for _, r := range batch {
		var usd any
		if r.usd != nil {
			usd = *r.usd
		}
		if _, err := stmt.ExecContext(ctx,
			r.chain, r.txHash, r.logIndex, r.blockNumber, r.blockTime,
			r.from, r.to, r.asset, r.raw, usd, r.basis); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("reprice %s: %w", r.txHash, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	log.Info("transfers repriced", "scanned", res.Scanned,
		"priced", res.Priced, "unpriced", res.Unpriced)
	return res, nil
}

// LoadPrices writes daily close prices.
type PricePoint struct {
	Asset string
	Date  string // YYYY-MM-DD
	USD   decimal.Decimal
}

func LoadPrices(ctx context.Context, pg *sql.DB, source string, points []PricePoint) (int, error) {
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var n int
	for _, p := range points {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO prices (asset, price_date, usd, basis, source)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (asset, price_date) DO UPDATE SET
				usd = EXCLUDED.usd, source = EXCLUDED.source, loaded_at = now()`,
			p.Asset, p.Date, p.USD, BasisDailyClose, source); err != nil {
			return n, fmt.Errorf("insert price %s/%s: %w", p.Asset, p.Date, err)
		}
		n++
	}
	return n, tx.Commit()
}
