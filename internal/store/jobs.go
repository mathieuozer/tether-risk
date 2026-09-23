package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Jobs is the demand-driven fetch queue.
//
// SPEC.md §5: ingestion is demand-driven, not full-chain. When an address is
// queried and we lack its history, enqueue a fetch for that address's
// transfers and its neighbours to the configured hop depth.
//
// A plain PostgreSQL queue drained with SELECT ... FOR UPDATE SKIP LOCKED.
// SPEC.md §11 asks for boring, inspectable code: a queue a reviewer can read
// with SQL beats a broker they cannot.
type Jobs struct{ pg *sql.DB }

func NewJobs(pg *sql.DB) *Jobs { return &Jobs{pg: pg} }

// Job is one unit of fetch work.
type Job struct {
	ID             int64
	Chain          string
	Address        string
	DepthRemaining int
	RunID          sql.NullInt64
	Attempts       int
	MaxAttempts    int
}

// ErrNoJobs signals an empty queue, which is the normal idle state rather than
// a failure.
var ErrNoJobs = errors.New("no claimable jobs")

// Enqueue adds a fetch job. The partial unique index on (chain, address) for
// live jobs means a duplicate is silently ignored — a fan-out that reaches the
// same busy address from several directions must not queue it many times and
// spend the rate-limit budget on duplicate work.
//
// When the address is already queued at a shallower depth, the depth is
// raised: the deeper request is the one that must be satisfied.
func (j *Jobs) Enqueue(ctx context.Context, chainID, address string, depth int, runID *int64) error {
	_, err := j.pg.ExecContext(ctx, `
		INSERT INTO fetch_jobs (chain, address, depth_remaining, run_id, priority)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (chain, address) WHERE state IN ('pending','running')
		DO UPDATE SET
			depth_remaining = GREATEST(fetch_jobs.depth_remaining, EXCLUDED.depth_remaining),
			priority        = LEAST(fetch_jobs.priority, EXCLUDED.priority)`,
		chainID, address, depth, runID, 100-depth)
	if err != nil {
		return fmt.Errorf("enqueue %s/%s: %w", chainID, address, err)
	}
	return nil
}

// BackgroundPriority is for work no customer is waiting on, such as the
// labeler's deposit candidates. Lower numbers run first; customer screens
// queue at 99-100, so background work never delays them.
const BackgroundPriority = 1000

// EnqueueBackground queues a fetch no customer is waiting on. If the address
// is already queued, its priority is left as it is: a customer's claim on it
// stays first.
func (j *Jobs) EnqueueBackground(ctx context.Context, chainID, address string) error {
	_, err := j.pg.ExecContext(ctx, `
		INSERT INTO fetch_jobs (chain, address, depth_remaining, priority)
		VALUES ($1, $2, 0, $3)
		ON CONFLICT (chain, address) WHERE state IN ('pending','running') DO NOTHING`,
		chainID, address, BackgroundPriority)
	if err != nil {
		return fmt.Errorf("enqueue background %s/%s: %w", chainID, address, err)
	}
	return nil
}

// Claim leases the highest-priority pending job for a worker.
//
// SKIP LOCKED lets many workers drain the queue concurrently without blocking
// each other. The lease matters for correctness, not just liveness: the
// pre-insert deduplication in TransferWriter is a read-then-write, and it is
// only safe because exactly one worker holds an address at a time.
func (j *Jobs) Claim(ctx context.Context, workerID string, lease time.Duration) (*Job, error) {
	return j.ClaimBelow(ctx, "", workerID, lease, 0)
}

