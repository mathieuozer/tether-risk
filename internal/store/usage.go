package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Upstream API usage per UTC day (docs/DECISIONS.md D35). TronGrid counts
// its daily quota in UTC days, so the day here is UTC as well.

// AddAPIUsage adds n requests to today's count for provider.
func AddAPIUsage(ctx context.Context, pg *sql.DB, provider string, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := pg.ExecContext(ctx, `
		INSERT INTO api_usage (day, provider, requests)
		VALUES ((now() AT TIME ZONE 'utc')::date, $1, $2)
		ON CONFLICT (day, provider) DO UPDATE SET requests = api_usage.requests + EXCLUDED.requests`,
		provider, n)
	if err != nil {
		return fmt.Errorf("record %s usage: %w", provider, err)
	}
	return nil
}

// APIUsageToday is today's request count for provider.
func APIUsageToday(ctx context.Context, pg *sql.DB, provider string) (int64, error) {
	var n int64
	err := pg.QueryRowContext(ctx, `
		SELECT COALESCE(sum(requests), 0) FROM api_usage
		WHERE day = (now() AT TIME ZONE 'utc')::date AND provider = $1`, provider).Scan(&n)
	return n, err
}
