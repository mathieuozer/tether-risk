-- 005_billing.sql — bot subscribers, their subscriptions, usage and USDT
-- invoices (docs/DECISIONS.md D26).
--
-- Every payment is its own row and nothing is ever deleted: a subscription
-- that was refunded or revoked is closed with a timestamp, so what a customer
-- paid for and when can always be answered from the table.

BEGIN;

CREATE TABLE IF NOT EXISTS bot_users (
    user_id          BIGINT      PRIMARY KEY,   -- Telegram user id
    username         TEXT,
    first_name       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Set when the free trial is granted. One trial per Telegram account.
    trial_started_at TIMESTAMPTZ
);

CREATE TYPE subscription_source AS ENUM ('trial', 'stars', 'usdt', 'grant');

CREATE TABLE IF NOT EXISTS subscriptions (
    id          BIGSERIAL   PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES bot_users(user_id),
    plan        TEXT        NOT NULL,
    source      subscription_source NOT NULL,
    starts_at   TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,

    -- What was paid, in the currency's smallest unit: Stars, or micro-USDT.
    amount      BIGINT      NOT NULL DEFAULT 0,
    currency    TEXT,

    -- Telegram Stars: the charge id, one per payment including renewals.
    stars_charge_id TEXT UNIQUE,
    is_recurring    BOOLEAN NOT NULL DEFAULT false,
    -- Set when auto-renewal was cancelled; access runs to expires_at.
    renewal_canceled_at TIMESTAMPTZ,

    -- USDT: the transaction that paid, and the invoice it matched.
    usdt_tx     TEXT UNIQUE,

    -- A refund or an admin revoke ends access immediately.
    revoked_at  TIMESTAMPTZ,
    note        TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT subscriptions_period_sane CHECK (expires_at > starts_at)
);

CREATE INDEX IF NOT EXISTS subscriptions_active
    ON subscriptions (user_id, expires_at) WHERE revoked_at IS NULL;

-- Screens per user per UTC day, for the plan's daily limit.
CREATE TABLE IF NOT EXISTS usage_daily (
    user_id  BIGINT NOT NULL REFERENCES bot_users(user_id),
    day      DATE   NOT NULL,
    screens  INT    NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, day)
);

CREATE TYPE invoice_state AS ENUM ('open', 'paid');

-- A USDT invoice is identified by its exact amount: the base price plus a
-- few cents unique among recent invoices. Exchanges withdraw to two
-- decimals, so cents are the finest unit a customer can reliably send.
CREATE TABLE IF NOT EXISTS usdt_invoices (
    id          BIGSERIAL   PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES bot_users(user_id),
    plan        TEXT        NOT NULL,
    address     TEXT        NOT NULL,        -- where to pay
    amount      BIGINT      NOT NULL,        -- micro-USDT (6 decimals)
    state       invoice_state NOT NULL DEFAULT 'open',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    paid_tx     TEXT UNIQUE,
    paid_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS usdt_invoices_open
    ON usdt_invoices (address, amount) WHERE state = 'open';

-- Transfers to the payment address that matched no invoice: a wrong amount,
-- a late payment, a stranger. Kept so an admin can resolve them by hand.
CREATE TABLE IF NOT EXISTS usdt_unmatched (
    tx          TEXT        PRIMARY KEY,
    from_address TEXT       NOT NULL,
    amount      BIGINT      NOT NULL,
    block_time  TIMESTAMPTZ NOT NULL,
    seen_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ
);

-- The watcher's position on the payment address, so a restart neither
-- rescans from the beginning nor skips a payment.
CREATE TABLE IF NOT EXISTS usdt_watch (
    address     TEXT        PRIMARY KEY,
    scanned_to  TIMESTAMPTZ NOT NULL
);

COMMIT;
