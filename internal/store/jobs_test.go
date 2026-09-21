package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestRetryDelayDoublesToCap(t *testing.T) {
	want := map[int]time.Duration{
		1: time.Minute,
		2: 2 * time.Minute,
		3: 4 * time.Minute,
		4: 8 * time.Minute,
		5: RetryBackoffCap,
		9: RetryBackoffCap,
	}
	for attempts, d := range want {
		if got := retryDelay(attempts); got != d {
			t.Errorf("retryDelay(%d) = %v, want %v", attempts, got, d)
		}
	}
}

// A failed job must not be claimable again until its backoff has passed.
// Before D24 it was, and five attempts ran inside one rate-limit window.
func TestFailedJobWaitsBeforeRetry(t *testing.T) {
	_, pg := testDBs(t)
	ctx := context.Background()
	truncateJobs(t, pg)

	jobs := NewJobs(pg)
	if err := jobs.Enqueue(ctx, "tron", "TBackoff", 0, nil); err != nil {
		t.Fatal(err)
	}
	job, err := jobs.Claim(ctx, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Fail(ctx, job.ID, job.Attempts, errors.New("trongrid rate limited (429)")); err != nil {
		t.Fatal(err)
	}

	if _, err := jobs.Claim(ctx, "w", time.Minute); !errors.Is(err, ErrNoJobs) {
		t.Fatalf("claim during backoff: got %v, want ErrNoJobs", err)
	}

	// Once the backoff has passed the job is claimable again.
	if _, err := pg.ExecContext(ctx,
		`UPDATE fetch_jobs SET not_before = now() - interval '1 second' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	again, err := jobs.Claim(ctx, "w", time.Minute)
	if err != nil {
		t.Fatalf("claim after backoff: %v", err)
	}
	if again.ID != job.ID || again.Attempts != 2 {
		t.Fatalf("reclaimed job %d attempt %d, want job %d attempt 2", again.ID, again.Attempts, job.ID)
	}

	// A job that succeeds on retry must not keep the earlier attempt's error.
	if err := jobs.Complete(ctx, again.ID); err != nil {
		t.Fatal(err)
	}
	var lastErr sql.NullString
	if err := pg.QueryRowContext(ctx,
		`SELECT last_error FROM fetch_jobs WHERE id = $1`, job.ID).Scan(&lastErr); err != nil {
		t.Fatal(err)
	}
	if lastErr.Valid {
		t.Fatalf("completed job kept last_error %q", lastErr.String)
	}
}

// A released job is not a failure and must be claimable at once, even if it
// was backing off from an earlier failure.
func TestReleasedJobIsClaimableAtOnce(t *testing.T) {
	_, pg := testDBs(t)
	ctx := context.Background()
	truncateJobs(t, pg)

	jobs := NewJobs(pg)
	if err := jobs.Enqueue(ctx, "tron", "TRelease", 0, nil); err != nil {
		t.Fatal(err)
	}
	job, err := jobs.Claim(ctx, "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ExecContext(ctx,
		`UPDATE fetch_jobs SET not_before = now() + interval '1 hour' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Release(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Claim(ctx, "w", time.Minute); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
}

func truncateJobs(t *testing.T, pg *sql.DB) {
	t.Helper()
	if _, err := pg.ExecContext(context.Background(), "TRUNCATE TABLE fetch_jobs"); err != nil {
		t.Fatalf("truncate fetch_jobs: %v", err)
	}
}
