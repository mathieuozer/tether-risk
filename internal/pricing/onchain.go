package pricing

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/mozer/tether-risk/internal/chain/evm"
	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/config"
	"github.com/shopspring/decimal"
)

// Daily closes read from DEX pools (docs/DECISIONS.md D41).
//
// Every other fact this engine uses comes from public chain data, and so can
// prices. A price vendor's terms either forbid keeping its data or require
// attribution and a paid plan; a pool's reserves are neither. The close for a
// UTC day is the pool's price at the last state before midnight: the stable
// side's balance over the asset's.

// SourceOnChain is the `source` recorded for these prices.
const SourceOnChain = "onchain"

// maxStaleness is how old a pool's last state may be and still price a day.
// A pool with no trade for longer says nothing about that day's price.
const maxStaleness = 48 * time.Hour

// TronPoolReader reads a TRON pool. tron.Client implements it.
type TronPoolReader interface {
	LastEventBefore(ctx context.Context, contract, event string, before time.Time) (*tron.ContractEvent, error)
	ConstantCall(ctx context.Context, contract, selector string) (string, error)
}

// EVMPoolReader reads an EVM pool. evm.Client implements it.
type EVMPoolReader interface {
	Latest(ctx context.Context) (uint64, error)
	BlockTime(ctx context.Context, n uint64) (time.Time, error)
	CallAt(ctx context.Context, to, data string, block uint64) (string, error)
}

var _ TronPoolReader = (*tron.Client)(nil)
var _ EVMPoolReader = (*evm.Client)(nil)

// TronCloses reads daily closes from a JustSwap exchange, from `from` to the
// day before `to`. Each exchange emits Snapshot(trx_balance, token_balance)
// on every trade, so one event per day is one request.
func TronCloses(ctx context.Context, r TronPoolReader, p config.PricePool, from, to time.Time) ([]PricePoint, error) {
	tok, err := r.ConstantCall(ctx, p.Pool, "tokenAddress()")
	if err != nil {
		return nil, err
	}
	if len(tok) < 40 {
		return nil, fmt.Errorf("pool %s: tokenAddress() returned %q", p.Pool, tok)
	}
	got, err := tron.HexToBase58("41" + tok[len(tok)-40:])
	if err != nil {
		return nil, err
	}
	if got != p.Quote {
		return nil, fmt.Errorf("pool %s trades %s, not the configured %s; no price read", p.Pool, got, p.Quote)
	}

	var out []PricePoint
	for day := startDay(from, p.Since); day.Before(to); day = day.AddDate(0, 0, 1) {
		end := day.AddDate(0, 0, 1)
		ev, err := r.LastEventBefore(ctx, p.Pool, "Snapshot", end)
		if err != nil {
			return out, fmt.Errorf("%s %s: %w", p.Asset, day.Format("2006-01-02"), err)
		}
		if ev == nil || end.Sub(ev.Time) > maxStaleness {
			continue
		}
		trx, ok1 := new(big.Int).SetString(ev.Result["trx_balance"], 10)
		usd, ok2 := new(big.Int).SetString(ev.Result["token_balance"], 10)
		if !ok1 || !ok2 || trx.Sign() <= 0 {
			continue
		}
		// Both sides have 6 decimals: TRX in sun, USDT in its units.
		price := decimal.NewFromBigInt(usd, 0).Div(decimal.NewFromBigInt(trx, 0))
		out = append(out, PricePoint{Asset: p.Asset, Date: day.Format("2006-01-02"), USD: price.Round(8)})
	}
	return out, nil
}

