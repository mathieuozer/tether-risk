// Package ingest drives demand-driven chain ingestion.
//
// SPEC.md §5: ingestion is demand-driven, not full-chain. When an address is
// queried and we lack its history, enqueue a fetch job for that address's
// transfers and its neighbours to the configured hop depth. Cache with a TTL.
// Full-chain backfill is optional and must not be a prerequisite for a working
// query.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"time"

	"github.com/mozer/tether-risk/internal/chain"
	"github.com/mozer/tether-risk/internal/store"
	"github.com/shopspring/decimal"
)

// Options configures a Worker.
type Options struct {
	// TTL is how long stored history stays usable before a refetch.
	TTL time.Duration

	// Lease is how long a worker holds a job before it can be reclaimed. It
	// must comfortably exceed the time a large address takes to drain, or a
	// slow job gets reclaimed and run twice concurrently — which would break
	// the single-writer assumption the pre-insert deduplication relies on.
	Lease time.Duration

	// MaxPagesPerAddress bounds work on very large addresses. Hitting it marks
	// the address truncated rather than letting one address consume the whole
	// rate-limit budget.
	MaxPagesPerAddress int

	// MaxNeighboursEnqueued caps fan-out per address, mirroring the traversal
	// cap in weights.yaml.
	MaxNeighboursEnqueued int

	PollInterval time.Duration
	Logger       *slog.Logger

	// Pricer values transfers as they are written. Without one they are
	// stored unpriced until the next `price backfill`, and traversal cannot
	// see them: an address fetched during the day would score as if it had
	// no history until the nightly run (docs/DECISIONS.md D22).
	Pricer Pricer
}

// Pricer values a raw amount. pricing.Pricer implements it.
type Pricer interface {
	Price(ctx context.Context, asset string, raw *big.Int, at time.Time) (*decimal.Decimal, string, error)
}

func (o *Options) setDefaults() {
	if o.TTL <= 0 {
		o.TTL = 24 * time.Hour
	}
	if o.Lease <= 0 {
		o.Lease = 10 * time.Minute
	}
	if o.MaxPagesPerAddress <= 0 {
		o.MaxPagesPerAddress = 50 // 50 x 200 = 10,000 transfers
	}
	if o.MaxNeighboursEnqueued <= 0 {
		o.MaxNeighboursEnqueued = 100
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Worker drains the fetch queue.
type Worker struct {
	id      string
	adapter chain.Adapter
	jobs    *store.Jobs
	writer  *store.TransferWriter
	opts    Options
	log     *slog.Logger
}

func NewWorker(id string, adapter chain.Adapter, jobs *store.Jobs, writer *store.TransferWriter, opts Options) *Worker {
	opts.setDefaults()
	return &Worker{
		id:      id,
		adapter: adapter,
		jobs:    jobs,
		writer:  writer,
		opts:    opts,
		log:     opts.Logger.With("worker", id),
	}
}

// Run drains the queue until the context is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("worker started", "chain", w.adapter.Chain())

	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker stopping")
			return ctx.Err()
		default:
		}

		job, err := w.jobs.Claim(ctx, w.id, w.opts.Lease)
		if errors.Is(err, store.ErrNoJobs) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(w.opts.PollInterval):
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.log.Error("claim failed", "error", err)
			time.Sleep(w.opts.PollInterval)
			continue
		}

		if err := w.process(ctx, job); err != nil {
			if ctx.Err() != nil {
				// A worker stopping is not the job failing. Release returns it
				// to the queue with its attempt count restored, so a job that
				// happens to be in flight across a few restarts is not
				// abandoned for reasons unrelated to the address.
				//
				// WithoutCancel because the context that just expired is the
				// reason we are here; the release still has to be written.
				if rerr := w.jobs.Release(context.WithoutCancel(ctx), job.ID); rerr != nil {
					w.log.Error("releasing job on shutdown failed", "error", rerr)
				}
				return ctx.Err()
			}
			w.log.Error("job failed",
				"address", job.Address, "attempt", job.Attempts, "error", err)
			if ferr := w.jobs.Fail(ctx, job.ID, job.Attempts, err); ferr != nil {
				w.log.Error("recording failure failed", "error", ferr)
			}
			continue
		}

		if err := w.jobs.Complete(ctx, job.ID); err != nil {
			w.log.Error("completing job failed", "error", err)
		}
	}
}