// ClaimBelow claims like Claim, but only a job for chainID ("" means any)
// whose priority number is below maxPriority (0 means any). With
// BackgroundPriority it takes customer work only, which is what the worker
// does once the day's background budget is spent (docs/DECISIONS.md D35).
// A worker runs one chain's adapter, so it must take only that chain's jobs:
// before D40 a TRON worker would have claimed Ethereum jobs.
func (j *Jobs) ClaimBelow(ctx context.Context, chainID, workerID string, lease time.Duration, maxPriority int) (*Job, error) {
	var job Job
	err := j.pg.QueryRowContext(ctx, `
		UPDATE fetch_jobs SET
			state        = 'running',
			leased_by    = $1,
			leased_until = now() + $2::interval,
			attempts     = attempts + 1,
			-- now(), not COALESCE(started_at, now()): started_at marks when the
			-- current attempt began, so finished_at - started_at measures work
			-- done. Pinning it to the first-ever claim made that difference
			-- measure queue wait plus every retry, which reported an average of
			-- 862s against jobs that visibly took 3 to 33 seconds. Queue entry
			-- is already recorded by created_at.
			started_at   = now()
		WHERE id = (
			SELECT id FROM fetch_jobs
			WHERE state = 'pending'
			  AND (not_before IS NULL OR not_before <= now())
			  AND ($3 = 0 OR priority < $3)
			  AND ($4 = '' OR chain = $4)
			ORDER BY priority, created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, chain, address, depth_remaining, run_id, attempts, max_attempts`,
		workerID, fmt.Sprintf("%d seconds", int(lease.Seconds())), maxPriority, chainID,
	).Scan(&job.ID, &job.Chain, &job.Address, &job.DepthRemaining,
		&job.RunID, &job.Attempts, &job.MaxAttempts)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoJobs
	}
	if err != nil {
		return nil, fmt.Errorf("claim job: %w", err)
	}
	return &job, nil
}

// Complete marks a job done. It clears last_error, which would otherwise
// leave a finished job reading as if its final attempt had failed.
func (j *Jobs) Complete(ctx context.Context, id int64) error {
	_, err := j.pg.ExecContext(ctx, `
		UPDATE fetch_jobs
		SET state = 'done', finished_at = now(), leased_by = NULL, leased_until = NULL,
		    last_error = NULL
		WHERE id = $1`, id)
	return err
}

// RetryBackoff is the wait after a job's first failure. It doubles with each
// further failure, up to RetryBackoffCap: 1, 2, 4 and 8 minutes with the
// default five attempts, so a job survives a quarter of an hour of rate
// limiting before it is abandoned.
const (
	RetryBackoff    = time.Minute
	RetryBackoffCap = 15 * time.Minute
)

// retryDelay is the wait before a job that has failed `attempts` times may be
// claimed again.
func retryDelay(attempts int) time.Duration {
	d := RetryBackoff
	for i := 1; i < attempts && d < RetryBackoffCap; i++ {
		d *= 2
	}
	return min(d, RetryBackoffCap)
}

// Fail records an error and returns the job to the queue after a backoff, or
// abandons it once attempts are exhausted.
//
// The backoff matters because most failures are rate limits, which clear with
// time and not with retries. Returning the job at once let a worker reclaim it
// within a second and spend every attempt inside one 429 window
// (docs/DECISIONS.md D24).
//
// An abandoned job is not a silent loss: the address keeps no freshness entry,
// so any query needing it will find it missing and the result will report
// reduced coverage rather than pretending the history was complete.
func (j *Jobs) Fail(ctx context.Context, id int64, attempts int, cause error) error {
	_, err := j.pg.ExecContext(ctx, `
		UPDATE fetch_jobs SET
			state = CASE WHEN attempts >= max_attempts THEN 'abandoned'::job_state
			             ELSE 'pending'::job_state END,
			last_error   = $2,
			leased_by    = NULL,
			leased_until = NULL,
			not_before   = CASE WHEN attempts >= max_attempts THEN NULL
			                    ELSE now() + $3::interval END,
			finished_at  = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END
		WHERE id = $1`, id, cause.Error(),
		fmt.Sprintf("%d seconds", int(retryDelay(attempts).Seconds())))
	return err
}

