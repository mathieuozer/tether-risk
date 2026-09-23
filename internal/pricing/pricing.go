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
	"sort"
	"sync"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/store"
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
	// not re-query per row. Guarded because ingest workers share one Pricer.
	mu    sync.Mutex
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
	p.mu.Lock()
	v, ok := p.cache[key]
	p.mu.Unlock()
	if ok {
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
	p.mu.Lock()
	p.cache[key] = usd
	p.mu.Unlock()
	return usd, true, nil
}

// BackfillResult reports what a repricing pass did.
type BackfillResult struct {
	Scanned  int64
	Priced   int64
	Unpriced int64
	ByBasis  map[string]int64
	Edges    int // edges recomputed
}

// Priceable lists the assets this Pricer can value.
func (p *Pricer) Priceable() []string {
	var out []string
	for asset := range assetDecimals {
		if _, ok := p.pinned[asset]; ok || p.daily[asset] {
			out = append(out, asset)
		}
	}
	sort.Strings(out)
	return out
}

// Backfill prices stored transfers that were written unpriced and can be
// priced now, usually because the day's close had not been loaded yet.
//
// Since D22 transfers are priced as they are written, so this is a small
// set. Until 2026-09-23 it reread and rewrote every stored transfer (16
// million rows), held them all in memory, and truncated and rebuilt the edge
// tables. Screens during the rebuild saw addresses with no flows, and the
// run outgrew its timeout (docs/DECISIONS.md D35). Now only the repriced
// rows are rewritten, and only the edges they belong to are recomputed.
func (p *Pricer) Backfill(ctx context.Context, ch *sql.DB, w EdgeRepairer, chainID string, log *slog.Logger) (*BackfillResult, error) {
	res := &BackfillResult{ByBasis: map[string]int64{}}
	assets := p.Priceable()
	if len(assets) == 0 {
		return res, nil
	}

	rows, err := ch.QueryContext(ctx, `
		SELECT chain, tx_hash, log_index, block_number, block_time,
		       from_address, to_address, asset, raw_value
		FROM transfers FINAL
		WHERE chain = ? AND price_basis = 'unpriced' AND asset IN (?)`, chainID, assets)
	if err != nil {
		return nil, fmt.Errorf("read unpriced transfers: %w", err)
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
		res.ByBasis[basis]++
		if usd == nil {
			// Still no price for its day; left as it is.
			res.Unpriced++
			continue
		}
		r.usd, r.basis = usd, basis
		res.Priced++
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
	pairs := map[store.EdgeKey]bool{}
	for _, r := range batch {
		if _, err := stmt.ExecContext(ctx,
			r.chain, r.txHash, r.logIndex, r.blockNumber, r.blockTime,
			r.from, r.to, r.asset, r.raw, *r.usd, r.basis); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("reprice %s: %w", r.txHash, err)
		}
		pairs[store.EdgeKey{From: r.from, To: r.to, Asset: r.asset}] = true
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// The edge views summed each rewritten row a second time (D2). Recompute
	// exactly the edges those rows belong to from the deduplicated
	// transfers.
	keys := make([]store.EdgeKey, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	if err := w.RepairEdges(ctx, chainID, keys); err != nil {
		return nil, fmt.Errorf("repair edges: %w", err)
	}
	res.Edges = len(keys)

	log.Info("transfers repriced", "scanned", res.Scanned,
		"priced", res.Priced, "unpriced", res.Unpriced, "edges_repaired", res.Edges)
	return res, nil
}

// EdgeRepairer recomputes named edges from the transfers they aggregate.
// store.TransferWriter implements it.
type EdgeRepairer interface {
	RepairEdges(ctx context.Context, chainID string, keys []store.EdgeKey) error
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

// RepriceAll revalues every stored transfer of one asset from the current
// daily closes, inside ClickHouse: the closes go into a scratch table and one
// INSERT ... SELECT joins them on the day. A day without a close leaves the
// transfer unpriced. The rewrite is summed a second time by the edge views,
// so the edges must be rebuilt afterwards, with nothing else writing: this is
// a maintenance job (docs/DECISIONS.md D41).
func (p *Pricer) RepriceAll(ctx context.Context, ch *sql.DB, chainID, asset string) (int64, error) {
	dec, ok := assetDecimals[asset]
	if !ok || !p.daily[asset] {
		return 0, fmt.Errorf("%s is not valued from daily closes", asset)
	}
	rows, err := p.pg.QueryContext(ctx, `SELECT price_date, usd FROM prices WHERE asset = $1 ORDER BY price_date`, asset)
	if err != nil {
		return 0, err
	}
	type close struct {
		day time.Time
		usd decimal.Decimal
	}
	var closes []close
	for rows.Next() {
		var c close
		if err := rows.Scan(&c.day, &c.usd); err != nil {
			rows.Close()
			return 0, err
		}
		closes = append(closes, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, q := range []string{
		`DROP TABLE IF EXISTS reprice_closes`,
		`CREATE TABLE reprice_closes (day Date, usd Decimal(38, 12)) ENGINE = Memory`,
	} {
		if _, err := ch.ExecContext(ctx, q); err != nil {
			return 0, err
		}
	}
	defer ch.ExecContext(context.WithoutCancel(ctx), `DROP TABLE IF EXISTS reprice_closes`)
	tx, err := ch.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO reprice_closes (day, usd)`)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	for _, c := range closes {
		if _, err := stmt.ExecContext(ctx, c.day, c.usd); err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}

	var n int64
	if err := ch.QueryRowContext(ctx, `SELECT count() FROM transfers WHERE chain = ? AND asset = ?`, chainID, asset).Scan(&n); err != nil {
		return 0, err
	}
	_, err = ch.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO transfers
			(chain, tx_hash, log_index, block_number, block_time,
			 from_address, to_address, asset, raw_value, usd_value, price_basis)
		SELECT t.chain, t.tx_hash, t.log_index, t.block_number, t.block_time,
		       t.from_address, t.to_address, t.asset, t.raw_value,
		       if(isNull(c.usd), NULL,
		          toDecimal128(toDecimal256(t.raw_value, 0) * toDecimal256(c.usd, 12) / toDecimal256(%d, 0), 6)),
		       if(isNull(c.usd), '%s', '%s')
		FROM (SELECT * FROM transfers FINAL WHERE chain = ? AND asset = ?) AS t
		LEFT JOIN reprice_closes AS c ON toDate(t.block_time) = c.day
		SETTINGS join_use_nulls = 1`, pow10(dec), BasisUnpriced, BasisDailyClose), chainID, asset)
	if err != nil {
		return 0, fmt.Errorf("reprice %s: %w", asset, err)
	}
	return n, nil
}

func pow10(n int32) int64 {
	v := int64(1)
	for i := int32(0); i < n; i++ {
		v *= 10
	}
	return v
}
