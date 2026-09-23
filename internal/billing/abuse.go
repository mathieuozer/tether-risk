package billing

import (
	"context"
	"time"
)

// Controls against the product being used to launder (docs/DECISIONS.md D43).
// Patterns are counted and raised to an admin; nothing is blocked
// automatically, since an investigator and a launderer can look alike.

// Patterns counts one user's screens since a time. Follow-ups and watch
// rescreens are the system's own and are not counted.
type Patterns struct {
	SameAddress int // screens of the address just screened
	Risky       int // distinct addresses answered high_risk
	Fresh       int // distinct addresses first active within freshDays
}

func (s *Store) ScreenPatterns(ctx context.Context, userID int64, address string, since time.Time, freshDays int) (Patterns, error) {
	var p Patterns
	err := s.pg.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE address = $2),
		       count(DISTINCT address) FILTER (WHERE verdict = 'high_risk'),
		       count(DISTINCT address) FILTER (WHERE first_seen >= (created_at - make_interval(days => $4))::date)
		FROM screen_history
		WHERE user_id = $1 AND created_at >= $3 AND channel NOT IN ('followup', 'watch')`,
		userID, address, since, freshDays).Scan(&p.SameAddress, &p.Risky, &p.Fresh)
	return p, err
}

// RaiseFlag records a pattern for review. It returns false when the same
// flag was already raised today, so an admin hears of it once a day.
func (s *Store) RaiseFlag(ctx context.Context, userID int64, kind, key, detail string, now time.Time) (bool, error) {
	res, err := s.pg.ExecContext(ctx, `
		INSERT INTO abuse_flags (user_id, kind, key, day, detail, created_at)
		VALUES ($1, $2, $3, ($5::timestamptz AT TIME ZONE 'utc')::date, $4, $5)
		ON CONFLICT (user_id, kind, key, day) DO NOTHING`,
		userID, kind, key, detail, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Flag is one raised pattern.
type Flag struct {
	Kind, Key, Detail string
	CreatedAt         time.Time
}

// Flags lists a user's most recent flags, newest first.
func (s *Store) Flags(ctx context.Context, userID int64, limit int) ([]Flag, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT kind, key, detail, created_at FROM abuse_flags
		WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Flag
	for rows.Next() {
		var f Flag
		if err := rows.Scan(&f.Kind, &f.Key, &f.Detail, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
