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
var coingeckoIDs = map[string]string{
	"TRX": "tron",
	"ETH": "ethereum",
	"BNB": "binancecoin",
}

func main() {
	var (
		configDir = flag.String("config", "config", "configuration directory")
		chainID   = flag.String("chain", "tron", "chain to reprice")
		days      = flag.Int("days", 365, "days of price history to load")
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

	if err := run(ctx, cmd, *configDir, *chainID, *days, log); err != nil {
		log.Error("failed", "command", cmd, "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd, configDir, chainID string, days int, log *slog.Logger) error {
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
		return loadPrices(ctx, pg, asset, days, log)

	case "backfill":
		ch, err := store.OpenClickHouse(ctx)
		if err != nil {
			return err
		}
		defer ch.Close()

		p := pricing.New(cfg, pg)
		res, err := p.Backfill(ctx, ch, chainID, log)
		if err != nil {
			return err
		}

		fmt.Printf("scanned:  %d\n", res.Scanned)
		fmt.Printf("priced:   %d\n", res.Priced)
		fmt.Printf("unpriced: %d\n", res.Unpriced)
		for basis, n := range res.ByBasis {
			fmt.Printf("  %-12s %d\n", basis, n)
		}

		// The edge aggregates were computed from the old values, and a
		// materialized view cannot retroactively revise what it already
		// summed (docs/DECISIONS.md D2). Rebuilding is not optional here.
		log.Info("rebuilding edges from repriced transfers")
		w := store.NewTransferWriter(ch, pg)
		if err := w.RebuildEdges(ctx); err != nil {
			return err
		}
		log.Info("edges rebuilt")
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
func loadPrices(ctx context.Context, pg *sql.DB, asset string, days int, log *slog.Logger) error {
	points, err := fetchDailyCloses(ctx, asset, days)
	if err != nil {
		return err
	}
	if len(points) == 0 {
		return fmt.Errorf("price source returned no points for %s", asset)
	}

	n, err := pricing.LoadPrices(ctx, pg, "coingecko", points)
	if err != nil {
		return err
	}

	log.Info("prices loaded", "asset", asset, "points", n)
	fmt.Printf("loaded %d daily closes for %s\n", n, asset)
	return nil
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
