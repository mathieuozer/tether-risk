// Command price loads daily close prices and reprices stored transfers.
//
//	price load <asset>      fetch and store the daily close series
//	price backfill          reprice stored transfers, then rebuild edges
//	price status            what is priced and what is not
//
// docs/PLAN.md F3: no commercial price feed is available. Stablecoins are
// pinned from configuration; everything else uses a daily close series, which
// carries real error on an individual transfer. The error is recorded on each
// transfer as `price_basis` rather than being smoothed away.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/pricing"
	"github.com/mozer/tether-risk/internal/store"
	"github.com/shopspring/decimal"
)

// coingeckoIDs maps our asset symbols to the free public API's identifiers.
//
// CoinGecko's free tier caps history at 365 days, which is not enough: the
// earliest TRX transfer held is from 2022-07-09, so a year of prices leaves
// four years of transfers unpriced and therefore invisible to traversal.
var coingeckoIDs = map[string]string{
	"TRX": "tron",
	"ETH": "ethereum",
	"BNB": "binancecoin",
}

// binanceSymbols maps our asset symbols to Binance spot pairs.
//
// Binance publishes daily klines with no key and history back to 2018, which
// covers everything we hold. The pairs quote against USDT rather than USD.
// That is a closer fit than it first appears: weights.yaml pins USDT to 1.0,
// so valuing TRX in USDT is internally consistent with how every stablecoin
// transfer in the system is already valued. Treating the peg as exact is the
// same assumption in both places rather than a new one.
var binanceSymbols = map[string]string{
	"TRX": "TRXUSDT",
	"ETH": "ETHUSDT",
	"BNB": "BNBUSDT",
}

