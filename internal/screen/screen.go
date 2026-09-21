// Package screen ties ingestion, labels, traversal and scoring into a single
// address screening operation.
package screen

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/graph"
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
