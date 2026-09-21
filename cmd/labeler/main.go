// Command labeler ingests label sources and derives labels from chain data.
//
//	labeler ingest            run every permitted source into a new snapshot
//	labeler derive            run the deposit-wallet heuristic
//	labeler counts            per-source label counts for the latest snapshot
//	labeler conflicts         unreviewed category conflicts
//
// SPEC.md §6: each ingestion job is idempotent and writes its own `source`.
// Sources whose terms prohibit redistribution are not ingested at all; see
// config/sources.yaml and docs/PLAN.md flag F2.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/labels"
	"github.com/mozer/tether-risk/internal/store"
)

func main() {
	var (
		configDir = flag.String("config", "config", "configuration directory")
		chainID   = flag.String("chain", "tron", "chain to derive labels for")
		ofacFile  = flag.String("ofac-file", "", "use a local SDN XML file instead of downloading")
		verbose   = flag.Bool("v", false, "debug logging")
		fromEx    = flag.String("from-exchange", "", "trace-tx: exchange the test withdrawal was made from")
		toEx      = flag.String("to-exchange", "", "trace-tx: exchange the deposit address belongs to")
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

	if cmd == "trace-tx" {
		// Needs only the chain API, not the label store.
		cfg, err := config.Load(*configDir)
		if err == nil {
			if flag.Arg(1) == "" {
				err = fmt.Errorf("trace-tx needs a transaction id")
			} else {
				err = traceTx(ctx, cfg, *chainID, flag.Arg(1), *fromEx, *toEx, log)
			}
		}
		if err != nil {
			log.Error("failed", "command", cmd, "error", err)
			os.Exit(1)
		}
		return
	}

	if err := run(ctx, cmd, *configDir, *chainID, *ofacFile, log); err != nil {
		log.Error("failed", "command", cmd, "error", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: labeler [flags] <command>

commands:
  ingest       run every permitted source into a new snapshot
  derive-services  detect high-volume service addresses from behaviour
  derive       run the deposit-wallet heuristic against stored chain data
  counts       per-source label counts for the latest sealed snapshot
  conflicts    unreviewed category conflicts
  trace-tx <txid>  follow a controlled test transfer to its hot wallets;
                   prints a curated_labels snippet for review, writes nothing

flags:
`)
	flag.PrintDefaults()
}

func run(ctx context.Context, cmd, configDir, chainID, ofacFile string, log *slog.Logger) error {
	cfg, err := config.Load(configDir)
	if err != nil {
		return err
	}

	pg, err := store.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer pg.Close()

	st := labels.NewStore(pg)
	resolver := labels.NewResolver(cfg)

	switch cmd {
	case "ingest":
		return ingest(ctx, cfg, st, resolver, configDir, ofacFile, log)
	case "derive-services":
		return deriveServices(ctx, cfg, st, chainID, log)
	case "derive":
		return derive(ctx, cfg, st, chainID, log)
	case "counts":
		return counts(ctx, st)
	case "conflicts":
		return conflicts(ctx, st)
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func ingest(ctx context.Context, cfg *config.Config, st *labels.Store, resolver *labels.Resolver,
	configDir, ofacFile string, log *slog.Logger) error {

	snapshotID, err := st.OpenSnapshot(ctx, "labeler ingest")
	if err != nil {
		return err
	}
	log.Info("snapshot opened", "id", snapshotID)

	var total labels.UpsertResult

	// --- OFAC ---
	// SPEC.md §9.1 makes a sanctions miss build-breaking, so this source
	// failing fails the whole run rather than being logged and skipped.
	if src, ok := cfg.Source("ofac"); ok && src.Ingestible() {
		batch, err := ingestOFAC(ctx, src.URL, ofacFile, log)
		if err != nil {
			return fmt.Errorf("ofac ingestion failed, and a sanctions gap is not survivable: %w", err)
		}
		res, err := st.Upsert(ctx, snapshotID, batch)
		if err != nil {
			return err
		}
		log.Info("ofac ingested", "labels", len(batch),
			"inserted", res.Inserted, "updated", res.Updated, "unchanged", res.Unchanged)
		total = add(total, res)
	}

	// --- curated ---
	if src, ok := cfg.Source("curated"); ok && src.Ingestible() {
		path := src.Path
		if path == "" {
			path = configDir + "/curated_labels.yaml"
		}
		batch, err := labels.LoadCurated(path, resolver.ValidateCategory)
		if err != nil {
			return err
		}
		res, err := st.Upsert(ctx, snapshotID, batch)
		if err != nil {
			return err
		}
		log.Info("curated ingested", "labels", len(batch), "inserted", res.Inserted)
		total = add(total, res)
	}

	// --- abuse feeds ---
	// SPEC.md §6.4: unverified user reports at confidence 0.5. The resolver
	// enforces that a lone report cannot reach a high band; this only records
	// what each source claims.
	for _, feed := range []struct {
		id    string
		url   string
		parse func(context.Context, io.Reader) (*labels.AbuseResult, []labels.Label, error)
	}{
		{"scamsniffer", "https://raw.githubusercontent.com/scamsniffer/scam-database/main/blacklist/address.json", labels.ParseScamSniffer},
		{"cryptoscamdb", "https://raw.githubusercontent.com/CryptoScamDB/blacklist/master/data/urls.yaml", labels.ParseCryptoScamDB},
	} {
		src, ok := cfg.Source(feed.id)
		if !ok || !src.Ingestible() {
			continue
		}

		batch, ares, err := ingestAbuse(ctx, feed.url, feed.parse, log)
		if err != nil {
			// An abuse feed is useful but not load-bearing. Unlike OFAC, its
			// absence reduces coverage rather than creating a sanctions gap,
			// so the run continues and says what was lost.
			log.Error("abuse feed failed; its labels are missing from this snapshot",
				"source", feed.id, "error", err)
			continue
		}

		res, err := st.Upsert(ctx, snapshotID, batch)
		if err != nil {
			return err
		}
		log.Info("abuse feed ingested", "source", feed.id,
			"entries", ares.EntriesScanned, "addresses", ares.AddressesFound,
			"labels", len(batch), "inserted", res.Inserted)
		for _, chainID := range sortedKeys(ares.ByChain) {
			log.Info("  by chain", "source", feed.id, "chain", chainID, "labels", ares.ByChain[chainID])
		}
		total = add(total, res)
	}

	// --- exchange proof-of-reserves lists (docs/DECISIONS.md D19) ---
	// Like the abuse feeds, not load-bearing: a missing list reduces coverage
	// rather than opening a sanctions gap, so the run continues and says so.
	for _, por := range []struct{ id, exchange string }{
		{"htx_por", "HTX"},
		{"poloniex_por", "Poloniex"},
	} {
		src, ok := cfg.Source(por.id)
		if !ok || !src.Ingestible() {
			continue
		}
		batch, pres, err := ingestPoR(ctx, src.URL, por.exchange, por.id, src.Confidence)
		if err != nil {
			log.Error("proof-of-reserves list failed; its labels are missing from this snapshot",
				"source", por.id, "error", err)
			continue
		}
		for _, m := range pres.Malformed {
			log.Warn("proof-of-reserves row could not be parsed", "source", por.id, "detail", m)
		}
		res, err := st.Upsert(ctx, snapshotID, batch)
		if err != nil {
			return err
		}
		log.Info("proof-of-reserves list ingested", "source", por.id,
			"tron_addresses", pres.Addresses, "signed", pres.Signed, "inserted", res.Inserted)
		total = add(total, res)
	}

	// --- sources declared but not yet implemented ---
	// Named explicitly rather than passed over in silence: a source that is
	// configured as permitted but contributes nothing is a coverage gap, and
	// SPEC.md §7 is emphatic that gaps are shown rather than hidden.
	for _, id := range []string{"un", "eu"} {
		if src, ok := cfg.Source(id); ok && src.Ingestible() {
			log.Warn("source permitted but not yet implemented; it contributes no labels "+
				"and its absence reduces coverage", "source", id)
		}
	}
	for _, id := range []string{"etherscan", "bscscan", "tronscan", "chainabuse", "dune", "binance_por", "okx_por"} {
		if src, ok := cfg.Source(id); ok && !src.Ingestible() {
			log.Info("source deliberately not ingested",
				"source", id, "status", src.Status)
		}
	}

	counts, err := st.SealSnapshot(ctx, snapshotID)
	if err != nil {
		return err
	}

	fmt.Printf("snapshot %d sealed\n", snapshotID)
	fmt.Printf("  inserted %d, updated %d, unchanged %d\n",
		total.Inserted, total.Updated, total.Unchanged)
	fmt.Println("label counts by source:")
	for _, src := range sortedKeys(counts) {
		fmt.Printf("  %-18s %d\n", src, counts[src])
	}
	return nil
}

func ingestOFAC(ctx context.Context, url, localFile string, log *slog.Logger) ([]labels.Label, error) {
	var body io.ReadCloser

	if localFile != "" {
		f, err := os.Open(localFile)
		if err != nil {
			return nil, err
		}
		body = f
	} else {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		client := &http.Client{Timeout: 5 * time.Minute}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("download SDN list: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("download SDN list: status %d", resp.StatusCode)
		}
		body = resp.Body
	}
	defer body.Close()

	res, out, err := labels.ParseOFAC(ctx, body)
	if err != nil {
		return nil, err
	}

	log.Info("ofac parsed",
		"entries", res.EntriesScanned, "addresses", res.AddressesFound,
		"labels", res.LabelsProduced, "malformed", len(res.Malformed))

	// A sanctioned address we could not parse is a potential miss. It does not
	// fail the run on its own — the other 99% of the list is still worth
	// having — but it must be impossible to overlook in the output.
	for _, m := range res.Malformed {
		log.Error("sanctioned address could not be parsed; it will NOT be screened", "detail", m)
	}
	for _, code := range sortedKeys(res.SkippedOffChain) {
		log.Info("skipped out-of-scope chain", "currency", code, "addresses", res.SkippedOffChain[code])
	}
	return out, nil
}

// ingestAbuse downloads and parses one abuse feed.
func ingestAbuse(ctx context.Context, url string,
	parse func(context.Context, io.Reader) (*labels.AbuseResult, []labels.Label, error),
	log *slog.Logger) ([]labels.Label, *labels.AbuseResult, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	res, out, err := parse(ctx, resp.Body)
	if err != nil {
		return nil, nil, err
	}
	for _, m := range res.Malformed {
		log.Warn("abuse feed entry could not be parsed", "detail", m)
	}
	return out, res, nil
}

func ingestPoR(ctx context.Context, url, exchange, sourceID string, confidence float64) ([]labels.Label, *labels.PoRResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	res, out, err := labels.ParsePoRCSV(ctx, resp.Body, exchange, sourceID, url, confidence)
	if err != nil {
		return nil, nil, err
	}
	return out, res, nil
}

// deriveServices detects high-volume service addresses from behaviour.
//
// Candidates are the busiest addresses already in the edge table, by
// counterparty count. Sampling costs an API call per page, so the candidate
// set is bounded and ordered by how likely each is to be a service.
func deriveServices(ctx context.Context, cfg *config.Config, st *labels.Store,
	chainID string, log *slog.Logger) error {

	ch, err := store.OpenClickHouse(ctx)
	if err != nil {
		return err
	}
	defer ch.Close()

	chainCfg, ok := cfg.Chain(chainID)
	if !ok || !chainCfg.Available() {
		return fmt.Errorf("chain %s has no live data path, so behaviour cannot be sampled", chainID)
	}

	// Rank by distinct counterparties rather than by value: a service is
	// defined by breadth, and a single large transfer says nothing about it.
	rows, err := ch.QueryContext(ctx, `
		SELECT addr, uniqExact(cp) AS counterparties
		FROM (
			SELECT from_address AS addr, to_address AS cp FROM edges_current WHERE chain = ?
			UNION ALL
			SELECT to_address AS addr, from_address AS cp FROM edges_current WHERE chain = ?
		)
		GROUP BY addr
		HAVING counterparties >= 5
		ORDER BY counterparties DESC, addr ASC
		LIMIT 200`, chainID, chainID)
	if err != nil {
		return fmt.Errorf("select candidates: %w", err)
	}
	defer rows.Close()

	var candidates []string
	for rows.Next() {
		var addr string
		var n uint64
		if err := rows.Scan(&addr, &n); err != nil {
			return err
		}
		candidates = append(candidates, addr)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(candidates) == 0 {
		fmt.Println("no candidates: ingest some addresses first")
		return nil
	}
	log.Info("sampling candidates", "count", len(candidates), "chain", chainID)

	sampler := labels.NewTronSampler(chainCfg.URL, os.Getenv("TRONGRID_API_KEY"), float64(chainCfg.RateLimitPerSec))
	judged, derived, err := labels.DetectServices(ctx, sampler, cfg, chainID, candidates)
	if err != nil {
		return err
	}

	fmt.Printf("candidates sampled: %d\n", len(judged))
	cost := sampler.Stats()
	fmt.Printf("requests: %d, retries: %d, rate limited: %d, waiting: %s\n",
		cost.Requests, cost.Retries, cost.RateLimited, cost.Waited.Round(time.Second))
	log.Info("sampling cost", "requests", cost.Requests, "retries", cost.Retries,
		"rate_limited", cost.RateLimited, "waited", cost.Waited.Round(time.Second))
	fmt.Printf("detected services:  %d\n", labels.AcceptedCount(judged))

	for _, c := range judged {
		if c.Accepted {
			fmt.Printf("  SERVICE  %s  (%d transfers, %d counterparties, more=%v)\n",
				c.Address, c.Sample.Transfers, c.Sample.Counterparties, c.Sample.MorePages)
		}
	}

	if len(derived) == 0 {
		return nil
	}

	snap, err := st.OpenSnapshot(ctx, "derived service detection")
	if err != nil {
		return err
	}
	res, err := st.Upsert(ctx, snap, derived)
	if err != nil {
		return err
	}
	counts, err := st.SealSnapshot(ctx, snap)
	if err != nil {
		return err
	}

	log.Info("service labels written", "snapshot", snap,
		"inserted", res.Inserted, "updated", res.Updated, "unchanged", res.Unchanged)
	fmt.Printf("snapshot %d sealed\n", snap)
	for _, src := range sortedKeys(counts) {
		fmt.Printf("  %-18s %d\n", src, counts[src])
	}
	return nil
}

func derive(ctx context.Context, cfg *config.Config, st *labels.Store, chainID string, log *slog.Logger) error {
	ch, err := store.OpenClickHouse(ctx)
	if err != nil {
		return err
	}
	defer ch.Close()

	snapshotID, err := st.LatestSealedSnapshot(ctx)
	if err != nil {
		return err
	}

	// The heuristic anchors on known exchange hot wallets. This map was once
	// left empty here, so the heuristic could never fire whatever the label
	// set held (docs/DECISIONS.md D20).
	exchangeLabels, err := st.ByCategories(ctx, snapshotID, chainID, labels.DepositAnchorCategories)
	if err != nil {
		return err
	}
	known := labels.DepositAnchors(exchangeLabels)
	log.Info("deposit anchors loaded", "snapshot", snapshotID, "hot_wallets", len(known))
	if len(known) == 0 {
		// Not a failure of this run: there is simply nothing to anchor on
		// yet. Said plainly, and without a non-zero exit, so the scheduled
		// daily run does not report a failure every night until the first
		// hot wallet is curated.
		log.Warn("no exchange hot wallets are labelled yet, so no deposit wallets can be derived; " +
			"add verified hot wallets to config/curated_labels.yaml")
		fmt.Println("skipped: no exchange hot wallets labelled")
		return nil
	}

	candidates, derived, err := labels.DeriveDeposits(ctx, ch, cfg, known, chainID)
	if err != nil {
		return err
	}

	var accepted int
	for _, c := range candidates {
		if c.Accepted {
			accepted++
		}
	}
	fmt.Printf("candidates examined: %d\n", len(candidates))
	fmt.Printf("accepted:            %d\n", accepted)
	fmt.Printf("rejected:            %d\n", len(candidates)-accepted)

	if len(derived) > 0 {
		snap, err := st.OpenSnapshot(ctx, "derived deposit wallets")
		if err != nil {
			return err
		}
		res, err := st.Upsert(ctx, snap, derived)
		if err != nil {
			return err
		}
		if _, err := st.SealSnapshot(ctx, snap); err != nil {
			return err
		}
		log.Info("derived labels written", "snapshot", snap, "inserted", res.Inserted)
	}
	return nil
}

func counts(ctx context.Context, st *labels.Store) error {
	id, err := st.LatestSealedSnapshot(ctx)
	if err != nil {
		return err
	}
	c, err := st.CountsBySource(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("snapshot %d\n", id)
	if len(c) == 0 {
		fmt.Println("  (no labels)")
		return nil
	}
	var total int
	for _, src := range sortedKeys(c) {
		fmt.Printf("  %-18s %d\n", src, c[src])
		total += c[src]
	}
	fmt.Printf("  %-18s %d\n", "TOTAL", total)
	return nil
}

func conflicts(ctx context.Context, st *labels.Store) error {
	list, err := st.UnreviewedConflicts(ctx, 50)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("no unreviewed conflicts")
		return nil
	}
	fmt.Printf("%d unreviewed conflicts:\n", len(list))
	for _, c := range list {
		fmt.Printf("  %s/%s resolved to %s (%s)\n", c.Chain, c.Address, c.ResolvedCategory, c.ResolvedSource)
		for _, comp := range c.Competing {
			fmt.Printf("      competing: %s says %s (%.2f)\n", comp.Source, comp.Category, comp.Confidence)
		}
	}
	return nil
}

func add(a, b labels.UpsertResult) labels.UpsertResult {
	return labels.UpsertResult{
		Inserted:  a.Inserted + b.Inserted,
		Updated:   a.Updated + b.Updated,
		Unchanged: a.Unchanged + b.Unchanged,
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
