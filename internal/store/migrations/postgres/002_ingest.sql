-- 002_ingest.sql — ingestion cursors, the demand-driven job queue, and the
-- ingest ledger that keeps `edges` honest.
--
-- SPEC.md §5: ingestion is demand-driven, not full-chain. When an address is
-- queried and we lack its history, enqueue a fetch for that address's
-- transfers and its neighbours to the configured hop depth. Cache with a TTL.

BEGIN;

-- ---------------------------------------------------------------------------
-- Resumable cursors
-- ---------------------------------------------------------------------------
-- SPEC.md §5 requires a resumable cursor persisted in PostgreSQL. TronGrid
-- paginates by opaque fingerprint, so the cursor is stored as text rather than
-- a block height.

CREATE TABLE IF NOT EXISTS ingest_cursors (
    chain           TEXT        NOT NULL,
    scope           TEXT        NOT NULL,   -- 'address:<addr>:trc20' | 'range'
    cursor_value    TEXT,                   -- NULL means "start from the beginning"
    last_block      BIGINT,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain, scope)
);

-- ---------------------------------------------------------------------------
-- Address freshness / TTL cache
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS address_freshness (
    chain           TEXT        NOT NULL,
    address         TEXT        NOT NULL,

    -- When this address's history was last fully fetched.
    fetched_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Fetching is complete only to this hop depth from the originally queried
    -- address. A later query needing deeper expansion must refetch.
    fetched_depth   INT         NOT NULL DEFAULT 0,

    transfer_count  BIGINT      NOT NULL DEFAULT 0,

    -- Set when an address is too large to fetch fully. Tracing through a
    -- truncated address understates its exposure, and the result must say so
    -- rather than pretend the history was complete.
    truncated       BOOLEAN     NOT NULL DEFAULT false,
    truncated_reason TEXT,

    PRIMARY KEY (chain, address)
);

CREATE INDEX IF NOT EXISTS address_freshness_stale
    ON address_freshness (fetched_at);

-- ---------------------------------------------------------------------------
-- Job queue
-- ---------------------------------------------------------------------------
-- A plain PostgreSQL queue drained with SELECT ... FOR UPDATE SKIP LOCKED.
-- SPEC.md §11 asks for boring, inspectable code; a queue a reviewer can read
-- with SQL beats a broker they cannot.

CREATE TYPE job_state AS ENUM ('pending', 'running', 'done', 'failed', 'abandoned');

CREATE TABLE IF NOT EXISTS fetch_jobs (
    id              BIGSERIAL PRIMARY KEY,
    chain           TEXT        NOT NULL,
    address         TEXT        NOT NULL,

    -- Remaining hops to expand from this address. 0 means fetch this address
    -- only and enqueue nothing further.
    depth_remaining INT         NOT NULL,

    -- The screening run that caused this job, for tracing work back to a query.
    run_id          BIGINT,

    state           job_state   NOT NULL DEFAULT 'pending',
    priority        INT         NOT NULL DEFAULT 100,  -- lower runs first

    attempts        INT         NOT NULL DEFAULT 0,
    max_attempts    INT         NOT NULL DEFAULT 5,
    last_error      TEXT,

    -- Lease held by a worker while running, so a crashed worker's job is
    -- reclaimed rather than lost.
    leased_by       TEXT,
    leased_until    TIMESTAMPTZ,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,

    CONSTRAINT fetch_jobs_depth_sane CHECK (depth_remaining >= 0)
);

-- At most one live job per address. Without this a fan-out of neighbours
-- enqueues the same busy address dozens of times and the workers fight over
-- rate limit for duplicate work.
CREATE UNIQUE INDEX IF NOT EXISTS fetch_jobs_one_live_per_address
    ON fetch_jobs (chain, address)
    WHERE state IN ('pending', 'running');

CREATE INDEX IF NOT EXISTS fetch_jobs_claimable
    ON fetch_jobs (priority, created_at)
    WHERE state = 'pending';

CREATE INDEX IF NOT EXISTS fetch_jobs_expired_leases
    ON fetch_jobs (leased_until)
    WHERE state = 'running';

-- ---------------------------------------------------------------------------
-- Ingest ledger
-- ---------------------------------------------------------------------------
-- The second of the three defences against `edges` double-counting
-- (docs/DECISIONS.md D2). A materialized view in ClickHouse aggregates every
-- row it is handed, including rows that ReplacingMergeTree will later collapse
-- in `transfers`. So a repeated insert permanently inflates `edges`, silently,
-- and every score that reads it is wrong.
--
-- This ledger records what each batch actually wrote. A job that has already
-- written a batch is a no-op on re-run rather than a second insert.

CREATE TABLE IF NOT EXISTS ingest_batches (
    id              BIGSERIAL PRIMARY KEY,
    chain           TEXT        NOT NULL,
    address         TEXT        NOT NULL,

    -- Identifies the exact page of upstream data this batch came from, so a
    -- retry of the same page is recognisable.
    page_key        TEXT        NOT NULL,

    -- Rows the adapter returned, and rows actually inserted after
    -- deduplication against existing keys. A persistent gap between the two
    -- is the expected steady state on re-fetch, not a fault.
    rows_fetched    BIGINT      NOT NULL,
    rows_inserted   BIGINT      NOT NULL,

    -- Hash over the inserted (tx_hash, log_index) keys. Lets a rebuild verify
    -- it reproduced exactly what was originally written.
    content_hash    TEXT        NOT NULL,

    written_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ingest_batches_counts_sane CHECK (
        rows_inserted >= 0 AND rows_inserted <= rows_fetched
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS ingest_batches_unique
    ON ingest_batches (chain, address, page_key);

COMMENT ON TABLE ingest_batches IS
    'Ingest ledger. Makes re-running a fetch a no-op so the edges materialized view never sees a duplicate.';

COMMIT;
