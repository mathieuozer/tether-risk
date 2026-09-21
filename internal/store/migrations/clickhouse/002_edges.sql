-- 002_edges.sql — aggregated address-to-address flow.
--
-- SPEC.md §4: "All traversal reads this table, never `transfers`. This is the
-- single most important performance decision in the system."
--
-- ---------------------------------------------------------------------------
-- The double-counting trap
-- ---------------------------------------------------------------------------
-- A ClickHouse materialized view fires once per INSERT, on the rows of that
-- insert — not on the rows that survive deduplication. `transfers` is a
-- ReplacingMergeTree, so re-inserting an already-known event eventually
-- collapses to a single row there. The aggregate behind `edges`, however, has
-- already added the duplicate, and nothing ever removes it.
--
-- The consequence is silent and severe: every re-run of an ingestion job
-- inflates edge values, traversal reads only `edges`, and every downstream
-- score is wrong with no visible error.
--
-- Three defences, all required (docs/DECISIONS.md D2):
--   1. Ingestion deduplicates each batch against existing keys BEFORE insert.
--   2. An ingest ledger in PostgreSQL makes a repeated job a no-op.
--   3. `cmd/ingest rebuild-edges` recomputes this table from deduplicated
--      `transfers`. That rebuild is ground truth, and a test asserts the
--      incremental view and the rebuild agree.
-- ---------------------------------------------------------------------------

-- SimpleAggregateFunction is used rather than AggregateFunction because sum,
-- min and max are all simple aggregates: the stored value is the value, so
-- rows stay readable with plain SQL instead of -Merge combinators. Readers
-- must still aggregate explicitly (see edges_current below) because parts are
-- only merged in the background.
CREATE TABLE IF NOT EXISTS edges
(
    chain            LowCardinality(String),
    from_address     String,
    to_address       String,
    asset            LowCardinality(String),

    -- USD is the value space traversal haircuts in, so it is the primary
    -- measure. Null usd_value in `transfers` contributes 0 here and is
    -- counted separately below, so unpriced flow can never masquerade as
    -- zero-value flow.
    total_usd_value  SimpleAggregateFunction(sum, Decimal(38, 6)),

    -- Raw units are kept because they are exact, and because a reviewer
    -- auditing a score needs the on-chain number, not a derived one.
    total_raw_value  SimpleAggregateFunction(sum, UInt256),

    transfer_count   SimpleAggregateFunction(sum, UInt64),

    -- How many of those transfers had no USD price. If this is a large share
    -- of an edge, any score leaning on that edge is weakly supported, and the
    -- coverage figure must say so rather than quietly rounding it away.
    unpriced_count   SimpleAggregateFunction(sum, UInt64),

    first_seen       SimpleAggregateFunction(min, DateTime),
    last_seen        SimpleAggregateFunction(max, DateTime)
)
ENGINE = AggregatingMergeTree
ORDER BY (chain, from_address, to_address, asset);


-- Reverse-direction ordering. SPEC.md §7 traverses one direction at a time and
-- both must be fast; inbound tracing scans by to_address.
--
-- A separate table rather than a projection, for the reason given in 001:
-- ClickHouse does not deduplicate projections across merges, and on an
-- aggregating engine that failure is silent inflation of exactly the numbers
-- traversal reads. Both tables are fed from `transfers` by their own
-- materialized view and are protected by the same pre-insert deduplication.
CREATE TABLE IF NOT EXISTS edges_by_to
(
    chain            LowCardinality(String),
    from_address     String,
    to_address       String,
    asset            LowCardinality(String),
    total_usd_value  SimpleAggregateFunction(sum, Decimal(38, 6)),
    total_raw_value  SimpleAggregateFunction(sum, UInt256),
    transfer_count   SimpleAggregateFunction(sum, UInt64),
    unpriced_count   SimpleAggregateFunction(sum, UInt64),
    first_seen       SimpleAggregateFunction(min, DateTime),
    last_seen        SimpleAggregateFunction(max, DateTime)
)
ENGINE = AggregatingMergeTree
ORDER BY (chain, to_address, from_address, asset);


-- The incremental maintainer. Correct only because ingestion guarantees each
-- (chain, tx_hash, log_index) is inserted exactly once.
CREATE MATERIALIZED VIEW IF NOT EXISTS edges_mv TO edges AS
SELECT
    chain,
    from_address,
    to_address,
    asset,
    sum(ifNull(usd_value, toDecimal64(0, 6)))           AS total_usd_value,
    sum(raw_value)                                      AS total_raw_value,
    count()                                             AS transfer_count,
    countIf(usd_value IS NULL)                          AS unpriced_count,
    min(block_time)                                     AS first_seen,
    max(block_time)                                     AS last_seen
FROM transfers
GROUP BY chain, from_address, to_address, asset;


CREATE MATERIALIZED VIEW IF NOT EXISTS edges_by_to_mv TO edges_by_to AS
SELECT
    chain,
    from_address,
    to_address,
    asset,
    sum(ifNull(usd_value, toDecimal64(0, 6)))           AS total_usd_value,
    sum(raw_value)                                      AS total_raw_value,
    count()                                             AS transfer_count,
    countIf(usd_value IS NULL)                          AS unpriced_count,
    min(block_time)                                     AS first_seen,
    max(block_time)                                     AS last_seen
FROM transfers
GROUP BY chain, from_address, to_address, asset;


-- Read these, not the underlying tables. Background merges are asynchronous,
-- so an unaggregated read can return several partial rows for one edge.
-- Traversal that summed only one of them would understate value and therefore
-- understate risk.
CREATE VIEW IF NOT EXISTS edges_current AS
SELECT
    chain,
    from_address,
    to_address,
    asset,
    sum(total_usd_value) AS total_usd_value,
    sum(total_raw_value) AS total_raw_value,
    sum(transfer_count)  AS transfer_count,
    sum(unpriced_count)  AS unpriced_count,
    min(first_seen)      AS first_seen,
    max(last_seen)       AS last_seen
FROM edges
GROUP BY chain, from_address, to_address, asset;

CREATE VIEW IF NOT EXISTS edges_by_to_current AS
SELECT
    chain,
    from_address,
    to_address,
    asset,
    sum(total_usd_value) AS total_usd_value,
    sum(total_raw_value) AS total_raw_value,
    sum(transfer_count)  AS transfer_count,
    sum(unpriced_count)  AS unpriced_count,
    min(first_seen)      AS first_seen,
    max(last_seen)       AS last_seen
FROM edges_by_to
GROUP BY chain, from_address, to_address, asset;