// EVMCloses reads daily closes from a Uniswap v2 style pair: its reserves at
// the last block before each UTC midnight.
func EVMCloses(ctx context.Context, r EVMPoolReader, p config.PricePool, from, to time.Time) ([]PricePoint, error) {
	latest, err := r.Latest(ctx)
	if err != nil {
		return nil, err
	}
	t0, err := word(ctx, r, p.Pool, "0x0dfe1681", latest) // token0()
	if err != nil {
		return nil, err
	}
	t1, err := word(ctx, r, p.Pool, "0xd21220a7", latest) // token1()
	if err != nil {
		return nil, err
	}
	base, quote := strings.ToLower(p.Base), strings.ToLower(p.Quote)
	var baseFirst bool
	switch {
	case addr(t0) == base && addr(t1) == quote:
		baseFirst = true
	case addr(t0) == quote && addr(t1) == base:
	default:
		return nil, fmt.Errorf("pool %s holds %s and %s, not the configured pair; no price read", p.Pool, addr(t0), addr(t1))
	}
	bd, err := decimals(ctx, r, base, latest)
	if err != nil {
		return nil, err
	}
	qd, err := decimals(ctx, r, quote, latest)
	if err != nil {
		return nil, err
	}

	f := &blockFinder{r: r, hi: latest}
	var out []PricePoint
	for day := startDay(from, p.Since); day.Before(to); day = day.AddDate(0, 0, 1) {
		end := day.AddDate(0, 0, 1)
		n, err := f.lastBefore(ctx, end)
		if err != nil {
			return out, fmt.Errorf("%s %s: %w", p.Asset, day.Format("2006-01-02"), err)
		}
		res, err := r.CallAt(ctx, p.Pool, "0x0902f1ac", n) // getReserves()
		if err != nil {
			return out, fmt.Errorf("%s %s reserves: %w", p.Asset, day.Format("2006-01-02"), err)
		}
		h := strings.TrimPrefix(res, "0x")
		if len(h) < 192 {
			continue // no pair at that block yet
		}
		r0, _ := new(big.Int).SetString(h[:64], 16)
		r1, _ := new(big.Int).SetString(h[64:128], 16)
		last, _ := new(big.Int).SetString(h[128:192], 16)
		if last != nil && end.Sub(time.Unix(last.Int64(), 0)) > maxStaleness {
			continue
		}
		rb, rq := r0, r1
		if !baseFirst {
			rb, rq = r1, r0
		}
		if rb == nil || rq == nil || rb.Sign() <= 0 {
			continue
		}
		price := decimal.NewFromBigInt(rq, -int32(qd)).Div(decimal.NewFromBigInt(rb, -int32(bd)))
		out = append(out, PricePoint{Asset: p.Asset, Date: day.Format("2006-01-02"), USD: price.Round(8)})
	}
	return out, nil
}

// blockFinder finds the last block before a time by binary search. Days are
// asked in order, so each search starts where the previous one ended, and
// its upper bound is guessed from the block rate seen so far: searching up to
// the chain's head every day cost 25 calls a day instead of about 12 (D41).
type blockFinder struct {
	r    EVMPoolReader
	lo   uint64    // a block known to be before every time still to be asked
	loT  time.Time // its time, once known
	hi   uint64    // the newest block
	rate float64   // blocks per second, once two days are known
}

func (f *blockFinder) lastBefore(ctx context.Context, t time.Time) (uint64, error) {
	lo, hi := f.lo, f.hi
	if f.rate > 0 && !f.loT.IsZero() {
		// Half again the expected distance, checked before it is trusted:
		// block times vary, and BSC's changed in 2025.
		guess := lo + uint64(f.rate*t.Sub(f.loT).Seconds()*1.5) + 1
		if guess < hi {
			bt, err := f.r.BlockTime(ctx, guess)
			if err != nil {
				return 0, err
			}
			if !bt.Before(t) {
				hi = guess
			}
		}
	}
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		bt, err := f.r.BlockTime(ctx, mid)
		if err != nil {
			return 0, err
		}
		if bt.Before(t) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	bt, err := f.r.BlockTime(ctx, lo)
	if err != nil {
		return 0, err
	}
	if !f.loT.IsZero() && bt.After(f.loT) {
		f.rate = float64(lo-f.lo) / bt.Sub(f.loT).Seconds()
	}
	f.lo, f.loT = lo, bt
	return lo, nil
}

func word(ctx context.Context, r EVMPoolReader, to, data string, block uint64) (string, error) {
	h, err := r.CallAt(ctx, to, data, block)
	if err != nil {
		return "", err
	}
	h = strings.TrimPrefix(h, "0x")
	if len(h) < 64 {
		return "", fmt.Errorf("%s %s returned %q", to, data, h)
	}
	return h[:64], nil
}

func addr(w string) string { return "0x" + strings.ToLower(w[len(w)-40:]) }

func decimals(ctx context.Context, r EVMPoolReader, token string, block uint64) (int64, error) {
	w, err := word(ctx, r, token, "0x313ce567", block) // decimals()
	if err != nil {
		return 0, err
	}
	n, ok := new(big.Int).SetString(w, 16)
	if !ok || !n.IsInt64() || n.Int64() > 36 {
		return 0, fmt.Errorf("token %s decimals %q", token, w)
	}
	return n.Int64(), nil
}

// startDay is the later of from and the pool's first day, at UTC midnight.
func startDay(from time.Time, since string) time.Time {
	d := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	if s, err := time.Parse("2006-01-02", since); err == nil && s.After(d) {
		return s
	}
	return d
}
