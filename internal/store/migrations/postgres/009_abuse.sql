-- 009_abuse.sql — controls against the product being used to launder
-- (docs/DECISIONS.md D43).

BEGIN;

-- What each screen answered, so patterns can be counted: how many risky
-- addresses a user screened in a day, how many freshly created ones.
ALTER TABLE screen_history ADD COLUMN IF NOT EXISTS verdict TEXT;
ALTER TABLE screen_history ADD COLUMN IF NOT EXISTS first_seen DATE;

CREATE INDEX IF NOT EXISTS screen_history_user_address
    ON screen_history (user_id, address, created_at DESC);

-- A pattern worth a human's look, raised once per user, kind and key a day.
-- Nothing is blocked automatically: an investigator and a launderer can look
-- alike for a day.
CREATE TABLE IF NOT EXISTS abuse_flags (
    id          BIGSERIAL   PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES bot_users(user_id),
    kind        TEXT        NOT NULL,   -- repeat_screen, many_risky, many_fresh, risky_payment
    key         TEXT        NOT NULL,   -- the address concerned, or '' for the user as a whole
    day         DATE        NOT NULL,
    detail      TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, kind, key, day)
);

COMMIT;