// Release returns a job to the queue without the attempt counting against it.
//
// Used on shutdown. A worker stopping is not the job failing, and counting it
// as one means a job unlucky enough to be in flight during a few restarts gets
// abandoned for reasons that have nothing to do with the address. Abandoned
// jobs leave no freshness entry, so the address simply never gets fetched and
// the resulting coverage gap has no visible cause.
func (j *Jobs) Release(ctx context.Context, id int64) error {
	_, err := j.pg.ExecContext(ctx, `
		UPDATE fetch_jobs SET
			state        = 'pending',
			attempts     = GREATEST(attempts - 1, 0),
			leased_by    = NULL,
			leased_until = NULL,
			not_before   = NULL
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("release job %d: %w", id, err)
	}
	return nil
}

// ReclaimExpired returns jobs whose lease ran out to the pending queue. A
// worker that crashed mid-job must not strand its address forever.
func (j *Jobs) ReclaimExpired(ctx context.Context) (int64, error) {
	res, err := j.pg.ExecContext(ctx, `
		UPDATE fetch_jobs SET
			state = 'pending', leased_by = NULL, leased_until = NULL
		WHERE state = 'running' AND leased_until < now()`)
	if err != nil {
		return 0, fmt.Errorf("reclaim expired leases: %w", err)
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// Freshness / TTL cache
// ---------------------------------------------------------------------------

// Freshness describes what is already stored for an address.
type Freshness struct {
	FetchedAt     time.Time
	FetchedDepth  int
	TransferCount int64
	Truncated     bool
	Found         bool
}

// Freshness reads the cache entry for an address.
func (j *Jobs) Freshness(ctx context.Context, chainID, address string) (Freshness, error) {
	var f Freshness
	err := j.pg.QueryRowContext(ctx, `
		SELECT fetched_at, fetched_depth, transfer_count, truncated
		FROM address_freshness WHERE chain = $1 AND address = $2`,
		chainID, address).Scan(&f.FetchedAt, &f.FetchedDepth, &f.TransferCount, &f.Truncated)

	if errors.Is(err, sql.ErrNoRows) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("read freshness: %w", err)
	}
	f.Found = true
	return f, nil
}

// IsFresh reports whether stored history is recent enough and deep enough to
// serve a query without refetching.
//
// Depth is checked as well as age on purpose: an address fetched to depth 1 is
// useless to a query that needs depth 3, however recently it was fetched.
func (f Freshness) IsFresh(ttl time.Duration, neededDepth int) bool {
	if !f.Found {
		return false
	}
	if f.FetchedDepth < neededDepth {
		return false
	}
	return time.Since(f.FetchedAt) < ttl
}

// MarkFetched records that an address's history is now stored.
func (j *Jobs) MarkFetched(ctx context.Context, chainID, address string, depth int, count int64, truncated bool, reason string) error {
	var r any
	if reason != "" {
		r = reason
	}
	_, err := j.pg.ExecContext(ctx, `
		INSERT INTO address_freshness
			(chain, address, fetched_at, fetched_depth, transfer_count, truncated, truncated_reason)
		VALUES ($1, $2, now(), $3, $4, $5, $6)
		ON CONFLICT (chain, address) DO UPDATE SET
			fetched_at       = now(),
			-- Keep the deepest fetch we have ever achieved rather than the
			-- most recent one, so a shallow refresh does not discard depth
			-- already paid for.
			fetched_depth    = GREATEST(address_freshness.fetched_depth, EXCLUDED.fetched_depth),
			transfer_count   = EXCLUDED.transfer_count,
			truncated        = EXCLUDED.truncated,
			truncated_reason = EXCLUDED.truncated_reason`,
		chainID, address, depth, count, truncated, r)
	if err != nil {
		return fmt.Errorf("mark fetched: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cursors
// ---------------------------------------------------------------------------

// Cursor reads a stored pagination cursor.
func (j *Jobs) Cursor(ctx context.Context, chainID, scope string) (string, error) {
	var v sql.NullString
	err := j.pg.QueryRowContext(ctx,
		`SELECT cursor_value FROM ingest_cursors WHERE chain = $1 AND scope = $2`,
		chainID, scope).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read cursor: %w", err)
	}
	return v.String, nil
}

// SaveCursor persists a pagination cursor so a fetch resumes where it stopped
// rather than restarting (SPEC.md §5).
func (j *Jobs) SaveCursor(ctx context.Context, chainID, scope, value string) error {
	_, err := j.pg.ExecContext(ctx, `
		INSERT INTO ingest_cursors (chain, scope, cursor_value, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (chain, scope) DO UPDATE SET
			cursor_value = EXCLUDED.cursor_value,
			updated_at   = now()`,
		chainID, scope, value)
	if err != nil {
		return fmt.Errorf("save cursor: %w", err)
	}
	return nil
}

// ClearCursor removes a cursor once an address is fully drained.
func (j *Jobs) ClearCursor(ctx context.Context, chainID, scope string) error {
	_, err := j.pg.ExecContext(ctx,
		`DELETE FROM ingest_cursors WHERE chain = $1 AND scope = $2`, chainID, scope)
	return err
}

// QueueStats is a snapshot of queue depth by state, for operational visibility.
func (j *Jobs) QueueStats(ctx context.Context) (map[string]int64, error) {
	rows, err := j.pg.QueryContext(ctx,
		`SELECT state::text, count(*) FROM fetch_jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var state string
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}
