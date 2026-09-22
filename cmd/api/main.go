// Command api serves the screening HTTP API.
//
//	POST /v1/screen                    { chain, address, direction? } -> full result
//	GET  /v1/address/:chain/:address   cached result
//	GET  /v1/health
//
// SPEC.md §1: every API response must carry the framing that this is a triage
// tool and not a regulated AML determination.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/ingest"
	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/mozer/tether-risk/internal/screen"
	"github.com/mozer/tether-risk/internal/store"
	"github.com/shopspring/decimal"
)

const disclaimer = "Automated triage and pre-screening built on open data. " +
	"This is not a regulated AML determination and must not be used as one."

func main() {
	var (
		addr      = flag.String("addr", ":8080", "listen address")
		configDir = flag.String("config", "config", "configuration directory")
		fetch     = flag.Bool("fetch", true,
			"fetch unknown or stale addresses and queue their counterparties before scoring")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *addr, *configDir, *fetch, log); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, addr, configDir string, fetch bool, log *slog.Logger) error {
	cfg, err := config.Load(configDir)
	if err != nil {
		return err
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

	svc := screen.NewService(ch, pg, cfg)
	if fetch {
		// Every chain with a live data path gets a prefetcher. One whose
		// adapter cannot be built is logged and scores stored data only,
		// rather than taking the whole API down.
		for _, c := range cfg.Sources.Chains {
			if !c.Available() {
				continue
			}
			p, err := ingest.NewPrefetcher(c.ID, cfg, ch, pg, log)
			if err != nil {
				log.Warn("no prefetch for chain; it will score stored data only", "chain", c.ID, "error", err)
				continue
			}
			svc.WithPrefetch(c.ID, p)
		}
	}

	srv := &server{
		svc: svc,
		cfg: cfg,
		pg:  pg,
		log: log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/screen", srv.handleScreen)
	mux.HandleFunc("GET /v1/address/{chain}/{address}", srv.handleCached)
	mux.HandleFunc("GET /v1/health", srv.handleHealth)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           logging(log, mux),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      2 * time.Minute, // a cold traversal can be slow
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("api listening", "addr", addr, "config_version", cfg.Version)
	return httpSrv.ListenAndServe()
}

type server struct {
	svc *screen.Service
	cfg *config.Config
	pg  *sql.DB
	log *slog.Logger
}

// ---------------------------------------------------------------------------
// Response shapes
// ---------------------------------------------------------------------------

type screenRequest struct {
	Chain     string `json:"chain"`
	Address   string `json:"address"`
	Direction string `json:"direction,omitempty"`
}

type screenResponse struct {
	Address string `json:"address"`
	Chain   string `json:"chain"`

	Score float64 `json:"score"`
	Band  string  `json:"band"`

	// SPEC.md §7: coverage in every response.
	Coverage      float64 `json:"coverage"`
	LowConfidence bool    `json:"low_confidence"`

	// Present only when true, so a reader never has to interpret a false.
	SanctionsOverride bool `json:"sanctions_override,omitempty"`
	BandCappedByAbuse bool `json:"band_capped_by_abuse_rule,omitempty"`

	Inbound  *directionResponse `json:"inbound,omitempty"`
	Outbound *directionResponse `json:"outbound,omitempty"`

	// OwnLabel is a label on the queried address itself, distinct from
	// exposure reached through it. Present only when the address is listed.
	OwnLabel *ownLabelResponse `json:"own_label,omitempty"`

	// Activity is the address's own stored history, before attribution.
	Activity *activityResponse `json:"activity,omitempty"`

	// Depth says whether tracing had finished when this was scored.
	Depth *depthResponse `json:"depth,omitempty"`

	LabelSnapshotID int64  `json:"label_snapshot_id"`
	ConfigVersion   string `json:"config_version"`

	Disclaimer string `json:"disclaimer"`
}

type ownLabelResponse struct {
	Entity     string  `json:"entity"`
	Category   string  `json:"category"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
	Conflicted bool    `json:"conflicted,omitempty"`
	Note       string  `json:"note"`
}

type directionResponse struct {
	Score           float64            `json:"score"`
	Coverage        float64            `json:"coverage"`
	Categories      []categoryResponse `json:"categories"`
	UnattributedPct float64            `json:"unattributed_pct"`

	// TotalTraced is the direction's summed path weight, the denominator of
	// its percentages. It is a share of value decayed per hop, not a USD
	// amount. Clients use it to combine directions the same way overall
	// coverage does.
	TotalTraced float64 `json:"total_traced"`

	TopPaths []pathResponse `json:"top_paths"`

	// Connections are identified counterparties, largest share first.
	Connections []connectionResponse `json:"connections"`
	// UnattributedReasons splits unattributed_pct by why tracing stopped.
	UnattributedReasons []reasonResponse `json:"unattributed_reasons"`

	Traversal traversalStats `json:"traversal"`
}

type activityResponse struct {
	InUSD             float64         `json:"in_usd"`
	OutUSD            float64         `json:"out_usd"`
	InTransfers       uint64          `json:"in_transfers"`
	OutTransfers      uint64          `json:"out_transfers"`
	InCounterparties  uint64          `json:"in_counterparties"`
	OutCounterparties uint64          `json:"out_counterparties"`
	FirstSeen         string          `json:"first_seen,omitempty"`
	LastSeen          string          `json:"last_seen,omitempty"`
	Assets            []assetResponse `json:"assets"`
	UnpricedTransfers uint64          `json:"unpriced_transfers"`
	UnpricedTokens    uint64          `json:"unpriced_tokens"`
}

type depthResponse struct {
	Fetched             bool   `json:"fetched"`
	FetchError          string `json:"fetch_error,omitempty"`
	StillFetching       bool   `json:"still_fetching"`
	FrontierPending     int    `json:"frontier_pending"`
	FrontierQueued      int    `json:"frontier_queued"`
	FrontierEnded       int    `json:"frontier_ended"`
	HistoryTruncated    bool   `json:"history_truncated"`
	Counterparties      int    `json:"counterparties"`
	Traced              int    `json:"traced"`
	TotalCounterparties int    `json:"total_counterparties"`
	Complete            bool   `json:"complete"`
}

type assetResponse struct {
	Asset     string  `json:"asset"`
	InUSD     float64 `json:"in_usd"`
	OutUSD    float64 `json:"out_usd"`
	Transfers uint64  `json:"transfers"`
}

type connectionResponse struct {
	Address    string  `json:"address"`
	Entity     string  `json:"entity,omitempty"`
	Category   string  `json:"category"`
	Source     string  `json:"source,omitempty"`
	Confidence float64 `json:"confidence"`
	Pct        float64 `json:"pct"`
	MinHops    int     `json:"min_hops"`
	Paths      int     `json:"paths"`
}

type reasonResponse struct {
	Reason string  `json:"reason"`
	Pct    float64 `json:"pct"`
	Paths  int     `json:"paths"`
}

type categoryResponse struct {
	Category     string  `json:"category"`
	Pct          float64 `json:"pct"`
	Weight       float64 `json:"weight"`
	Contribution float64 `json:"contribution"`
	PathCount    int     `json:"path_count"`
}

type pathResponse struct {
	// Explanation is the plain-language form SPEC.md §8 asks for.
	Explanation  string   `json:"explanation"`
	Hops         []string `json:"hops"`
	HopCount     int      `json:"hop_count"`
	Terminal     string   `json:"terminal_address"`
	Entity       string   `json:"entity,omitempty"`
	Category     string   `json:"category"`
	Source       string   `json:"source,omitempty"`
	Contribution float64  `json:"contribution"`
}

type traversalStats struct {
	NodesVisited    int  `json:"nodes_visited"`
	FanoutCapped    bool `json:"fanout_capped"`
	HopLimitReached bool `json:"hop_limit_reached"`
}

type errorResponse struct {
	Error      string `json:"error"`
	Detail     string `json:"detail,omitempty"`
	Disclaimer string `json:"disclaimer"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *server) handleScreen(w http.ResponseWriter, r *http.Request) {
	var req screenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Chain == "" {
		req.Chain = "tron"
	}
	if strings.TrimSpace(req.Address) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "address is required")
		return
	}

	res, err := s.svc.Screen(r.Context(), req.Chain, strings.TrimSpace(req.Address))
	if err != nil {
		// A chain with no live data path is a distinct, documented condition,
		// not a generic failure: an empty result would read as "no activity".
		if strings.HasPrefix(err.Error(), "chain_unavailable") {
			writeError(w, http.StatusServiceUnavailable, "chain_unavailable", err.Error())
			return
		}
		s.log.Error("screen failed", "address", req.Address, "error", err)
		writeError(w, http.StatusInternalServerError, "screen_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toResponse(res))
}

func (s *server) handleCached(w http.ResponseWriter, r *http.Request) {
	chainID := r.PathValue("chain")
	address := r.PathValue("address")

	var (
		score, coverage       decimal.Decimal
		band, configVersion   string
		snapshotID            int64
		lowConfidence, sancOv bool
		finishedAt            time.Time
	)
	err := s.pg.QueryRowContext(r.Context(), `
		SELECT score, band, coverage, low_confidence, sanctions_override,
		       label_snapshot_id, config_version, finished_at
		FROM runs
		WHERE chain = $1 AND address = $2 AND status = 'ok'
		ORDER BY started_at DESC LIMIT 1`,
		chainID, address).Scan(&score, &band, &coverage, &lowConfidence, &sancOv,
		&snapshotID, &configVersion, &finishedAt)

	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_screened",
			"this address has not been screened; POST /v1/screen first")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"address":            address,
		"chain":              chainID,
		"score":              round(score, 4),
		"band":               band,
		"coverage":           round(coverage, 6),
		"low_confidence":     lowConfidence,
		"sanctions_override": sancOv,
		"label_snapshot_id":  snapshotID,
		"config_version":     configVersion,
		"screened_at":        finishedAt.UTC().Format(time.RFC3339),
		"cached":             true,
		"disclaimer":         disclaimer,
	})
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{
		"status":         "ok",
		"config_version": s.cfg.Version,
	}

	// Report which chains actually have a live data path, rather than implying
	// every configured chain is queryable.
	chains := map[string]string{}
	for _, c := range s.cfg.Sources.Chains {
		chains[c.ID] = string(c.Status)
	}
	status["chains"] = chains

	if err := s.pg.PingContext(r.Context()); err != nil {
		status["status"] = "degraded"
		status["postgres"] = err.Error()
		writeJSON(w, http.StatusServiceUnavailable, status)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// ---------------------------------------------------------------------------

// round trims presentation noise.
//
// Dust worth two cents against millions of dollars of stablecoin flow produces
// a share like 1.49631e-9%, which is accurate and unreadable. Rounding to four
// decimal places keeps every figure a reader can act on while dropping digits
// that only encode floating-point residue. It never rounds a non-zero value up
// to something material, and `low_confidence` is set from the unrounded
// figure, so nothing is hidden by this.
func round(d decimal.Decimal, places int32) float64 {
	return d.Round(places).InexactFloat64()
}

func toResponse(res *scoring.Result) screenResponse {
	var own *ownLabelResponse
	if l := res.OwnLabel; l != nil {
		own = &ownLabelResponse{
			Entity: l.Entity, Category: l.Category, Source: l.Source,
			Confidence: l.Confidence, Conflicted: l.Conflicted,
			Note: "This address is directly listed by the named source. " +
				"That is a finding in its own right, separate from the traced exposure below.",
		}
	}
	return screenResponse{
		OwnLabel:          own,
		Address:           res.Address,
		Chain:             res.Chain,
		Score:             round(res.Score, 4),
		Band:              res.Band,
		Coverage:          round(res.Coverage, 6),
		LowConfidence:     res.LowConfidence,
		SanctionsOverride: res.SanctionsOverride,
		BandCappedByAbuse: res.BandCappedByAbuseRule,
		Inbound:           toDirection(res.Inbound),
		Outbound:          toDirection(res.Outbound),
		LabelSnapshotID:   res.LabelSnapshotID,
		ConfigVersion:     res.ConfigVersion,
		Disclaimer:        disclaimer,
		Activity:          toActivity(res.Activity),
		Depth:             toDepth(res.Depth),
	}
}

func toDepth(d *scoring.DepthStatus) *depthResponse {
	if d == nil {
		return nil
	}
	return &depthResponse{
		Fetched: d.Fetched, FetchError: d.FetchError,
		StillFetching: d.StillFetching, HistoryTruncated: d.HistoryTruncated,
		FrontierPending: d.FrontierPending, FrontierQueued: d.FrontierQueued, FrontierEnded: d.FrontierEnded,
		Counterparties: d.Counterparties, Traced: d.Traced,
		TotalCounterparties: d.TotalCounterparties, Complete: d.Complete(),
	}
}

func toActivity(a *scoring.Activity) *activityResponse {
	if a == nil {
		return nil
	}
	out := &activityResponse{
		InUSD: round(a.InUSD, 2), OutUSD: round(a.OutUSD, 2),
		InTransfers: a.InTransfers, OutTransfers: a.OutTransfers,
		InCounterparties: a.InCounterparties, OutCounterparties: a.OutCounterparties,
		Assets:            make([]assetResponse, 0, len(a.Assets)),
		UnpricedTransfers: a.UnpricedTransfers, UnpricedTokens: a.UnpricedTokens,
	}
	if !a.FirstSeen.IsZero() {
		out.FirstSeen = a.FirstSeen.UTC().Format("2006-01-02")
		out.LastSeen = a.LastSeen.UTC().Format("2006-01-02")
	}
	for _, as := range a.Assets {
		out.Assets = append(out.Assets, assetResponse{
			Asset: as.Asset, InUSD: round(as.InUSD, 2), OutUSD: round(as.OutUSD, 2), Transfers: as.Transfers,
		})
	}
	return out
}

func toDirection(d *scoring.DirectionResult) *directionResponse {
	if d == nil {
		return nil
	}
	out := &directionResponse{
		Score:               round(d.Score, 4),
		Coverage:            round(d.Coverage, 6),
		UnattributedPct:     round(d.UnattributedPct, 4),
		TotalTraced:         round(d.TotalTraced, 6),
		Categories:          make([]categoryResponse, 0, len(d.Categories)),
		TopPaths:            make([]pathResponse, 0, len(d.TopPaths)),
		Connections:         make([]connectionResponse, 0, len(d.Connections)),
		UnattributedReasons: make([]reasonResponse, 0, len(d.UnattributedReasons)),
		Traversal: traversalStats{
			NodesVisited:    d.NodesVisited,
			FanoutCapped:    d.FanoutCapped,
			HopLimitReached: d.HopLimitReached,
		},
	}
	for _, c := range d.Categories {
		out.Categories = append(out.Categories, categoryResponse{
			Category:     c.Category,
			Pct:          round(c.Pct, 4),
			Weight:       c.Weight.InexactFloat64(),
			Contribution: round(c.Contribution, 4),
			PathCount:    c.PathCount,
		})
	}
	for _, p := range d.TopPaths {
		name := p.Terminal.Entity
		if name == "" {
			name = p.Terminal.Address
		}
		out.TopPaths = append(out.TopPaths, pathResponse{
			Explanation: fmt.Sprintf("%d hop(s) to %s, categorised %s, contributing %s of traced value",
				p.HopCount(), name, p.Terminal.Category, p.Contribution.StringFixed(4)),
			Hops:         p.Hops,
			HopCount:     p.HopCount(),
			Terminal:     p.Terminal.Address,
			Entity:       p.Terminal.Entity,
			Category:     p.Terminal.Category,
			Source:       p.Terminal.Source,
			Contribution: round(p.Contribution, 6),
		})
	}
	for _, c := range d.Connections {
		out.Connections = append(out.Connections, connectionResponse{
			Address: c.Address, Entity: c.Entity, Category: c.Category, Source: c.Source,
			Confidence: c.Confidence, Pct: round(c.Pct, 4), MinHops: c.MinHops, Paths: c.Paths,
		})
	}
	for _, r := range d.UnattributedReasons {
		out.UnattributedReasons = append(out.UnattributedReasons, reasonResponse{
			Reason: r.Reason, Pct: round(r.Pct, 4), Paths: r.Paths,
		})
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorResponse{Error: code, Detail: detail, Disclaimer: disclaimer})
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		log.Info("request", "method", r.Method, "path", r.URL.Path,
			"duration", time.Since(started).Round(time.Millisecond))
	})
}
