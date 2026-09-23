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

// Background work never delays a customer: a customer's job queued later is
// claimed first, and a customer asking for an address already waiting in the
// background lifts it to the customer's priority.
func TestBackgroundJobsYieldToCustomers(t *testing.T) {
	_, pg := testDBs(t)
	ctx := context.Background()
	truncateJobs(t, pg)
	jobs := NewJobs(pg)

	for _, a := range []string{"TBg1", "TBg2", "TShared"} {
		if err := jobs.EnqueueBackground(ctx, "tron", a); err != nil {
			t.Fatal(err)
		}
	}
	if err := jobs.Enqueue(ctx, "tron", "TCustomer", 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Enqueue(ctx, "tron", "TShared", 0, nil); err != nil {
		t.Fatal(err)
	}

	var order []string
	for i := 0; i < 4; i++ {
		j, err := jobs.Claim(ctx, "w", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, j.Address)
	}
	if order[0] != "TShared" && order[0] != "TCustomer" || order[1] != "TShared" && order[1] != "TCustomer" {
		t.Fatalf("claim order %v: customer jobs must come first", order)
	}
}

// Once the day's background budget is spent the worker claims below
// BackgroundPriority: a customer's job still runs, background work waits
// (docs/DECISIONS.md D35).
func TestClaimBelowSkipsBackgroundWork(t *testing.T) {
	_, pg := testDBs(t)
	ctx := context.Background()
	truncateJobs(t, pg)

	jobs := NewJobs(pg)
	if err := jobs.EnqueueBackground(ctx, "tron", "TBackground"); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.ClaimBelow(ctx, "w", time.Minute, BackgroundPriority); !errors.Is(err, ErrNoJobs) {
		t.Fatalf("background job claimed under the ceiling: %v", err)
	}
	if err := jobs.Enqueue(ctx, "tron", "TCustomer", 1, nil); err != nil {
		t.Fatal(err)
	}
	job, err := jobs.ClaimBelow(ctx, "w", time.Minute, BackgroundPriority)
	if err != nil || job.Address != "TCustomer" {
		t.Fatalf("customer job not claimed: %v %v", job, err)
	}
	if job, err := jobs.Claim(ctx, "w", time.Minute); err != nil || job.Address != "TBackground" {
		t.Fatalf("without a ceiling background work runs: %v %v", job, err)
	}
}

func TestAPIUsageAccumulatesPerDay(t *testing.T) {
	_, pg := testDBs(t)
	ctx := context.Background()
	if _, err := pg.Exec(`DELETE FROM api_usage WHERE provider = 'test'`); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int64{120, 0, 30} {
		if err := AddAPIUsage(ctx, pg, "test", n); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := APIUsageToday(ctx, pg, "test"); err != nil || n != 150 {
		t.Fatalf("usage = %d, %v; want 150", n, err)
	}
}
