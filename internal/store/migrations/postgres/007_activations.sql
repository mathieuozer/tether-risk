-- 007_activations.sql — who created each account (docs/DECISIONS.md D31).
--
-- A TRON account exists once something pays to create it. Operators create
-- their wallets from a few operations accounts, so a shared activator links
-- wallets to one operator. Chain facts: they never change once read.

BEGIN;

CREATE TABLE IF NOT EXISTS activations (
    chain        TEXT        NOT NULL,
    address      TEXT        NOT NULL,
    -- NULL when the account has no creating transaction to read.
    activator    TEXT,
    tx_id        TEXT,
    tx_type      TEXT,
    amount       NUMERIC(38,0),
    activated_at TIMESTAMPTZ,
    fetched_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain, address)
);

CREATE INDEX IF NOT EXISTS activations_by_activator ON activations (chain, activator);

COMMIT;