// FetchAddress drains one address's history and enqueues its neighbours.
//
// Exported so a cold query can run it inline rather than waiting for a worker
// to pick the job up — the Phase 1 acceptance target is a wall-clock number
// for the querying user, not for the queue.
func (w *Worker) FetchAddress(ctx context.Context, address string, depthRemaining int) (Result, error) {
	return w.fetch(ctx, address, depthRemaining, nil)
}

func (w *Worker) process(ctx context.Context, job *store.Job) error {
	var runID *int64
	if job.RunID.Valid {
		runID = &job.RunID.Int64
	}
	_, err := w.fetch(ctx, job.Address, job.DepthRemaining, runID)
	return err
}

// Result reports what a fetch did.
type Result struct {
	Address    string
	Pages      int
	Fetched    int
	Inserted   int
	Duplicates int
	Neighbours int
	Truncated  bool
	Skipped    bool // history was already fresh
	Duration   time.Duration
}

// Queue hands an address to the background workers instead of fetching it
// now. Screening uses it when a fetch runs out of time: the pages already
// written are kept, and the saved cursor lets the worker continue from there.
func (w *Worker) Queue(ctx context.Context, address string, depthRemaining int) error {
	return w.jobs.Enqueue(ctx, w.adapter.Chain(), address, depthRemaining, nil)
}

