-- 010_indexer.sql — how far each indexer run has got (docs/INDEXER_PLAN.md).

BEGIN;

CREATE TABLE IF NOT EXISTS indexer_cursors (
    name        TEXT        PRIMARY KEY,   -- 'tail', 'backfill:<from>-<to>'
    block       BIGINT      NOT NULL,      -- last block written
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
