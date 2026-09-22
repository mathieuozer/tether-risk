// Package screen ties ingestion, labels, traversal and scoring into a single
// address screening operation.
package screen

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/graph"
	"github.com/mozer/tether-risk/internal/ingest"
	"github.com/mozer/tether-risk/internal/labels"
	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/shopspring/decimal"
)

// clickhouseEdges reads neighbours from the aggregated edge views.
//
// Reads go through edges_current / edges_by_to_current, never the underlying
// tables: background merges mean an unaggregated read can return several
// partial rows for one edge, and traversal that summed only one would
// understate value and therefore understate risk (docs/DECISIONS.md D8).
type clickhouseEdges struct{ ch *sql.DB }

func (c clickhouseEdges) Neighbours(ctx context.Context, chainID, address string, dir graph.Direction) ([]graph.Neighbour, error) {
	var query string
	switch dir {
	case graph.Outbound:
		query = `SELECT to_address, total_usd_value, transfer_count, unpriced_count
		         FROM edges_current WHERE chain = ? AND from_address = ?`
	case graph.Inbound:
		query = `SELECT from_address, total_usd_value, transfer_count, unpriced_count
		         FROM edges_by_to_current WHERE chain = ? AND to_address = ?`
	default:
		return nil, fmt.Errorf("unknown direction %q", dir)
	}

	rows, err := c.ch.QueryContext(ctx, query, chainID, address)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Edges are per-asset, so one counterparty can appear several times. They
	// are summed here: a counterparty that received USDT and TRX is one
	// counterparty, and treating it as two would halve each share and distort
	// the haircut.
	merged := map[string]*graph.Neighbour{}
	for rows.Next() {
		var addr string
		var usd decimal.Decimal
		var transfers, unpriced uint64
		if err := rows.Scan(&addr, &usd, &transfers, &unpriced); err != nil {
			return nil, err
		}
		if n, ok := merged[addr]; ok {
			n.USDValue = n.USDValue.Add(usd)
			n.TransferCount += transfers
			n.UnpricedCount += unpriced
			continue
		}
		merged[addr] = &graph.Neighbour{
			Address: addr, USDValue: usd,
			TransferCount: transfers, UnpricedCount: unpriced,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]graph.Neighbour, 0, len(merged))
	for _, n := range merged {
		out = append(out, *n)
	}
	return out, nil
}

// postgresLabels resolves labels at a fixed snapshot.
type postgresLabels struct {
	store      *labels.Store
	resolver   *labels.Resolver
	snapshotID int64
}

func (p postgresLabels) Lookup(ctx context.Context, chainID string, addresses []string) (map[string]labels.Resolution, error) {
	raw, err := p.store.ForAddresses(ctx, p.snapshotID, chainID, addresses)
	if err != nil {
		return nil, err
	}
	out := make(map[string]labels.Resolution, len(raw))
	for addr, ls := range raw {
		out[addr] = p.resolver.Resolve(chainID, addr, ls)
	}
	return out, nil
}

// Service screens addresses.
type Service struct {
	ch       *sql.DB
	pg       *sql.DB
	cfg      *config.Config
	store    *labels.Store
	resolver *labels.Resolver
	scorer   *scoring.Scorer

	prefetch map[string]Prefetcher // by chain
}

// Prefetcher fetches an address's history before it is scored, and queues
// its counterparties to the given depth. ingest.Worker implements it.
type Prefetcher interface {
	FetchAddress(ctx context.Context, address string, depthRemaining int) (ingest.Result, error)
	Queue(ctx context.Context, address string, depthRemaining int) error
}

// fetchBudget bounds how long a screen spends fetching before it answers
// with what is stored. A large address can take minutes to drain at the
// public rate limit, longer than a chat client or the API's write timeout
// will wait. The worker finishes the rest.
const fetchBudget = 45 * time.Second

// prefetchDepth is how far a screen asks for: the address and its direct
// counterparties. Counterparties are fetched by the background worker, so a
// screen returns once the address itself is stored.
const prefetchDepth = 1

// enqueueCap mirrors ingest.Options.MaxNeighboursEnqueued's default: the
// number of counterparties a fetch queues, most active first.
const enqueueCap = 100