// fetchBinanceDailyCloses retrieves daily closing prices from `from` to now.
//
// Binance returns at most 1000 candles per request, so the window is walked
// forward until it reaches the present.
func fetchBinanceDailyCloses(ctx context.Context, asset string, from time.Time) ([]pricing.PricePoint, error) {
	symbol, ok := binanceSymbols[asset]
	if !ok {
		return nil, fmt.Errorf("no binance symbol configured for asset %q", asset)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	seen := map[string]bool{}
	var out []pricing.PricePoint

	cursor := from
	for iteration := 0; iteration < 50; iteration++ { // bounded; 50k days is far past any need
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		u := fmt.Sprintf(
			"https://api.binance.com/api/v3/klines?symbol=%s&interval=1d&startTime=%d&limit=1000",
			symbol, cursor.UnixMilli())

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch binance klines: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("binance returned %d: %s", resp.StatusCode, truncate(body, 200))
		}

		// Each candle is a heterogeneous array: [openTime, open, high, low,
		// close, volume, closeTime, ...]. Index 4 is the close.
		var candles [][]json.RawMessage
		if err := json.Unmarshal(body, &candles); err != nil {
			return nil, fmt.Errorf("decode binance klines: %w", err)
		}
		if len(candles) == 0 {
			break
		}

		var lastOpen int64
		for _, c := range candles {
			if len(c) < 5 {
				continue
			}
			var openMs int64
			if err := json.Unmarshal(c[0], &openMs); err != nil {
				continue
			}
			var closeStr string
			if err := json.Unmarshal(c[4], &closeStr); err != nil {
				continue
			}
			price, err := decimal.NewFromString(closeStr)
			if err != nil {
				continue
			}

			day := time.UnixMilli(openMs).UTC().Format("2006-01-02")
			if !seen[day] {
				seen[day] = true
				out = append(out, pricing.PricePoint{Asset: asset, Date: day, USD: price})
			}
			lastOpen = openMs
		}

		if len(candles) < 1000 {
			break // reached the present
		}
		cursor = time.UnixMilli(lastOpen).Add(24 * time.Hour)
		if cursor.After(time.Now()) {
			break
		}
	}

	return out, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

func main() {
	var (
		configDir = flag.String("config", "config", "configuration directory")
		chainID   = flag.String("chain", "tron", "chain to reprice")
		days      = flag.Int("days", 365, "days of history (coingecko source only)")
		source    = flag.String("source", "binance",
			"price source: binance (daily closes back to 2018, no key) or coingecko (365 days max)")
		from = flag.String("from", "",
			"earliest date to load, YYYY-MM-DD; default is the earliest unpriced transfer held")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := flag.Arg(0)
	if cmd == "" {
		fmt.Fprintln(os.Stderr, "usage: price [flags] load <asset> | backfill | status")
		os.Exit(2)
	}

	if err := run(ctx, cmd, *configDir, *chainID, *days, *source, *from, log); err != nil {
		log.Error("failed", "command", cmd, "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd, configDir, chainID string, days int, source, from string, log *slog.Logger) error {
	cfg, err := config.Load(configDir)
	if err != nil {
		return err
	}

	pg, err := store.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer pg.Close()

	switch cmd {
	case "load":
		asset := flag.Arg(1)
		if asset == "" {
			return fmt.Errorf("load needs an asset, e.g. TRX")
		}
		return loadPrices(ctx, pg, asset, days, source, from, chainID, log)

	case "backfill":
		// Reads every unpriced transfer: a batch job, which the 60 s
		// interactive read timeout cut off on 2026-09-23 (D32, D35).
		ch, err := store.OpenClickHouseBatch(ctx)
		if err != nil {
			return err
		}
		defer ch.Close()

		p := pricing.New(cfg, pg)
		res, err := p.Backfill(ctx, ch, store.NewTransferWriter(ch, pg), chainID, log)
		if err != nil {
			return err
		}

		fmt.Printf("scanned:  %d\n", res.Scanned)
		fmt.Printf("priced:   %d\n", res.Priced)
		fmt.Printf("unpriced: %d\n", res.Unpriced)
		for basis, n := range res.ByBasis {
			fmt.Printf("  %-12s %d\n", basis, n)
		}

		fmt.Printf("edges repaired: %d\n", res.Edges)
		return nil

	case "status":
		return status(ctx, pg)

	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// loadPrices fetches a daily close series and stores it.
//
// No API key is required and the rate limit is low, so this is a one-off
// backfill rather than anything on the query path.
func loadPrices(ctx context.Context, pg *sql.DB, asset string, days int,
	source, from, chainID string, log *slog.Logger) error {

	var (
		points []pricing.PricePoint
		err    error
	)

	switch source {
	case "binance":
		start, serr := resolveStart(ctx, from, asset, chainID)
		if serr != nil {
			return serr
		}
		log.Info("loading daily closes", "source", "binance", "asset", asset,
			"from", start.Format("2006-01-02"))
		points, err = fetchBinanceDailyCloses(ctx, asset, start)

	case "coingecko":
		log.Info("loading daily closes", "source", "coingecko", "asset", asset, "days", days)
		points, err = fetchDailyCloses(ctx, asset, days)

	default:
		return fmt.Errorf("unknown price source %q; use binance or coingecko", source)
	}
	if err != nil {
		return err
	}
	if len(points) == 0 {
		return fmt.Errorf("price source returned no points for %s", asset)
	}

	// The source actually used. Until 2026-09-23 every load was recorded as
	// coingecko, including the Binance history back to 2018 (D40).
	n, err := pricing.LoadPrices(ctx, pg, source, points)
	if err != nil {
		return err
	}

	log.Info("prices loaded", "asset", asset, "points", n)
	fmt.Printf("loaded %d daily closes for %s\n", n, asset)
	return nil
}

// resolveStart decides how far back to load.
//
// Defaulting to the earliest unpriced transfer we actually hold, rather than a
// fixed window, is what stops this being guesswork: loading a year of prices
// against four years of transfers leaves the remainder silently valued at zero
// and therefore invisible to traversal.
func resolveStart(ctx context.Context, from, asset, chainID string) (time.Time, error) {
	if from != "" {
		t, err := time.Parse("2006-01-02", from)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid -from date %q: %w", from, err)
		}
		return t, nil
	}

	ch, err := store.OpenClickHouseBatch(ctx)
	if err != nil {
		// Without the store we cannot look up the earliest transfer, so fall
		// back to a wide window rather than a narrow one. Loading too much
		// history is cheap; loading too little is invisible.
		return time.Now().AddDate(-5, 0, 0), nil
	}
	defer ch.Close()

	var earliest time.Time
	err = ch.QueryRowContext(ctx, `
		SELECT min(block_time) FROM transfers FINAL WHERE chain = ? AND asset = ?`,
		chainID, asset).Scan(&earliest)
	if err != nil || earliest.IsZero() || earliest.Year() < 2009 {
		return time.Now().AddDate(-5, 0, 0), nil
	}
	// A day earlier, so the earliest transfer's own day is covered.
	return earliest.AddDate(0, 0, -1), nil
}

// status reports what is priced and what is not, per asset.
//
// SPEC.md §7 turns on not hiding unknown exposure, and an asset with no price
// contributes no value at all — so knowing how much of the data that affects
// is part of reading any score honestly.
func status(ctx context.Context, pg *sql.DB) error {
	rows, err := pg.QueryContext(ctx, `
		SELECT asset, count(*), min(price_date), max(price_date)
		FROM prices GROUP BY asset ORDER BY asset`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Println("daily close series:")
	var any bool
	for rows.Next() {
		var asset string
		var n int
		var from, to time.Time
		if err := rows.Scan(&asset, &n, &from, &to); err != nil {
			return err
		}
		fmt.Printf("  %-6s %d days, %s to %s\n",
			asset, n, from.Format("2006-01-02"), to.Format("2006-01-02"))
		any = true
	}
	if !any {
		fmt.Println("  (none loaded; native-asset transfers will be unpriced)")
	}
	return rows.Err()
}

// fetchDailyCloses retrieves [date, price] pairs for an asset.
func fetchDailyCloses(ctx context.Context, asset string, days int) ([]pricing.PricePoint, error) {
	id, ok := coingeckoIDs[asset]
	if !ok {
		return nil, fmt.Errorf("no price source configured for asset %q", asset)
	}

	url := fmt.Sprintf(
		"https://api.coingecko.com/api/v3/coins/%s/market_chart?vs_currency=usd&days=%d&interval=daily",
		id, days)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch prices: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("price source returned %d: %s", resp.StatusCode, body)
	}

	var payload struct {
		Prices [][2]float64 `json:"prices"` // [unix_ms, price]
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode prices: %w", err)
	}

	// One point per day; later points for the same day win, which for a daily
	// series means the close.
	byDay := map[string]decimal.Decimal{}
	for _, pt := range payload.Prices {
		day := time.UnixMilli(int64(pt[0])).UTC().Format("2006-01-02")
		byDay[day] = decimal.NewFromFloat(pt[1])
	}

	out := make([]pricing.PricePoint, 0, len(byDay))
	for day, usd := range byDay {
		out = append(out, pricing.PricePoint{Asset: asset, Date: day, USD: usd})
	}
	return out, nil
}
