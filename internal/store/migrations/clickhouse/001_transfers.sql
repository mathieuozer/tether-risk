-- 001_transfers.sql — raw normalised transfer events, one row per value movement.
--
-- SPEC.md §4 specifies MergeTree ORDER BY (chain, from_address, block_time)
-- together with deduplication on (chain, tx_hash, log_index). A plain
-- MergeTree cannot deduplicate, so this uses ReplacingMergeTree and extends
-- the sorting key with the natural key. The spec's ordering survives as a
-- prefix, so scans by (chain, from_address) remain fast.
-- See docs/DECISIONS.md D1.

CREATE TABLE IF NOT EXISTS transfers
(
    chain           LowCardinality(String),
    tx_hash         String,
    log_index       UInt32,
    block_number    UInt64,
    block_time      DateTime,
    from_address    String,
    to_address      String,
    asset           LowCardinality(String),
    raw_value       UInt256,

    -- SPEC.md §4: nullable until Phase 3. Null means "not yet priced", which
    -- is different from zero and must stay distinguishable.
    usd_value       Nullable(Decimal(38, 6)),

    -- docs/PLAN.md F3: native-asset prices come from a daily close series, so
    -- an individual transfer's USD value carries real error. Recording the
    -- pricing basis lets a score driven by imprecisely-valued native flows be
    -- told apart from one driven by stablecoins.
    price_basis     LowCardinality(String) DEFAULT 'unpriced',

    -- Ingestion bookkeeping. Not part of the natural key: re-ingesting the
    -- same event must collapse to one row, not two.
    ingested_at     DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
ORDER BY (chain, from_address, block_time, tx_hash, log_index)
SETTINGS index_granularity = 8192;


-- SPEC.md §4 asks for a second ordering by (chain, to_address, block_time)
-- because both traversal directions must be fast.
--
-- A projection would be the obvious way to get it, but ClickHouse rejects
-- projections on ReplacingMergeTree unless deduplicate_merge_projection_mode
-- is set, because the projection is not deduplicated when parts merge: it
-- would keep counting rows the base table had already collapsed. That is the
-- same class of bug documented for `edges` in 002, and it is worth avoiding
-- rather than configuring around.
--
-- A separate ReplacingMergeTree fed by a materialized view has no such
-- problem. Duplicate inserts collapse here exactly as they do in the base
-- table, because this is a copy of the rows and not an aggregate over them.
-- This is also what SPEC.md §4 literally asks for.
--
-- Traversal itself reads `edges`, never `transfers` (SPEC.md §4). This table
-- serves inspection, auditing, and the derived-label jobs, which do need to
-- walk raw inbound events.
CREATE TABLE IF NOT EXISTS transfers_by_to
(
    chain           LowCardinality(String),
    tx_hash         String,
    log_index       UInt32,
    block_number    UInt64,
    block_time      DateTime,
    from_address    String,
    to_address      String,
    asset           LowCardinality(String),
    raw_value       UInt256,
    usd_value       Nullable(Decimal(38, 6)),
    price_basis     LowCardinality(String) DEFAULT 'unpriced',
    ingested_at     DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
ORDER BY (chain, to_address, block_time, tx_hash, log_index)
SETTINGS index_granularity = 8192;

CREATE MATERIALIZED VIEW IF NOT EXISTS transfers_by_to_mv TO transfers_by_to AS
SELECT
    chain, tx_hash, log_index, block_number, block_time,
    from_address, to_address, asset, raw_value, usd_value,
    price_basis, ingested_at
FROM transfers;