// frontierCap bounds how many dead ends one screen queues. Each rescreen then
// reaches one ring further, up to the traversal's hop limit, so cost grows
// with how far the trail actually goes rather than with fan-out.
const frontierCap = 100

// WithPrefetch makes Screen fetch unknown or stale addresses on a chain
// before scoring. A chain without one scores only what is already stored.
func (s *Service) WithPrefetch(chainID string, p Prefetcher) *Service {
	if s.prefetch == nil {
		s.prefetch = map[string]Prefetcher{}
	}
	s.prefetch[chainID] = p
	return s
}

func NewService(ch, pg *sql.DB, cfg *config.Config) *Service {
	return &Service{
		ch: ch, pg: pg, cfg: cfg,
		store:    labels.NewStore(pg),
		resolver: labels.NewResolver(cfg),
		scorer:   scoring.New(cfg),
	}
}

// Screen runs both directions and scores the result.
func (s *Service) Screen(ctx context.Context, chainID, address string) (*scoring.Result, error) {
	// A chain with no live data path must say so rather than return an empty
	// result that reads as "no activity" (docs/PLAN.md F1).
	chainCfg, ok := s.cfg.Chain(chainID)
	if !ok {
		return nil, fmt.Errorf("unknown chain %q", chainID)
	}
	if !chainCfg.Available() {
		return nil, fmt.Errorf("chain_unavailable: %s", chainCfg.UnavailableReason)
	}

	// Scoring resolves against a sealed snapshot. An open one is still being
	// written, so two runs against it could legitimately differ and SPEC.md
	// §2's determinism guarantee would not hold.
	snapshotID, err := s.store.LatestSealedSnapshot(ctx)
	if err != nil {
		return nil, err
	}

	// Fetch first, so an address nobody has ingested is not scored against an
	// empty store and reported as inactive. The worker's TTL cache makes this
	// free for anything fetched recently. A failed fetch still scores what is
	// stored, and says so.
	depth := &scoring.DepthStatus{}
	if p, ok := s.prefetch[chainID]; ok {
		fctx, cancel := context.WithTimeout(ctx, fetchBudget)
		r, err := p.FetchAddress(fctx, address, prefetchDepth)
		cancel()
		switch {
		case err != nil && fctx.Err() != nil && ctx.Err() == nil:
			// Out of time, not failed. Pages written so far are kept and the
			// cursor is saved, so the worker resumes rather than restarts.
			depth.StillFetching = true
			if qerr := p.Queue(ctx, address, prefetchDepth); qerr != nil {
				depth.FetchError = qerr.Error()
			}
		case err != nil:
			depth.FetchError = err.Error()
		default:
			depth.Fetched = !r.Skipped
			depth.HistoryTruncated = r.Truncated
		}
	}

	edges := clickhouseEdges{ch: s.ch}
	lookup := postgresLabels{store: s.store, resolver: s.resolver, snapshotID: snapshotID}
	traverser := graph.New(edges, lookup, s.cfg)

	started := time.Now()

	inboundTr, err := traverser.Traverse(ctx, chainID, address, graph.Inbound)
	if err != nil {
		return nil, fmt.Errorf("inbound traversal: %w", err)
	}
	outboundTr, err := traverser.Traverse(ctx, chainID, address, graph.Outbound)
	if err != nil {
		return nil, fmt.Errorf("outbound traversal: %w", err)
	}

	inbound, err := s.scorer.ScoreDirection(inboundTr)
	if err != nil {
		return nil, err
	}
	outbound, err := s.scorer.ScoreDirection(outboundTr)
	if err != nil {
		return nil, err
	}

	// SPEC.md §7's override is about a hit on the queried address itself, not
	// exposure several hops away.
	own, err := s.store.ForAddress(ctx, snapshotID, chainID, address)
	if err != nil {
		return nil, err
	}
	ownResolution := s.resolver.Resolve(chainID, address, own)
	directHit := ownResolution.Labelled && s.cfg.AlwaysWins(ownResolution.Category)

	res := s.scorer.Combine(chainID, address, inbound, outbound,
		directHit, ownResolution.CappedByAbuseRule, snapshotID)

	// Surface the queried address's own label. A direct listing is a finding
	// regardless of what the traversal found.
	if ownResolution.Labelled {
		res.OwnLabel = &scoring.OwnLabel{
			Entity:     ownResolution.Entity,
			Category:   ownResolution.Category,
			Source:     ownResolution.Source,
			Confidence: ownResolution.Confidence,
			Conflicted: ownResolution.Conflicted,
		}
	}

	act, err := activity(ctx, s.ch, chainID, address)
	if err != nil {
		return nil, err
	}
	res.Activity = act
	res.Flags = scoring.BehaviourFlags(act, time.Now(), s.cfg.Weights.Behaviour)

	if err := s.profile(ctx, chainID, res); err != nil {
		return nil, err
	}

	if err := s.depthStatus(ctx, chainID, address, depth); err != nil {
		return nil, err
	}
	if p, ok := s.prefetch[chainID]; ok {
		if err := s.deepen(ctx, chainID, address, p, depth, inboundTr, outboundTr); err != nil {
			return nil, err
		}
	}
	res.Depth = depth
	v := scoring.Decide(res, s.cfg.Weights.Verdict)
	res.Verdict = &v

	// SPEC.md §2: every score must be reconstructible from stored intermediate
	// data. Persist the paths, not just the number.
	if err := s.persist(ctx, res, inboundTr, outboundTr, time.Since(started)); err != nil {
		return nil, fmt.Errorf("persist run: %w", err)
	}

	if c := ownResolution.Conflict(); c != nil {
		if err := s.store.RecordConflict(ctx, snapshotID, c); err != nil {
			return nil, err
		}
	}

	return res, nil
}

