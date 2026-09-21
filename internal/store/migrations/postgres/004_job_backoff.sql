-- 004_job_backoff.sql — a failed job waits before it is retried.
--
-- docs/DECISIONS.md D24: Fail used to return a job to `pending` at once, so a
-- worker reclaimed it within a second. Five attempts then ran inside one
-- rate-limit window and the address was abandoned for a cause that had
-- nothing to do with it.

BEGIN;

-- Earliest time a pending job may be claimed. NULL means now.
ALTER TABLE fetch_jobs ADD COLUMN IF NOT EXISTS not_before TIMESTAMPTZ;

COMMIT;
