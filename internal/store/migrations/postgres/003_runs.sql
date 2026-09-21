-- 003_runs.sql — screening runs, the paths that produced each score, and
-- price data.
--
-- SPEC.md §2: "every score must be reconstructible from stored intermediate
-- data. Persist the traced paths, not just the final number." This schema is
-- what makes that true. A score with no surviving path set is not auditable
-- and should be treated as invalid.

BEGIN;

-- ---------------------------------------------------------------------------
-- Runs
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS runs (
    id                  BIGSERIAL PRIMARY KEY,

    chain               TEXT         NOT NULL,
    address             TEXT         NOT NULL,
    direction           TEXT         NOT NULL,  -- 'inbound' | 'outbound' | 'both'

    -- The two version stamps that make a run reproducible. SPEC.md §2
    -- requires identical input plus identical snapshot to reproduce exactly.
    label_snapshot_id   BIGINT       NOT NULL REFERENCES label_snapshots(id),
    config_version      TEXT         NOT NULL,  -- SHA-256 prefix of the config files

    score               NUMERIC(6,3),
    band                TEXT,

    -- SPEC.md §7: coverage is mandatory in every response.
    coverage            NUMERIC(6,5),
    low_confidence      BOOLEAN      NOT NULL DEFAULT false,

    -- SPEC.md §7: a direct sanctions hit reports High regardless of score.
    -- Recorded separately so the override is visible rather than implied by
    -- a band that does not match the number beside it.
    sanctions_override  BOOLEAN      NOT NULL DEFAULT false,

    total_traced_usd    NUMERIC(38,6),
    attributed_usd      NUMERIC(38,6),

    -- Traversal honesty flags. SPEC.md §7 requires recording when the
    -- neighbour cap was hit so the result admits it is truncated.
    hops_used           INT,
    nodes_visited       INT,
    edges_considered    BIGINT,
    fanout_capped       BOOLEAN      NOT NULL DEFAULT false,
    hop_limit_reached   BOOLEAN      NOT NULL DEFAULT false,

    started_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ,
    duration_ms         INT,

    -- 'ok' | 'chain_unavailable' | 'error'
    status              TEXT         NOT NULL DEFAULT 'ok',
    error               TEXT,

    CONSTRAINT runs_direction_valid CHECK (direction IN ('inbound','outbound','both')),
    CONSTRAINT runs_coverage_range CHECK (coverage IS NULL OR (coverage >= 0 AND coverage <= 1))
);

CREATE INDEX IF NOT EXISTS runs_lookup ON runs (chain, address, started_at DESC);
CREATE INDEX IF NOT EXISTS runs_by_snapshot ON runs (label_snapshot_id);

-- ---------------------------------------------------------------------------
-- Category breakdown
-- ---------------------------------------------------------------------------
-- SPEC.md §1: exposure by counterparty category as percentages of value,
-- computed separately for inbound and outbound.

CREATE TABLE IF NOT EXISTS run_categories (
    run_id          BIGINT       NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    direction       TEXT         NOT NULL,
    category        TEXT         NOT NULL,

    usd_value       NUMERIC(38,6) NOT NULL,
    pct             NUMERIC(8,5)  NOT NULL,   -- share of total traced value
    weight          NUMERIC(6,2)  NOT NULL,   -- the weight applied, from config
    contribution    NUMERIC(8,5)  NOT NULL,   -- pct * weight / 100

    PRIMARY KEY (run_id, direction, category),
    CONSTRAINT run_categories_direction_valid CHECK (direction IN ('inbound','outbound'))
);

-- The weight is stored alongside rather than looked up at read time on
-- purpose: a score must remain explicable after the config changes.

-- ---------------------------------------------------------------------------
-- Paths
-- ---------------------------------------------------------------------------
-- SPEC.md §2 and §8: persist the traced paths, and report the top contributing
-- paths in plain language.

CREATE TABLE IF NOT EXISTS run_paths (
    id              BIGSERIAL PRIMARY KEY,
    run_id          BIGINT       NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    direction       TEXT         NOT NULL,

    -- Ordered addresses from the queried address to the terminal node.
    -- Kept as an array so a path is one row and reconstruction is a single
    -- read rather than a join per hop.
    hops            TEXT[]       NOT NULL,
    hop_count       INT          NOT NULL,

    -- Where the path ended, and why it counted.
    terminal_address   TEXT      NOT NULL,
    terminal_entity    TEXT,
    terminal_category  TEXT      NOT NULL,
    terminal_source    TEXT,
    terminal_confidence NUMERIC(3,2),

    -- The haircut arithmetic, stored so it can be re-checked by hand:
    --   contribution = value_share(hop1) * ... * value_share(hopN) * decay^N
    value_shares    NUMERIC(12,10)[] NOT NULL,
    decay_applied   NUMERIC(6,5)     NOT NULL,
    contribution    NUMERIC(20,12)   NOT NULL,
    usd_value       NUMERIC(38,6)    NOT NULL,

    -- True when the path ended because it ran out of hops or hit the fan-out
    -- cap rather than reaching a labelled node. These are unattributed and
    -- count against coverage.
    truncated       BOOLEAN      NOT NULL DEFAULT false,

    CONSTRAINT run_paths_direction_valid CHECK (direction IN ('inbound','outbound')),
    CONSTRAINT run_paths_shares_match_hops CHECK (
        array_length(value_shares, 1) IS NOT DISTINCT FROM array_length(hops, 1)
    )
);

CREATE INDEX IF NOT EXISTS run_paths_by_contribution
    ON run_paths (run_id, direction, contribution DESC);

CREATE INDEX IF NOT EXISTS run_paths_by_category
    ON run_paths (run_id, terminal_category);

-- ---------------------------------------------------------------------------
-- Prices
-- ---------------------------------------------------------------------------
-- docs/PLAN.md F3: no price feed credential is available, so native assets are
-- valued from a daily close series. Daily granularity on a volatile asset
-- introduces real error on any individual transfer. That limitation is
-- recorded in docs/METHODOLOGY.md and surfaced through price_basis rather than
-- being hidden behind a confident-looking number.

CREATE TABLE IF NOT EXISTS prices (
    asset           TEXT          NOT NULL,
    price_date      DATE          NOT NULL,
    usd             NUMERIC(38,12) NOT NULL,

    -- 'pinned'     - stablecoin held at 1.0 by configuration
    -- 'daily_close'- daily close from a public series
    -- 'unpriced'   - no price available; value contributes 0 and is counted
    basis           TEXT          NOT NULL,
    source          TEXT          NOT NULL,
    loaded_at       TIMESTAMPTZ   NOT NULL DEFAULT now(),

    PRIMARY KEY (asset, price_date),
    CONSTRAINT prices_positive CHECK (usd >= 0),
    CONSTRAINT prices_basis_valid CHECK (basis IN ('pinned','daily_close','unpriced'))
);

COMMIT;
