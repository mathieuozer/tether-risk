-- 003_index.sql — tables our own TRON index writes besides `transfers`
-- (docs/INDEXER_PLAN.md). Applied to every ClickHouse database, so the index
-- database (`tron_index`) has the full schema; in the live database they
-- stay empty until cutover.

-- Blocks already written. The indexer checks a batch against it before
-- writing, so a batch repeated after a crash does not insert its transfers,
-- and the edge views do not count them, a second time (D2, D37).
CREATE TABLE IF NOT EXISTS indexed_blocks
(
    chain        LowCardinality(String),
    block        UInt64,
    block_time   DateTime,
    transfers    UInt32,
    written_at   DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(written_at)
ORDER BY (chain, block);

-- Contract events kept whole: Tether's blacklist, a DEX pool's reserves.
CREATE TABLE IF NOT EXISTS contract_events
(
    chain        LowCardinality(String),
    contract     String,
    event        LowCardinality(String),
    tx_hash      String,
    position     UInt32,
    block_number UInt64,
    block_time   DateTime,
    topics       Array(String),
    data         String,
    ingested_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
ORDER BY (chain, contract, event, block_time, tx_hash, position);
