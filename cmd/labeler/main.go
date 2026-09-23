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
	"database/sql"
	"flag"
	"fmt"
	ingestpkg "github.com/mozer/tether-risk/internal/ingest"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/mozer/tether-risk/internal/chain/tron"
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
  tether       refresh Tether's blacklist only, and make watches touching a newly frozen address due
  derive-services  detect high-volume service addresses from behaviour
  derive       run the deposit-wallet heuristic against stored chain data
  activations  read who created each labelled service wallet (for operator grouping)
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
	// Every TronGrid request counts against the key's daily quota (D35).
	ingestpkg.TrackTronUsage(ctx, pg, log)
	defer ingestpkg.FlushTronUsage(context.WithoutCancel(ctx), pg, log)

	st := labels.NewStore(pg)
	resolver := labels.NewResolver(cfg)

	switch cmd {
	case "ingest":
		return ingest(ctx, cfg, st, resolver, configDir, ofacFile, log)
	case "derive-services":
		return deriveServices(ctx, cfg, st, chainID, log)
	case "derive":
		return derive(ctx, cfg, pg, st, chainID, log)
	case "tether":
		return tetherRefresh(ctx, cfg, pg, st, log)
	case "activations":
		return fetchActivations(ctx, cfg, pg, chainID, log)
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
			// absence is no sanctions gap, so the run continues. Nothing
			// retires the feed's labels, so the previous ones stand; the
			// message said they were missing until 2026-09-23, when 2,987
			// were verified still current after a failed download.
			log.Error("abuse feed failed; the previous snapshot's labels stand",
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

	// --- UK and EU sanctions (docs/DECISIONS.md D34) ---
	// Addresses OFAC lacks, for Russian exchanges among others. A failure is
	// logged loudly and the previous snapshot's labels stand; OFAC above is
	// the list whose failure fails the run.
	for _, list := range []struct {
		id     string
		format labels.FreeTextFormat
	}{{"uk", labels.FormatUK}, {"eu", labels.FormatEU}} {
		src, ok := cfg.Source(list.id)
		if !ok || !src.Ingestible() {
			continue
		}
		batch, fres, err := ingestFreeTextSanctions(ctx, src.URL, list.format, list.id, src.Confidence)
		if err != nil {
			log.Error("sanctions list failed; the previous snapshot's labels stand", "source", list.id, "error", err)
			continue
		}
		res, err := st.Upsert(ctx, snapshotID, batch)
		if err != nil {
			return err
		}
		for _, chainID := range sortedKeys(fres.ByChain) {
			keep := map[string]bool{}
			for _, l := range batch {
				if l.Chain == chainID {
					keep[l.Address] = true
				}
			}
			if _, err := st.Retire(ctx, snapshotID, list.id, chainID, keep); err != nil {
				return err
			}
		}
		log.Info("sanctions list ingested", "source", list.id, "designations", fres.Designations,
			"with_addresses", fres.WithAddress, "labels", len(batch), "inserted", res.Inserted)
		total = add(total, res)
	}

	// --- Tether's USDT blacklist (docs/DECISIONS.md D29) ---
	// First-party and authoritative, but not a sanctions list: a failure is
	// logged and the run continues, and the previous snapshot's list stands.
	if src, ok := cfg.Source("tether_blacklist"); ok && src.Ingestible() {
		res, _, err := refreshTether(ctx, cfg, st, snapshotID, 0, log)
		if err != nil {
			return err
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
			log.Error("proof-of-reserves list failed; the previous snapshot's labels stand",
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
	for _, id := range []string{"un"} {
		if src, ok := cfg.Source(id); ok && src.Ingestible() {
			log.Warn("source permitted but not yet implemented; it contributes no labels "+
				"and its absence reduces coverage", "source", id)
		}
	}
	for _, id := range []string{"etherscan", "bscscan", "tronscan", "chainabuse", "dune", "binance_por", "okx_por", "nbctf"} {
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

// storedServiceStats splits candidates into those whose own history is
// fetched, with their stored transfer and counterparty counts, and the rest.
func storedServiceStats(ctx context.Context, ch, pg *sql.DB, chainID string, candidates []string) ([]labels.StoredStats, []string, error) {
	truncated := map[string]bool{}
	fetched := map[string]bool{}
	rows, err := pg.QueryContext(ctx,
		`SELECT address, truncated FROM address_freshness WHERE chain = $1 AND address = ANY($2)`, chainID, candidates)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var a string
		var t bool
		if err := rows.Scan(&a, &t); err != nil {
			rows.Close()
			return nil, nil, err
		}
		fetched[a], truncated[a] = true, t
	}
	rows.Close()
	var have, rest []string
	for _, a := range candidates {
		if fetched[a] {
			have = append(have, a)
		} else {
			rest = append(rest, a)
		}
	}
	var out []labels.StoredStats
	for start := 0; start < len(have); start += 500 {
		chunk := have[start:min(start+500, len(have))]
		crows, err := ch.QueryContext(ctx, `
			SELECT a, sum(n), uniqExact(cp), dateDiff('day', min(f), max(l)) FROM (
				SELECT from_address AS a, to_address AS cp, transfer_count AS n, first_seen AS f, last_seen AS l
				FROM edges_current WHERE chain = ? AND from_address IN (?)
				UNION ALL
				SELECT to_address, from_address, transfer_count, first_seen, last_seen
				FROM edges_by_to_current WHERE chain = ? AND to_address IN (?))
			GROUP BY a`, chainID, chunk, chainID, chunk)
		if err != nil {
			return nil, nil, err
		}
		for crows.Next() {
			var s labels.StoredStats
			var days int64
			if err := crows.Scan(&s.Address, &s.Transfers, &s.Counterparties, &days); err != nil {
				crows.Close()
				return nil, nil, err
			}
			s.Truncated = truncated[s.Address]
			s.ActiveDays = int(days)
			out = append(out, s)
		}
		crows.Close()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, rest, nil
}

// ingestTetherBlacklist reads the USDT contract's blacklist events from the
// start and folds them into the current list.
func ingestTetherBlacklist(ctx context.Context, cfg *config.Config, rps float64, log *slog.Logger) ([]labels.Label, error) {
	chainCfg, ok := cfg.Chain("tron")
	if !ok {
		return nil, fmt.Errorf("tron is not declared in sources.yaml")
	}
	key := os.Getenv("TRONGRID_API_KEY")
	if rps <= 0 {
		rps = chainCfg.RequestRate(key != "")
	}
	// Patient retries: the worker shares the key, and TronGrid throttles in
	// bursts that outlast the default six attempts.
	client := tron.NewClient(tron.Options{BaseURL: chainCfg.URL, APIKey: key,
		RequestsPerSecond: rps, MaxRetries: 12, Logger: log})
	src, _ := cfg.Source("tether_blacklist")

	events := map[string][]tron.ContractEvent{}
	for _, name := range []string{"AddedBlackList", "RemovedBlackList", "DestroyedBlackFunds"} {
		evs, err := client.ContractEvents(ctx, tron.USDTContract, name, time.Unix(0, 0))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		events[name] = evs
	}
	frozen := labels.TetherBlacklist(events["AddedBlackList"], events["RemovedBlackList"], events["DestroyedBlackFunds"])
	log.Info("tether blacklist read", "added_events", len(events["AddedBlackList"]),
		"removed_events", len(events["RemovedBlackList"]), "frozen_now", len(frozen))
	return labels.TetherLabels(frozen, src.Confidence), nil
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

	ch, err := store.OpenClickHouseBatch(ctx)
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
	//
	// Addresses that already carry a label are skipped. They used to fill the
	// top of the ranking every run, so the same known services were sampled
	// again and again while new ones never reached the 200-candidate budget.
	// THasRe…geRM, with 1,430 counterparties and $126M passed through, was
	// never sampled, and screens traced through it into its other customers'
	// exposure (docs/DECISIONS.md D30).
	budget := cfg.Weights.DerivedService.MaxCandidates
	if budget <= 0 {
		budget = 200
	}
	rows, err := ch.QueryContext(ctx, `
		SELECT addr, uniqExact(cp) AS counterparties
		FROM (
			SELECT from_address AS addr, to_address AS cp FROM edges_current WHERE chain = ?
			UNION ALL
			SELECT to_address AS addr, from_address AS cp FROM edges_by_to_current WHERE chain = ?
		)
		GROUP BY addr
		HAVING counterparties >= ?
		ORDER BY counterparties DESC, addr ASC
		LIMIT 5000`, chainID, chainID, cfg.Weights.DerivedService.MinCounterparties)
	if err != nil {
		return fmt.Errorf("select candidates: %w", err)
	}
	var ranked []string
	for rows.Next() {
		var addr string
		var n uint64
		if err := rows.Scan(&addr, &n); err != nil {
			rows.Close()
			return err
		}
		ranked = append(ranked, addr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	current, err := st.LatestSealedSnapshot(ctx)
	if err != nil {
		return err
	}
	known, err := st.ForAddresses(ctx, current, chainID, ranked)
	if err != nil {
		return err
	}
	var candidates []string
	labelled := 0
	for _, a := range ranked {
		if len(known[a]) > 0 {
			labelled++
			continue
		}
		if len(candidates) < budget {
			candidates = append(candidates, a)
		}
	}
	log.Info("service candidates", "service_shaped", len(ranked), "already_labelled", labelled,
		"sampled_now", len(candidates), "left_for_later", len(ranked)-labelled-len(candidates))

	if len(candidates) == 0 {
		fmt.Println("no candidates: ingest some addresses first")
		return nil
	}
	// Labels given from stored history are judged again every run: the rule
	// can tighten and history grows, and a label that no longer holds must
	// go rather than stop traversal forever (docs/DECISIONS.md D30).
	existing, err := st.BySources(ctx, current, chainID, []string{"derived:service"})
	if err != nil {
		return err
	}
	var recheck []string
	for _, l := range existing {
		if h, _ := l.Evidence["heuristic"].(string); h == "high_volume_service_stored" {
			recheck = append(recheck, l.Address)
		}
	}

	// Addresses with their history fetched are judged from it; only the rest
	// are sampled through the API (docs/DECISIONS.md D30).
	stored, toSample, err := storedServiceStats(ctx, ch, st.DB(), chainID, append(candidates, recheck...))
	if err != nil {
		return err
	}
	storedJudged, storedLabels := labels.JudgeStoredServices(stored, cfg, chainID)
	var withdraw []string
	isRecheck := map[string]bool{}
	for _, a := range recheck {
		isRecheck[a] = true
	}
	for _, c := range storedJudged {
		if isRecheck[c.Address] && !c.Accepted {
			withdraw = append(withdraw, c.Address)
		}
	}
	fmt.Printf("stored-history labels rechecked: %d, withdrawn %d\n", len(recheck), len(withdraw))
	fmt.Printf("judged from stored history: %d, accepted %d\n", len(storedJudged), len(storedLabels))
	log.Info("sampling candidates", "count", len(toSample), "chain", chainID)

	key := os.Getenv("TRONGRID_API_KEY")
	sampler := labels.NewTronSampler(chainCfg.URL, key, chainCfg.RequestRate(key != ""))
	judged, derived, err := labels.DetectServices(ctx, sampler, cfg, chainID, toSample)
	if err != nil {
		return err
	}
	judged = append(judged, storedJudged...)
	derived = append(derived, storedLabels...)

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

	if len(derived) == 0 && len(withdraw) == 0 {
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
	if _, err := st.RetireAddresses(ctx, snap, "derived:service", chainID, withdraw); err != nil {
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

func derive(ctx context.Context, cfg *config.Config, pg *sql.DB, st *labels.Store, chainID string, log *slog.Logger) error {
	ch, err := store.OpenClickHouseBatch(ctx)
	if err != nil {
		return err
	}
	defer ch.Close()

	snapshotID, err := st.LatestSealedSnapshot(ctx)
	if err != nil {
		return err
	}

	// Hot wallets first: the deposit heuristic can then anchor on them too
	// (docs/DECISIONS.md D28).
	if hw := cfg.Weights.DerivedHotWallet; hw.Enabled {
		reserves, err := st.BySources(ctx, snapshotID, chainID, hw.ReserveSources)
		if err != nil {
			return err
		}
		judged, hot, err := labels.DeriveHotWallets(ctx, ch, cfg, reserves, chainID)
		if err != nil {
			return err
		}
		fmt.Printf("hot wallets:         %d accepted of %d wallets with reserve flows\n", len(hot), len(judged))
		for _, l := range hot {
			fmt.Printf("  %s  %s\n", l.Address, l.Entity)
		}
		if len(hot) > 0 {
			snap, err := st.OpenSnapshot(ctx, "derived hot wallets")
			if err != nil {
				return err
			}
			if _, err := st.Upsert(ctx, snap, hot); err != nil {
				return err
			}
			if _, err := st.SealSnapshot(ctx, snap); err != nil {
				return err
			}
			snapshotID = snap
		}
	}

	if cfg.Weights.DerivedOperator.Enabled {
		snap, err := deriveOperators(ctx, cfg, pg, st, snapshotID, chainID)
		if err != nil {
			return err
		}
		if snap != 0 {
			snapshotID = snap
		}
	}
	if cfg.Weights.DerivedPoisoning.Enabled {
		snap, err := derivePoisoning(ctx, cfg, ch, st, snapshotID, chainID)
		if err != nil {
			return err
		}
		if snap != 0 {
			snapshotID = snap
		}
	}

	// The heuristic anchors on known exchange hot wallets. This map was once
	// left empty here, so the heuristic could never fire whatever the label
	// set held (docs/DECISIONS.md D20).
	exchangeLabels, err := st.ByCategories(ctx, snapshotID, chainID, labels.DepositAnchorCategories)
	if err != nil {
		return err
	}
	rules := cfg.Weights.DerivedDeposit
	if len(rules.AnchorSources) > 0 {
		bySource, err := st.BySources(ctx, snapshotID, chainID, rules.AnchorSources)
		if err != nil {
			return err
		}
		exchangeLabels = append(exchangeLabels, bySource...)
	}
	known := labels.DepositAnchors(exchangeLabels, rules.AnchorSources)
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

	jobs := store.NewJobs(pg)
	fetched := func(ctx context.Context, addrs []string) (map[string]bool, error) {
		return fetchedAddresses(ctx, pg, chainID, addrs)
	}

	// An anchor's depositors are found from its own history, so an anchor
	// never fetched finds none. Queue those; the worker fetches them and the
	// next run sees their senders.
	anchors := make([]string, 0, len(known))
	for a := range known {
		anchors = append(anchors, a)
	}
	sort.Strings(anchors)
	haveAnchor, err := fetched(ctx, anchors)
	if err != nil {
		return err
	}
	var anchorsQueued int
	for _, a := range anchors {
		if !haveAnchor[a] {
			if err := jobs.EnqueueBackground(ctx, chainID, a); err != nil {
				return err
			}
			anchorsQueued++
		}
	}

	candidates, derived, err := labels.DeriveDeposits(ctx, ch, cfg, known, chainID, fetched)
	if err != nil {
		return err
	}

	// Candidates rejected only for lack of history are queued, largest
	// value to the hot wallet first, so the next run can judge them.
	var needFetch []labels.DepositCandidate
	for _, c := range candidates {
		if c.NeedsFetch {
			needFetch = append(needFetch, c)
		}
	}
	sort.SliceStable(needFetch, func(i, j int) bool {
		return needFetch[i].ValueToHotWallet.GreaterThan(needFetch[j].ValueToHotWallet)
	})
	if len(needFetch) > rules.FetchCandidates {
		needFetch = needFetch[:rules.FetchCandidates]
	}
	for _, c := range needFetch {
		if err := jobs.EnqueueBackground(ctx, chainID, c.Address); err != nil {
			return err
		}
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
	fmt.Printf("anchors:             %d (%d queued for fetching)\n", len(known), anchorsQueued)
	fmt.Printf("candidates queued:   %d (judged on the next run, once fetched)\n", len(needFetch))

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

// fetchedAddresses reports which addresses have their own history stored.
func fetchedAddresses(ctx context.Context, pg *sql.DB, chainID string, addrs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(addrs) == 0 {
		return out, nil
	}
	rows, err := pg.QueryContext(ctx,
		`SELECT address FROM address_freshness WHERE chain = $1 AND address = ANY($2)`, chainID, addrs)
	if err != nil {
		return nil, fmt.Errorf("fetched addresses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out[a] = true
	}
	return out, rows.Err()
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

// refreshTether reads the blacklist into snapshotID and returns the
// addresses frozen since the previous read. A failed read is logged and the
// previous list stands: the blacklist is authoritative but not a sanctions
// list, so its absence is not worth failing a run over.
func refreshTether(ctx context.Context, cfg *config.Config, st *labels.Store, snapshotID int64,
	rps float64, log *slog.Logger) (labels.UpsertResult, []string, error) {
	batch, err := ingestTetherBlacklist(ctx, cfg, rps, log)
	if err != nil {
		log.Error("tether blacklist failed; the previous list stands", "error", err)
		return labels.UpsertResult{}, nil, nil
	}
	before, err := st.CurrentAddresses(ctx, "tether_blacklist", "tron")
	if err != nil {
		return labels.UpsertResult{}, nil, err
	}
	res, err := st.Upsert(ctx, snapshotID, batch)
	if err != nil {
		return res, nil, err
	}
	keep := make(map[string]bool, len(batch))
	var fresh []string
	for _, l := range batch {
		keep[l.Address] = true
		if !before[l.Address] {
			fresh = append(fresh, l.Address)
		}
	}
	released, err := st.Retire(ctx, snapshotID, "tether_blacklist", "tron", keep)
	if err != nil {
		return res, nil, err
	}
	log.Info("tether blacklist ingested", "frozen", len(batch), "inserted", res.Inserted,
		"newly_frozen", len(fresh), "released_since_last_run", released)
	return res, fresh, nil
}

// tetherRefresh is the frequent refresh (docs/DECISIONS.md D34). Tether
// freezes in clusters, and a counterparty of a frozen wallet that is going
// to be frozen too almost always is within three days, so a daily read is
// too slow to warn anyone. It seals a snapshot holding only the blacklist
// change, then makes every watch with direct flow to or from a newly frozen
// address due, so the bot's monitor rescreens it within minutes.
func tetherRefresh(ctx context.Context, cfg *config.Config, pg *sql.DB, st *labels.Store, log *slog.Logger) error {
	// Labels are valid from the snapshot they were written in. Sealing a
	// later snapshot while another run is still writing into an earlier one
	// would change what the sealed one resolves to after the fact (SPEC.md
	// §2), so the refresh waits for that run. A snapshot left open for six
	// hours is a crashed run, not a running one.
	var open int
	if err := pg.QueryRowContext(ctx, `SELECT count(*) FROM label_snapshots
		WHERE sealed_at IS NULL AND created_at > now() - interval '6 hours'`).Scan(&open); err != nil {
		return err
	}
	if open > 0 {
		log.Info("another labeler run holds an open snapshot; skipping this refresh")
		return nil
	}
	snapshotID, err := st.OpenSnapshot(ctx, "labeler tether")
	if err != nil {
		return err
	}
	// Two requests a second: the read takes about 25 s, and leaves the
	// worker, which shares the API key's 15 a second, its full rate. At the
	// worker's own rate the two together were throttled.
	_, fresh, err := refreshTether(ctx, cfg, st, snapshotID, 2, log)
	if err != nil {
		return err
	}
	if _, err := st.SealSnapshot(ctx, snapshotID); err != nil {
		return err
	}
	fmt.Printf("snapshot %d sealed; %d newly frozen\n", snapshotID, len(fresh))
	if len(fresh) == 0 {
		return nil
	}

	var watched []string
	rows, err := pg.QueryContext(ctx, `SELECT DISTINCT address FROM watches WHERE chain = 'tron' AND removed_at IS NULL`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return err
		}
		watched = append(watched, a)
	}
	rows.Close()
	if len(watched) == 0 {
		return nil
	}

	ch, err := store.OpenClickHouse(ctx)
	if err != nil {
		return err
	}
	defer ch.Close()
	crows, err := ch.QueryContext(ctx, `
		SELECT DISTINCT a FROM (
			SELECT from_address AS a FROM edges_current WHERE chain = 'tron' AND from_address IN (?) AND to_address IN (?)
			UNION ALL
			SELECT to_address AS a FROM edges_by_to_current WHERE chain = 'tron' AND to_address IN (?) AND from_address IN (?)
		)`, watched, fresh, watched, fresh)
	if err != nil {
		return err
	}
	var hit []string
	for crows.Next() {
		var a string
		if err := crows.Scan(&a); err != nil {
			crows.Close()
			return err
		}
		hit = append(hit, a)
	}
	crows.Close()
	if len(hit) == 0 {
		return nil
	}
	n, err := pg.ExecContext(ctx, `UPDATE watches SET checked_at = NULL
		WHERE chain = 'tron' AND address = ANY($1) AND removed_at IS NULL`, hit)
	if err != nil {
		return err
	}
	k, _ := n.RowsAffected()
	log.Info("watches touching a newly frozen address made due", "addresses", len(hit), "watches", k)
	return nil
}

func ingestFreeTextSanctions(ctx context.Context, url string, format labels.FreeTextFormat, sourceID string,
	confidence float64) ([]labels.Label, *labels.FreeTextResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	res, out, err := labels.ParseFreeTextSanctions(ctx, resp.Body, format, sourceID, confidence)
	if err != nil {
		return nil, nil, err
	}
	// A list that parses to nothing has changed shape, not emptied.
	if res.Designations == 0 {
		return nil, nil, fmt.Errorf("no designations read; the list's layout may have changed")
	}
	return out, res, nil
}
