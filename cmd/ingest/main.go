// Command ingest runs chain ingestion workers and one-off fetch operations.
//
//	ingest worker                 drain the fetch queue
//	ingest fetch <address>        fetch one address now, reporting timings
//	ingest rebuild-edges          recompute edges from transfers
//	ingest stats                  queue and storage counts
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mozer/tether-risk/internal/chain"
	"github.com/mozer/tether-risk/internal/chain/evm"
	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/ingest"
	"github.com/mozer/tether-risk/internal/store"
)

func main() {
	var (
		workers   = flag.Int("workers", 4, "concurrent workers")
		depth     = flag.Int("depth", 1, "neighbour hop depth to expand on fetch")
		ttl       = flag.Duration("ttl", 24*time.Hour, "how long stored history stays fresh")
		configDir = flag.String("config", "config", "configuration directory")
		chainID   = flag.String("chain", "tron", "chain to ingest")
		verbose   = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := flag.Arg(0)
	if cmd == "" {
		usage()
		os.Exit(2)
	}

	if err := run(ctx, cmd, *configDir, *chainID, *workers, *depth, *ttl, log); err != nil {
		log.Error("failed", "command", cmd, "error", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: ingest [flags] <command>

commands:
  worker                drain the fetch queue until interrupted
  fetch <address>       fetch one address now and report timings
  rebuild-edges         recompute edges from deduplicated transfers
  stats                 queue and storage counts

flags:
`)
	flag.PrintDefaults()
}

func run(ctx context.Context, cmd, configDir, chainID string, workers, depth int, ttl time.Duration, log *slog.Logger) error {
	cfg, err := config.Load(configDir)
	if err != nil {
		return err
	}

	// SPEC.md §6.2 and docs/PLAN.md F1: a chain without a live data path must
	// say so rather than return an empty result that reads as "no activity".
	chainCfg, ok := cfg.Chain(chainID)
	if !ok {
		return fmt.Errorf("chain %s is not declared in sources.yaml", chainID)
	}
	if !chainCfg.Available() {
		return fmt.Errorf("chain %s is unavailable: %s", chainID, chainCfg.UnavailableReason)
	}

	ch, err := store.OpenClickHouse(ctx)
	if err != nil {
		return err
	}
	defer ch.Close()

	pg, err := store.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer pg.Close()

	jobs := store.NewJobs(pg)
	writer := store.NewTransferWriter(ch, pg)

	adapter, err := buildAdapter(chainID, chainCfg, log)
	if err != nil {
		return err
	}

	opts := ingest.Options{TTL: ttl, Logger: log}

	switch cmd {
	case "worker":
		return runWorkers(ctx, workers, adapter, jobs, writer, opts, log)

	case "fetch":
		address := flag.Arg(1)
		if address == "" {
			return fmt.Errorf("fetch needs an address")
		}
		return fetchOne(ctx, address, depth, adapter, jobs, writer, opts, log)

	case "rebuild-edges":
		started := time.Now()
		if err := writer.RebuildEdges(ctx); err != nil {
			return err
		}
		log.Info("edges rebuilt from deduplicated transfers", "duration", time.Since(started))
		return nil

	case "stats":
		return printStats(ctx, ch, jobs)

	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// buildAdapter selects the adapter for a chain.
//
// EVM chains prefer Alchemy's per-address transfers API when a key is
// configured, because raw JSON-RPC has no per-address history call and the
// archive scan that would substitute for one is not served by any public
// endpoint (see internal/chain/evm/client.go). Without a key the chain is
// declared unavailable in sources.yaml and never reaches here.
func buildAdapter(chainID string, cfg config.ChainSource, log *slog.Logger) (chain.Adapter, error) {
	switch chainID {
	case "tron":
		return tron.NewAdapter(tron.NewClient(tron.Options{
			BaseURL:           cfg.URL,
			APIKey:            os.Getenv("TRONGRID_API_KEY"), // optional; no secrets in the repo
			RequestsPerSecond: float64(cfg.RateLimitPerSec),
			Logger:            log,
		})), nil

	case "ethereum", "bsc":
		envVar := "ETH_RPC_URL"
		if chainID == "bsc" {
			envVar = "BSC_RPC_URL"
		}
		url := os.Getenv(envVar)
		if url == "" {
			url = cfg.URL
		}
		if url == "" {
			return nil, fmt.Errorf("no endpoint for %s; set %s", chainID, envVar)
		}

		client := evm.NewClient(evm.Options{
			URL:               url,
			RequestsPerSecond: float64(cfg.RateLimitPerSec),
			Logger:            log,
		})

		if strings.Contains(url, "alchemy.com") {
			log.Info("using the alchemy per-address transfers API", "chain", chainID)
			return evm.NewAlchemyAdapter(client, evm.AlchemyOptions{ChainID: chainID}), nil
		}

		// A non-Alchemy endpoint still gives a working block-range adapter,
		// but demand-driven per-address ingestion will fail with a typed
		// archive error rather than returning a misleadingly empty history.
		log.Warn("endpoint is not Alchemy; per-address ingestion will report "+
			"that archive access is required", "chain", chainID, "url_host", hostOf(url))
		return evm.NewAdapter(client, evm.AdapterOptions{ChainID: chainID}), nil

	default:
		return nil, fmt.Errorf("no adapter for chain %q", chainID)
	}
}

func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return "unknown"
}

func runWorkers(ctx context.Context, n int, adapter chain.Adapter, jobs *store.Jobs,
	writer *store.TransferWriter, opts ingest.Options, log *slog.Logger) error {

	// Reclaim leases stranded by a previous crash before taking new work.
	if n, err := jobs.ReclaimExpired(ctx); err != nil {
		log.Warn("reclaiming expired leases failed", "error", err)
	} else if n > 0 {
		log.Info("reclaimed stranded jobs", "count", n)
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := ingest.NewWorker(fmt.Sprintf("worker-%d", i), adapter, jobs, writer, opts)
			if err := w.Run(ctx); err != nil && ctx.Err() == nil {
				log.Error("worker exited", "worker", i, "error", err)
			}
		}(i)
	}

	// Periodically reclaim leases from workers that died mid-job.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := jobs.ReclaimExpired(ctx); err == nil && n > 0 {
					log.Info("reclaimed expired leases", "count", n)
				}
			}
		}
	}()

	wg.Wait()
	return nil
}

func fetchOne(ctx context.Context, address string, depth int, adapter chain.Adapter,
	jobs *store.Jobs, writer *store.TransferWriter, opts ingest.Options, log *slog.Logger) error {

	w := ingest.NewWorker("cli", adapter, jobs, writer, opts)

	res, err := w.FetchAddress(ctx, address, depth)
	if err != nil {
		return err
	}

	// SPEC.md §5 acceptance: a cold query for a thousand-transfer address
	// completes in under 30 seconds, a warm one in under 2. Reported here so
	// the gate is measured rather than asserted.
	fmt.Printf("address:     %s\n", res.Address)
	fmt.Printf("pages:       %d\n", res.Pages)
	fmt.Printf("fetched:     %d\n", res.Fetched)
	fmt.Printf("inserted:    %d\n", res.Inserted)
	fmt.Printf("duplicates:  %d\n", res.Duplicates)
	fmt.Printf("neighbours:  %d queued\n", res.Neighbours)
	fmt.Printf("truncated:   %v\n", res.Truncated)
	fmt.Printf("cache hit:   %v\n", res.Skipped)
	fmt.Printf("duration:    %s\n", res.Duration.Round(time.Millisecond))
	return nil
}

func printStats(ctx context.Context, ch *sql.DB, jobs *store.Jobs) error {
	stats, err := jobs.QueueStats(ctx)
	if err != nil {
		return err
	}
	fmt.Println("queue:")
	for _, state := range []string{"pending", "running", "done", "failed", "abandoned"} {
		fmt.Printf("  %-10s %d\n", state, stats[state])
	}

	fmt.Println("storage:")
	// FINAL on transfers, and an explicit aggregate on edges: both engines
	// merge in the background, so a naive count reports whatever happens to be
	// unmerged right now (docs/DECISIONS.md D8).
	for _, q := range []struct{ label, query string }{
		{"transfers", "SELECT count() FROM transfers FINAL"},
		{"edges", "SELECT count() FROM (SELECT 1 FROM edges GROUP BY chain, from_address, to_address, asset)"},
		{"addresses", "SELECT uniqExact(addr) FROM (SELECT from_address AS addr FROM transfers UNION ALL SELECT to_address FROM transfers)"},
	} {
		var n uint64
		if err := ch.QueryRowContext(ctx, q.query).Scan(&n); err != nil {
			return fmt.Errorf("count %s: %w", q.label, err)
		}
		fmt.Printf("  %-10s %d\n", q.label, n)
	}
	return nil
}