func (s *Service) persist(ctx context.Context, res *scoring.Result,
	in, out *graph.Result, elapsed time.Duration) error {

	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	totalTraced := in.TotalTraced.Add(out.TotalTraced)
	attributed := in.Attributed.Add(out.Attributed)

	var runID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO runs (
			chain, address, direction, label_snapshot_id, config_version,
			score, band, coverage, low_confidence, sanctions_override,
			total_traced_usd, attributed_usd,
			hops_used, nodes_visited, edges_considered, fanout_capped, hop_limit_reached,
			finished_at, duration_ms, status
		) VALUES ($1,$2,'both',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,now(),$17,'ok')
		RETURNING id`,
		res.Chain, res.Address, res.LabelSnapshotID, res.ConfigVersion,
		res.Score, res.Band, res.Coverage, res.LowConfidence, res.SanctionsOverride,
		totalTraced, attributed,
		maxInt(in.MaxHopReached, out.MaxHopReached),
		in.NodesVisited+out.NodesVisited,
		in.EdgesConsidered+out.EdgesConsidered,
		in.FanoutCapped || out.FanoutCapped,
		in.HopLimitReached || out.HopLimitReached,
		elapsed.Milliseconds(),
	).Scan(&runID)
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}

	for _, d := range []*scoring.DirectionResult{res.Inbound, res.Outbound} {
		if d == nil {
			continue
		}
		for _, c := range d.Categories {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO run_categories (run_id, direction, category, usd_value, pct, weight, contribution)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				runID, string(d.Direction), c.Category, c.USDValue, c.Pct, c.Weight, c.Contribution,
			); err != nil {
				return fmt.Errorf("insert category %s: %w", c.Category, err)
			}
		}
	}

	// Persist the paths. SPEC.md §2 is explicit that a score must reconstruct
	// from stored data; a score with no surviving path set is not auditable.
	for _, spec := range []struct {
		dir    graph.Direction
		result *graph.Result
	}{{graph.Inbound, in}, {graph.Outbound, out}} {
		for _, p := range spec.result.Paths {
			shares := make([]string, len(p.Shares))
			for i, sh := range p.Shares {
				shares[i] = sh.String()
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO run_paths (
					run_id, direction, hops, hop_count,
					terminal_address, terminal_entity, terminal_category,
					terminal_source, terminal_confidence,
					value_shares, decay_applied, contribution, usd_value, truncated
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
				runID, string(spec.dir), p.Hops, len(p.Hops),
				p.Terminal.Address, nullable(p.Terminal.Entity), p.Terminal.Category,
				nullable(p.Terminal.Source), nullableFloat(p.Terminal.Confidence),
				shares, p.Decay, p.Contribution, p.USDValue,
				!p.Terminal.Attributed(),
			); err != nil {
				return fmt.Errorf("insert path: %w", err)
			}
		}
	}

	return tx.Commit()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// depthStatus counts how many of the address's queued counterparties have
// their own history stored. Counterparties are ranked by transfer count, the
// order the ingest worker queues them in, so "traced" means exactly the set
// that was queued.
func (s *Service) depthStatus(ctx context.Context, chainID, address string, d *scoring.DepthStatus) error {
	rows, err := s.ch.QueryContext(ctx, `
		SELECT cp, sum(n) AS transfers FROM (
			SELECT to_address AS cp, transfer_count AS n FROM edges_current
			WHERE chain = ? AND from_address = ?
			UNION ALL
			SELECT from_address AS cp, transfer_count AS n FROM edges_by_to_current
			WHERE chain = ? AND to_address = ?)
		WHERE cp != ?
		GROUP BY cp ORDER BY transfers DESC, cp`,
		chainID, address, chainID, address, address)
	if err != nil {
		return fmt.Errorf("depth status: %w", err)
	}
	defer rows.Close()

	var queued []string
	for rows.Next() {
		var cp string
		var n uint64
		if err := rows.Scan(&cp, &n); err != nil {
			return fmt.Errorf("depth status: %w", err)
		}
		d.TotalCounterparties++
		if len(queued) < enqueueCap {
			queued = append(queued, cp)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	d.Counterparties = len(queued)

	// The address's own truncation, for a result served from the cache as
	// well as one fetched just now.
	var truncated sql.NullBool
	err = s.pg.QueryRowContext(ctx,
		`SELECT truncated FROM address_freshness WHERE chain = $1 AND address = $2`,
		chainID, address).Scan(&truncated)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("depth status: %w", err)
	}
	if truncated.Valid && truncated.Bool {
		d.HistoryTruncated = true
	}

	if len(queued) == 0 {
		return nil
	}

	if err := s.pg.QueryRowContext(ctx,
		`SELECT count(*) FROM address_freshness WHERE chain = $1 AND address = ANY($2)`,
		chainID, queued).Scan(&d.Traced); err != nil {
		return fmt.Errorf("depth status: %w", err)
	}
	return nil
}

// deepen queues the addresses where traversal ran out of stored history.
//
// Fetching every counterparty of every counterparty is fan-out squared, about
// 10,000 addresses at depth 2. The traversal already knows the handful where
// value actually stopped, so those are fetched instead, largest unattributed
// share first. On TAythDdK… that was 122 addresses holding 84.8% of traced
// value (docs/DECISIONS.md D23).
func (s *Service) deepen(ctx context.Context, chainID, address string, p Prefetcher,
	d *scoring.DepthStatus, results ...*graph.Result) error {

	weight := map[string]decimal.Decimal{}
	for _, tr := range results {
		if tr == nil || !tr.TotalTraced.IsPositive() {
			continue
		}
		for _, path := range tr.Paths {
			t := path.Terminal
			if t.Reason != "dead_end" || t.Address == "" || t.Address == address {
				continue
			}
			// Normalised per direction, so the two directions rank on the
			// same scale.
			weight[t.Address] = weight[t.Address].Add(path.Contribution.Div(tr.TotalTraced))
		}
	}
	if len(weight) == 0 {
		return nil
	}

	addrs := make([]string, 0, len(weight))
	for a := range weight {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool {
		if !weight[addrs[i]].Equal(weight[addrs[j]]) {
			return weight[addrs[i]].GreaterThan(weight[addrs[j]])
		}
		return addrs[i] < addrs[j] // docs/DECISIONS.md D6
	})

	fetched := map[string]bool{}
	rows, err := s.pg.QueryContext(ctx,
		`SELECT address FROM address_freshness WHERE chain = $1 AND address = ANY($2)`, chainID, addrs)
	if err != nil {
		return fmt.Errorf("frontier: %w", err)
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return fmt.Errorf("frontier: %w", err)
		}
		fetched[a] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, a := range addrs {
		if fetched[a] {
			d.FrontierEnded++
			continue
		}
		d.FrontierPending++
		if d.FrontierQueued < frontierCap {
			if err := p.Queue(ctx, a, 0); err != nil {
				return fmt.Errorf("frontier: %w", err)
			}
			d.FrontierQueued++
		}
	}
	return nil
}
