-- 008_api_usage.sql — upstream API requests per day (docs/DECISIONS.md D35).
--
-- TronGrid's key allows 100,000 requests a day, counted in UTC days, after
-- which every request is held to one a second. Every process adds what it
-- spent here, so the worker can stop background work before the quota that
-- customers' screens and the Tether blacklist refresh depend on is gone.

BEGIN;

CREATE TABLE IF NOT EXISTS api_usage (
    day       DATE   NOT NULL,
    provider  TEXT   NOT NULL,
    requests  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (day, provider)
);

COMMIT;