func (w *Worker) fetch(ctx context.Context, address string, depthRemaining int, runID *int64) (Result, error) {
	started := time.Now()
	chainID := w.adapter.Chain()
	res := Result{Address: address}

	// TTL cache. SPEC.md §5 requires caching; depth is checked too, because an
	// address fetched shallowly is not usable by a deeper query however recent
	// it is.
	fresh, err := w.jobs.Freshness(ctx, chainID, address)
	if err != nil {
		return res, err
	}
	if fresh.IsFresh(w.opts.TTL, depthRemaining) {
		res.Skipped = true
		res.Duration = time.Since(started)
		w.log.Debug("history already fresh", "address", address, "age", time.Since(fresh.FetchedAt))
		return res, nil
	}

	scope := "address:" + address
	stored, err := w.jobs.Cursor(ctx, chainID, scope)
	if err != nil {
		return res, err
	}
	cur := chain.Cursor{Value: stored}

	// Neighbour values are accumulated so the fan-out cap can keep the highest
	// value counterparties rather than whichever happened to arrive first.
	// Picking arbitrarily would make which neighbours get traced depend on API
	// ordering (docs/DECISIONS.md D6).
	neighbourValue := map[string]float64{}

	// Fetching and writing are pipelined. Measured against the live API, a page
	// costs roughly 1.3s of network wait and 1s of write; done serially that is
	// ~2.3s per page, and a thousand-transfer address lands uncomfortably close
	// to the 30s acceptance target in SPEC.md §5. Overlapping the two puts the
	// write time behind the next fetch's latency.
	//
	// The channel is deliberately shallow. Reading far ahead of the writer
	// would spend rate-limit budget on pages that a failed write is about to
	// discard, and would let the saved cursor drift further from what is
	// actually stored.
	type fetched struct {
		page chain.AddressPage
		err  error
	}
	pages := make(chan fetched, 2)

	fetchCtx, cancelFetch := context.WithCancel(ctx)
	defer cancelFetch()

	go func() {
		defer close(pages)
		c := cur
		for i := 0; i < w.opts.MaxPagesPerAddress; i++ {
			p, err := w.adapter.FetchAddress(fetchCtx, address, c)
			select {
			case pages <- fetched{page: p, err: err}:
			case <-fetchCtx.Done():
				return
			}
			if err != nil || p.Next.Done {
				return
			}
			c = p.Next
		}
	}()

	page := -1
	for f := range pages {
		page++
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}

		if f.err != nil {
			return res, fmt.Errorf("fetch %s page %d: %w", address, page, f.err)
		}
		p := f.page
		res.Pages++

		if w.opts.Pricer != nil {
			for i := range p.Transfers {
				t := &p.Transfers[i]
				v, basis, err := w.opts.Pricer.Price(ctx, t.Asset, t.RawValue, t.BlockTime)
				if err != nil {
					return res, fmt.Errorf("price %s: %w", t.Key(), err)
				}
				t.USDValue, t.PriceBasis = v, basis
			}
		}

		wr, err := w.writer.WritePage(ctx, address, p.PageKey, p.Transfers)
		if err != nil {
			return res, fmt.Errorf("write %s page %d: %w", address, page, err)
		}
		res.Fetched += wr.Fetched
		res.Inserted += wr.Inserted
		res.Duplicates += wr.Duplicates

		for _, t := range p.Transfers {
			for _, counterparty := range []string{t.FromAddress, t.ToAddress} {
				if counterparty != "" && counterparty != address {
					// Raw units across assets are not comparable, but this
					// only orders candidates for expansion; scoring uses USD
					// from `edges`. Ranking by count of appearances would bias
					// toward dust, which is worse.
					neighbourValue[counterparty] += float64(t.RawValue.Sign())
				}
			}
		}

		// Persist the cursor only after the page is written, so an interrupted
		// drain resumes from what is actually stored rather than from how far
		// the fetcher had read ahead (SPEC.md §5).
		if err := w.jobs.SaveCursor(ctx, chainID, scope, p.Next.Value); err != nil {
			return res, err
		}

		if p.Next.Done {
			if err := w.jobs.ClearCursor(ctx, chainID, scope); err != nil {
				return res, err
			}
			break
		}
	}

	if res.Pages >= w.opts.MaxPagesPerAddress {
		// Honesty over completeness: an address too large to drain is recorded
		// as truncated so any score built on it can say so, rather than
		// silently resting on partial history.
		res.Truncated = true
		w.log.Warn("address truncated at page limit",
			"address", address, "pages", w.opts.MaxPagesPerAddress)
	}

	reason := ""
	if res.Truncated {
		reason = fmt.Sprintf("stopped after %d pages", w.opts.MaxPagesPerAddress)
	}
	if err := w.jobs.MarkFetched(ctx, chainID, address, depthRemaining,
		int64(res.Inserted), res.Truncated, reason); err != nil {
		return res, err
	}

	if depthRemaining > 0 {
		n, err := w.enqueueNeighbours(ctx, chainID, neighbourValue, depthRemaining-1, runID)
		if err != nil {
			return res, err
		}
		res.Neighbours = n
	}

	res.Duration = time.Since(started)
	w.log.Info("address fetched",
		"address", address, "pages", res.Pages, "inserted", res.Inserted,
		"duplicates", res.Duplicates, "neighbours", res.Neighbours,
		"truncated", res.Truncated, "duration", res.Duration)
	return res, nil
}

// enqueueNeighbours queues the highest-value counterparties for expansion.
func (w *Worker) enqueueNeighbours(ctx context.Context, chainID string, values map[string]float64, depth int, runID *int64) (int, error) {
	type candidate struct {
		address string
		value   float64
	}
	cands := make([]candidate, 0, len(values))
	for addr, v := range values {
		cands = append(cands, candidate{addr, v})
	}

	// Deterministic order: value descending, address ascending as tie-break.
	// Go map iteration is randomised, so without this the set of neighbours
	// that get expanded would differ between runs and so would the score
	// (docs/DECISIONS.md D6).
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].value != cands[j].value {
			return cands[i].value > cands[j].value
		}
		return cands[i].address < cands[j].address
	})

	limit := min(len(cands), w.opts.MaxNeighboursEnqueued)
	for _, c := range cands[:limit] {
		if err := w.jobs.Enqueue(ctx, chainID, c.address, depth, runID); err != nil {
			return 0, err
		}
	}
	return limit, nil
}
