// Command indexer builds our own TRON index (docs/INDEXER_PLAN.md).
//
//	indexer measure [-sample 200] [-from 8000000]   read sample blocks, write nothing, project size and time
//	indexer init                                    create the index database and its schema
//	indexer tail [-start N]                         follow the solidified head
//	indexer backfill -from N -to M [-parts 8]       index a block range, resumably
//
// The index goes to its own ClickHouse database (-db, default tron_index), so
// the live tables are untouched until cutover.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/indexer"
	"github.com/mozer/tether-risk/internal/pricing"
	"github.com/mozer/tether-risk/internal/store"
	"github.com/mozer/tether-risk/pkg/tronindex"
)

func main() {
	var (
		source    = flag.String("source", "trongrid", "trongrid, alchemy, or a node's HTTP base URL (http://127.0.0.1:8090)")
		solid     = flag.Bool("solid", true, "read solidified blocks only (/walletsolidity)")
		rps       = flag.Float64("rps", 0, "requests per second; 0 picks a default for the source")
		db        = flag.String("db", "tron_index", "ClickHouse database the index is written to")
		fetchers  = flag.Int("fetchers", 4, "blocks fetched at once")
		batch     = flag.Int("batch", 50, "blocks written at once")
		sample    = flag.Int("sample", 200, "measure: blocks to sample")
		from      = flag.Uint64("from", 0, "first block (measure: default 8,000,000, where USDT on TRON begins)")
		to        = flag.Uint64("to", 0, "last block (default: the head)")
		start     = flag.Uint64("start", 0, "tail: first block when there is no cursor (default: the head)")
		parts     = flag.Int("parts", 8, "backfill: ranges run at once")
		configDir = flag.String("config", "config", "configuration directory")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	src, err := newSource(*source, *solid, *rps)
	if err != nil {
		fail(log, err)
	}
	switch flag.Arg(0) {
	case "measure":
		first := *from
		if first == 0 {
			first = 8_000_000
		}
		err = measure(ctx, src, first, *to, *sample, *fetchers)
	case "init":
		err = initDB(ctx, *db, log)
	case "tail", "backfill":
		err = run(ctx, flag.Arg(0), src, *db, *configDir, *from, *to, *start, *parts, *fetchers, *batch, log)
	default:
		fmt.Fprintln(os.Stderr, "usage: indexer [flags] measure | init | tail | backfill")
		os.Exit(2)
	}
	if err != nil && ctx.Err() == nil {
		fail(log, err)
	}
}

func fail(log *slog.Logger, err error) {
	log.Error("failed", "error", err)
	os.Exit(1)
}

// newSource picks the endpoint. Keys come from the environment only.
func newSource(name string, solid bool, rps float64) (*tronindex.NodeClient, error) {
	opts := tronindex.NodeOptions{Solidified: solid, RequestsPerSecond: rps}
	switch name {
	case "trongrid":
		opts.BaseURL = "https://api.trongrid.io"
		if k := os.Getenv("TRONGRID_API_KEY"); k != "" {
			opts.Header = map[string]string{"TRON-PRO-API-KEY": k}
		}
		if rps == 0 {
			opts.RequestsPerSecond = 2 // the worker shares the key (D35)
		}
	case "alchemy":
		// The same Alchemy app as Ethereum, with TRON Mainnet enabled on it.
		u := os.Getenv("TRON_RPC_URL")
		if u == "" {
			eth := os.Getenv("ETH_RPC_URL")
			if i := strings.LastIndex(eth, "/"); i >= 0 && eth != "" {
				u = "https://tron-mainnet.g.alchemy.com/v2" + eth[i:]
			}
		}
		if u == "" {
			return nil, fmt.Errorf("set TRON_RPC_URL or ETH_RPC_URL for the alchemy source")
		}
		opts.BaseURL = u
		if rps == 0 {
			opts.RequestsPerSecond = 20
		}
	default:
		opts.BaseURL = name // a local node needs no limit
	}
	return tronindex.NewNodeClient(opts), nil
}

// measure reads blocks spread evenly from..to and projects the index's size
// and the backfill's duration. It writes nothing.
func measure(ctx context.Context, src *tronindex.NodeClient, from, to uint64, n, fetchers int) error {
	head, err := src.Head(ctx)
	if err != nil {
		return err
	}
	if to == 0 || to > head {
		to = head
	}
	if n < 1 || from >= to {
		return fmt.Errorf("nothing to sample between %d and %d", from, to)
	}
	ix := &tronindex.Indexer{Source: src, Config: indexer.Config(), Fetchers: fetchers}
	type era struct {
		blocks, txs, transfers, events int
		byAsset                        map[string]int
		secs                           float64
	}
	eras := map[string]*era{}
	step := (to - from) / uint64(n)
	for i := 0; i < n && ctx.Err() == nil; i++ {
		b := from + uint64(i)*step
		t0 := time.Now()
		raw, err := src.Block(ctx, b)
		if err != nil {
			return err
		}
		bd, err := ix.Fetch(ctx, b)
		if err != nil {
			return err
		}
		secs := time.Since(t0).Seconds()
		y := bd.Time.Format("2006")
		e := eras[y]
		if e == nil {
			e = &era{byAsset: map[string]int{}}
			eras[y] = e
		}
		e.blocks++
		e.txs += len(raw.Transactions)
		e.transfers += len(bd.Transfers)
		e.events += len(bd.Events)
		e.secs += secs
		for _, t := range bd.Transfers {
			a := "TRX"
			if t.Token != "" {
				a = shortAsset(t.Token)
			}
			e.byAsset[a]++
		}
		if (i+1)%25 == 0 {
			fmt.Fprintf(os.Stderr, "  %d/%d blocks\n", i+1, n)
		}
	}

	years := make([]string, 0, len(eras))
	for y := range eras {
		years = append(years, y)
	}
	sort.Strings(years)
	fmt.Printf("\nblocks %d..%d (%d), %d sampled\n\n", from, to, to-from, n)
	fmt.Printf("%-6s %7s %9s %11s %8s %9s  %s\n", "year", "blocks", "tx/block", "kept/block", "events", "s/block", "by asset per block")
	var perBlock, secsPerBlock float64
	var total int
	for _, y := range years {
		e := eras[y]
		var assets []string
		for a, c := range e.byAsset {
			assets = append(assets, fmt.Sprintf("%s %.1f", a, float64(c)/float64(e.blocks)))
		}
		sort.Strings(assets)
		fmt.Printf("%-6s %7d %9.1f %11.1f %8d %9.2f  %s\n", y, e.blocks, float64(e.txs)/float64(e.blocks),
			float64(e.transfers)/float64(e.blocks), e.events, e.secs/float64(e.blocks), strings.Join(assets, ", "))
		perBlock += float64(e.transfers)
		secsPerBlock += e.secs
		total += e.blocks
	}
	perBlock /= float64(total)
	secsPerBlock /= float64(total)
	rows := perBlock * float64(to-from)
	bytesPerRow := storedBytesPerTransfer(ctx)
	fmt.Printf("\nprojected: %.2f billion transfers, about %.0f GB in ClickHouse at %.0f bytes a transfer (this database's own ratio, edges included)\n",
		rows/1e9, rows*bytesPerRow/1e9, bytesPerRow)
	fmt.Printf("backfill at the sampled speed with %d fetchers: about %.1f days (%.2f s a block, sequential, from this endpoint)\n",
		fetchers, float64(to-from)*secsPerBlock/float64(fetchers)/86400, secsPerBlock)
	return nil
}

// storedBytesPerTransfer is what one transfer costs on disk today: the
// transfers tables and the edge tables together, over the transfer rows.
func storedBytesPerTransfer(ctx context.Context) float64 {
	ch, err := store.OpenClickHouse(ctx)
	if err != nil {
		return 290
	}
	defer ch.Close()
	var bytes, rows float64
	if err := ch.QueryRowContext(ctx, `
		SELECT sumIf(bytes_on_disk, table IN ('transfers','transfers_by_to','edges','edges_by_to')),
		       sumIf(rows, table = 'transfers')
		FROM system.parts WHERE active AND database = currentDatabase()`).Scan(&bytes, &rows); err != nil || rows == 0 {
		return 290
	}
	return bytes / rows
}

func shortAsset(token string) string {
	for contract, name := range map[string]string{
		"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t": "USDT", "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8": "USDC",
		"TUpMhErZL2fhh4sVNULAbNKLokS4GjC1F4": "TUSD", "TXDk8mbtRbXeYuMNS83CfKPaYYT8XWv9Hz": "USDD",
		"TPYmHEhy5n8TCEfYGqW2rPxsghSfzghPDn": "USDD"} {
		if token == contract {
			return name
		}
	}
	return "other"
}

// initDB creates the index database and applies the ClickHouse schema to it.
func initDB(ctx context.Context, db string, log *slog.Logger) error {
	admin, err := store.OpenClickHouse(ctx)
	if err != nil {
		return err
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+db); err != nil {
		admin.Close()
		return err
	}
	admin.Close()
	os.Setenv("CLICKHOUSE_DB", db)
	ch, err := store.OpenClickHouse(ctx)
	if err != nil {
		return err
	}
	defer ch.Close()
	return store.MigrateClickHouse(ctx, ch, log)
}

func run(ctx context.Context, mode string, src *tronindex.NodeClient, db, configDir string,
	from, to, start uint64, parts, fetchers, batch int, log *slog.Logger) error {
	cfg, err := config.Load(configDir)
	if err != nil {
		return err
	}
	pg, err := store.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer pg.Close()
	os.Setenv("CLICKHOUSE_DB", db)
	ch, err := store.OpenClickHouseBatch(ctx)
	if err != nil {
		return err
	}
	defer ch.Close()

	ix := &tronindex.Indexer{
		Source: src, Cursors: indexer.Cursors{PG: pg}, Config: indexer.Config(),
		Sink:     &indexer.Sink{CH: ch, Writer: store.NewTransferWriter(ch, pg), Pricer: pricing.New(cfg, pg)},
		Fetchers: fetchers, Batch: batch, Log: log,
		OnBatch: func(bs []tronindex.BlockData, took time.Duration) {
			var n int
			for _, b := range bs {
				n += len(b.Transfers)
			}
			log.Info("batch written", "through", bs[len(bs)-1].Number, "blocks", len(bs), "transfers", n,
				"blocks_per_s", fmt.Sprintf("%.1f", float64(len(bs))/took.Seconds()))
		},
	}
	switch mode {
	case "tail":
		if start == 0 {
			if start, err = src.Head(ctx); err != nil {
				return err
			}
		}
		return ix.Tail(ctx, "tail", start)
	default:
		if to == 0 {
			if to, err = src.Head(ctx); err != nil {
				return err
			}
		}
		if from == 0 || from > to {
			return fmt.Errorf("backfill needs -from below -to (%d)", to)
		}
		return ix.Backfill(ctx, from, to, parts)
	}
}
