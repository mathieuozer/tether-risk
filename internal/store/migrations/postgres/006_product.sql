-- 006_product.sql — screen history, watched addresses and API keys
-- (docs/DECISIONS.md D27).

BEGIN;

-- The language the bot and the Mini App speak to a user. NULL means follow
-- the Telegram client's language.
ALTER TABLE bot_users ADD COLUMN IF NOT EXISTS lang TEXT;
-- The Telegram client's language_code as last seen, for messages the bot
-- sends unprompted (payments, alerts), which carry no client to ask.
ALTER TABLE bot_users ADD COLUMN IF NOT EXISTS client_lang TEXT;

-- Every screen a customer ran, from any channel, so they can find and rerun
-- it. The result itself is in `runs`; this is the customer's view of it.
CREATE TABLE IF NOT EXISTS screen_history (
    id          BIGSERIAL   PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES bot_users(user_id),
    chain       TEXT        NOT NULL,
    address     TEXT        NOT NULL,
    channel     TEXT        NOT NULL,     -- bot, app, api, batch, watch
    score       NUMERIC(6,3),
    band        TEXT,
    coverage    NUMERIC(8,6),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS screen_history_user
    ON screen_history (user_id, created_at DESC);

-- Addresses a customer watches. The monitor rescreens each on a schedule and
-- alerts when its risk worsens, comparing against the last state it saw.
CREATE TABLE IF NOT EXISTS watches (
    id           BIGSERIAL   PRIMARY KEY,
    user_id      BIGINT      NOT NULL REFERENCES bot_users(user_id),
    chain        TEXT        NOT NULL,
    address      TEXT        NOT NULL,
    label        TEXT,                    -- the customer's own name for it
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    checked_at   TIMESTAMPTZ,
    -- Last state seen, as JSON: band, score, coverage, high-risk categories,
    -- direct listing. Alerts compare against this, never against a guess.
    last_state   JSONB,
    last_alert_at TIMESTAMPTZ,
    removed_at   TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS watches_one_per_address
    ON watches (user_id, chain, address) WHERE removed_at IS NULL;

CREATE INDEX IF NOT EXISTS watches_due
    ON watches (checked_at NULLS FIRST) WHERE removed_at IS NULL;

-- API keys for the business plan. Only a SHA-256 of the key is stored; the
-- key itself is shown to the customer once and cannot be recovered.
CREATE TABLE IF NOT EXISTS api_keys (
    id           BIGSERIAL   PRIMARY KEY,
    user_id      BIGINT      NOT NULL REFERENCES bot_users(user_id),
    prefix       TEXT        NOT NULL,     -- first characters, to tell keys apart
    key_hash     TEXT        NOT NULL UNIQUE,
    name         TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS api_keys_user ON api_keys (user_id) WHERE revoked_at IS NULL;

COMMIT;
