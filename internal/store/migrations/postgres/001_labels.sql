-- 001_labels.sql — the label database and its snapshot versioning.
--
-- SPEC.md §2 requires that identical input plus an identical label snapshot
-- produces an identical score, and §4 puts a snapshot_id on every label row.
-- SPEC.md §4 also asks for UNIQUE (chain, address, source).
--
-- Those two requirements conflict as literally written: one row per source
-- cannot simultaneously be one row per snapshot. Materialising a full copy of
-- the table per daily snapshot would mean roughly 365M rows a year for a 1M
-- label set, which is not a real option either.
--
-- Resolution (docs/DECISIONS.md D4): slowly-changing-dimension type 2. Each
-- row is valid over a half-open range of snapshots. The spec's uniqueness
-- holds within any single snapshot, which is what it was actually protecting.
-- Resolving labels at snapshot S selects rows where
--   valid_from_snapshot <= S AND (valid_to_snapshot IS NULL OR S < valid_to_snapshot)

BEGIN;

-- ---------------------------------------------------------------------------
-- Snapshots
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS label_snapshots (
    id           BIGSERIAL PRIMARY KEY,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),

    -- Set when the snapshot stops being the open one. A score always names a
    -- closed or open snapshot id, never "latest", so it stays reproducible.
    sealed_at    TIMESTAMPTZ,

    -- Per-source row counts at seal time. SPEC.md §6 acceptance requires
    -- label counts per source be reported.
    source_counts JSONB       NOT NULL DEFAULT '{}'::jsonb,

    notes        TEXT
);

COMMENT ON TABLE label_snapshots IS
    'Immutable versions of the label set. Every score stamps the snapshot id it resolved against.';

-- ---------------------------------------------------------------------------
-- Labels
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS labels (
    id                   BIGSERIAL PRIMARY KEY,

    chain                TEXT         NOT NULL,
    address              TEXT         NOT NULL,

    entity               TEXT         NOT NULL,   -- 'Binance', 'Tornado Cash', 'OFAC SDN #1234'
    category             TEXT         NOT NULL,   -- controlled vocabulary, see config/weights.yaml
    confidence           NUMERIC(3,2) NOT NULL,
    source               TEXT         NOT NULL,   -- must match an id in config/sources.yaml

    -- SPEC.md §6: the derived-deposit heuristic must record its reasoning.
    -- Every derived label is expected to carry enough here to reconstruct why
    -- it was emitted.
    evidence             JSONB        NOT NULL DEFAULT '{}'::jsonb,

    first_seen           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_updated         TIMESTAMPTZ  NOT NULL DEFAULT now(),

    -- SCD-2 validity window over snapshot ids. NULL valid_to means current.
    valid_from_snapshot  BIGINT       NOT NULL REFERENCES label_snapshots(id),
    valid_to_snapshot    BIGINT                REFERENCES label_snapshots(id),

    CONSTRAINT labels_confidence_range CHECK (confidence > 0 AND confidence <= 1),
    CONSTRAINT labels_validity_ordered CHECK (
        valid_to_snapshot IS NULL OR valid_to_snapshot > valid_from_snapshot
    ),
    CONSTRAINT labels_address_not_blank CHECK (length(address) > 0)
);

-- SPEC.md §4's uniqueness, scoped to the snapshot in which a row opens: at
-- most one open row per (chain, address, source).
CREATE UNIQUE INDEX IF NOT EXISTS labels_current_unique
    ON labels (chain, address, source)
    WHERE valid_to_snapshot IS NULL;

-- Historical rows may repeat a key across snapshots, but never open twice in
-- the same one.
CREATE UNIQUE INDEX IF NOT EXISTS labels_versioned_unique
    ON labels (chain, address, source, valid_from_snapshot);

-- The resolution hot path: look up every label for an address, then apply the
-- §6 rules.
CREATE INDEX IF NOT EXISTS labels_lookup
    ON labels (chain, address)
    INCLUDE (category, confidence, source)
    WHERE valid_to_snapshot IS NULL;

CREATE INDEX IF NOT EXISTS labels_by_category ON labels (category)
    WHERE valid_to_snapshot IS NULL;

CREATE INDEX IF NOT EXISTS labels_by_source ON labels (source)
    WHERE valid_to_snapshot IS NULL;

-- Traversal asks "is this address labelled at all?" at every node, because a
-- labelled address is a terminal node (SPEC.md §7). That question must be
-- cheap.
CREATE INDEX IF NOT EXISTS labels_address_only
    ON labels (address)
    WHERE valid_to_snapshot IS NULL;

COMMENT ON COLUMN labels.evidence IS
    'Why this label exists. Required to be non-trivial for derived sources.';

-- ---------------------------------------------------------------------------
-- Conflicts
-- ---------------------------------------------------------------------------
-- SPEC.md §6: "Never silently merge conflicting categories — record the
-- conflict." A conflict is not an error to be swallowed; it is a signal that
-- two sources disagree about what an address is, and a reviewer needs to see
-- it.

CREATE TABLE IF NOT EXISTS label_conflicts (
    id              BIGSERIAL PRIMARY KEY,
    chain           TEXT        NOT NULL,
    address         TEXT        NOT NULL,
    snapshot_id     BIGINT      NOT NULL REFERENCES label_snapshots(id),

    -- The category that resolution chose, and the ones it set aside.
    resolved_category TEXT      NOT NULL,
    resolved_source   TEXT      NOT NULL,

    -- [{source, category, confidence}, ...] for every competing label.
    competing       JSONB       NOT NULL,

    detected_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Set when a human has looked at it. Unreviewed conflicts are the queue.
    reviewed_at     TIMESTAMPTZ,
    review_note     TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS label_conflicts_unique
    ON label_conflicts (chain, address, snapshot_id);

CREATE INDEX IF NOT EXISTS label_conflicts_unreviewed
    ON label_conflicts (detected_at)
    WHERE reviewed_at IS NULL;

COMMIT;
