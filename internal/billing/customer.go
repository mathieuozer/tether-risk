package billing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Customer data beyond billing: language, screen history, watched addresses
// and API keys (docs/DECISIONS.md D27).

// ErrNotFound is a watch or key that does not exist or is not the user's.
var ErrNotFound = errors.New("not found")

// Langs returns the user's chosen language and their client's, either ""
// when unknown.
func (s *Store) Langs(ctx context.Context, userID int64) (chosen, client string, err error) {
	err = s.pg.QueryRowContext(ctx,
		`SELECT COALESCE(lang, ''), COALESCE(client_lang, '') FROM bot_users WHERE user_id = $1`,
		userID).Scan(&chosen, &client)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return chosen, client, err
}

// SetLang stores the user's language choice.
func (s *Store) SetLang(ctx context.Context, userID int64, lang string) error {
	_, err := s.pg.ExecContext(ctx, `UPDATE bot_users SET lang = $2 WHERE user_id = $1`, userID, lang)
	return err
}

// FetchBacklog is how many fetch jobs the ingest worker could run now. The
// follow-up waits for it to reach zero: a rescreen before the worker has
// fetched the last ring would see nothing new.
func (s *Store) FetchBacklog(ctx context.Context) (int, error) {
	var n int
	err := s.pg.QueryRowContext(ctx, `
		SELECT count(*) FROM fetch_jobs
		WHERE state = 'running' OR (state = 'pending' AND (not_before IS NULL OR not_before <= now()))`).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

// HistoryItem is one screen a customer ran.
type HistoryItem struct {
	Chain     string    `json:"chain"`
	Address   string    `json:"address"`
	Channel   string    `json:"channel"`
	Band      *string   `json:"band"`
	Score     *float64  `json:"score"`
	Coverage  *float64  `json:"coverage"`
	CreatedAt time.Time `json:"created_at"`
}

// RecordScreen adds a screen to the user's history. score, band and coverage
// are nil when the channel produced no result to read them from (a PDF).
func (s *Store) RecordScreen(ctx context.Context, userID int64, chain, address, channel string,
	score *float64, band *string, coverage *float64, now time.Time) error {
	_, err := s.pg.ExecContext(ctx, `
		INSERT INTO screen_history (user_id, chain, address, channel, score, band, coverage, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		userID, chain, address, channel, score, band, coverage, now)
	return err
}

// History returns the user's most recent screens, newest first.
func (s *Store) History(ctx context.Context, userID int64, limit int) ([]HistoryItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pg.QueryContext(ctx, `
		SELECT chain, address, channel, band, score::float8, coverage::float8, created_at
		FROM screen_history WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryItem{}
	for rows.Next() {
		var h HistoryItem
		if err := rows.Scan(&h.Chain, &h.Address, &h.Channel, &h.Band, &h.Score, &h.Coverage, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Watches
// ---------------------------------------------------------------------------

// WatchState is what the monitor last saw for a watched address. Alerts
// compare a new state against it.
type WatchState struct {
	Band           string   `json:"band"`
	Score          float64  `json:"score"`
	Coverage       float64  `json:"coverage"`
	RiskCategories []string `json:"risk_categories"`
	Listed         bool     `json:"listed"`
}

// Watch is a watched address.
type Watch struct {
	ID        int64       `json:"id"`
	UserID    int64       `json:"-"`
	Chain     string      `json:"chain"`
	Address   string      `json:"address"`
	Label     string      `json:"label"`
	CreatedAt time.Time   `json:"created_at"`
	CheckedAt *time.Time  `json:"checked_at"`
	Last      *WatchState `json:"last"`
}

// ErrWatchLimit means the user's plan allows no more watches.
var ErrWatchLimit = errors.New("watch limit reached")

// AddWatch starts watching an address, within limit. Watching an address
// already watched returns the existing watch, with the label updated if one
// was given.
func (s *Store) AddWatch(ctx context.Context, userID int64, chain, address, label string, limit int, now time.Time) (Watch, error) {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return Watch{}, err
	}
	defer tx.Rollback()

	// Serialise per user so two concurrent adds cannot both pass the limit.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, userID); err != nil {
		return Watch{}, err
	}
	var existing int64
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM watches WHERE user_id = $1 AND chain = $2 AND address = $3 AND removed_at IS NULL`,
		userID, chain, address).Scan(&existing)
	switch {
	case err == nil:
		if label != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE watches SET label = $2 WHERE id = $1`, existing, label); err != nil {
				return Watch{}, err
			}
		}
	case errors.Is(err, sql.ErrNoRows):
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM watches WHERE user_id = $1 AND removed_at IS NULL`, userID).Scan(&n); err != nil {
			return Watch{}, err
		}
		if limit >= 0 && n >= limit {
			return Watch{}, ErrWatchLimit
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO watches (user_id, chain, address, label, created_at)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5) RETURNING id`,
			userID, chain, address, label, now).Scan(&existing); err != nil {
			return Watch{}, err
		}
	default:
		return Watch{}, err
	}
	if err := tx.Commit(); err != nil {
		return Watch{}, err
	}
	return s.watch(ctx, userID, existing)
}

func (s *Store) watch(ctx context.Context, userID, id int64) (Watch, error) {
	ws, err := s.queryWatches(ctx, `WHERE user_id = $1 AND id = $2 AND removed_at IS NULL`, userID, id)
	if err != nil {
		return Watch{}, err
	}
	if len(ws) == 0 {
		return Watch{}, ErrNotFound
	}
	return ws[0], nil
}

// Watches lists the user's watched addresses, oldest first.
func (s *Store) Watches(ctx context.Context, userID int64) ([]Watch, error) {
	return s.queryWatches(ctx, `WHERE user_id = $1 AND removed_at IS NULL ORDER BY created_at, id`, userID)
}

// RemoveWatch stops watching. It never deletes: the row keeps the history.
func (s *Store) RemoveWatch(ctx context.Context, userID, id int64, now time.Time) error {
	res, err := s.pg.ExecContext(ctx, `
		UPDATE watches SET removed_at = $3 WHERE user_id = $1 AND id = $2 AND removed_at IS NULL`, userID, id, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RemoveWatchByAddress stops watching an address, for the chat command.
func (s *Store) RemoveWatchByAddress(ctx context.Context, userID int64, address string, now time.Time) error {
	res, err := s.pg.ExecContext(ctx, `
		UPDATE watches SET removed_at = $3
		WHERE user_id = $1 AND lower(address) = lower($2) AND removed_at IS NULL`, userID, address, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DueWatches are watches not checked since before, never-checked first, at
// most limit. Watches of users without an active plan are included; the
// monitor decides whether to act on them.
func (s *Store) DueWatches(ctx context.Context, before time.Time, limit int) ([]Watch, error) {
	return s.queryWatches(ctx, `
		WHERE removed_at IS NULL AND (checked_at IS NULL OR checked_at < $1)
		ORDER BY checked_at NULLS FIRST, id LIMIT $2`, before, limit)
}

// SetWatchState records a check. alerted marks that the user was told.
func (s *Store) SetWatchState(ctx context.Context, id int64, st WatchState, now time.Time, alerted bool) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = s.pg.ExecContext(ctx, `
		UPDATE watches SET last_state = $2, checked_at = $3,
		       last_alert_at = CASE WHEN $4 THEN $3 ELSE last_alert_at END
		WHERE id = $1`, id, raw, now, alerted)
	return err
}

// TouchWatch records a check that produced no state, so a failing address
// does not jump the queue on every pass.
func (s *Store) TouchWatch(ctx context.Context, id int64, now time.Time) error {
	_, err := s.pg.ExecContext(ctx, `UPDATE watches SET checked_at = $2 WHERE id = $1`, id, now)
	return err
}

func (s *Store) queryWatches(ctx context.Context, where string, args ...any) ([]Watch, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT id, user_id, chain, address, COALESCE(label, ''), created_at, checked_at, last_state
		FROM watches `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Watch{}
	for rows.Next() {
		var w Watch
		var raw []byte
		if err := rows.Scan(&w.ID, &w.UserID, &w.Chain, &w.Address, &w.Label, &w.CreatedAt, &w.CheckedAt, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			var st WatchState
			if err := json.Unmarshal(raw, &st); err == nil {
				w.Last = &st
			}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

// APIKey describes a key without revealing it.
type APIKey struct {
	ID         int64      `json:"id"`
	Prefix     string     `json:"prefix"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// keyPrefix marks our keys, so a leaked one is recognisable in a scan.
const keyPrefix = "trk_"

// maxKeys is how many live keys one customer may hold.
const maxKeys = 10

// ErrKeyLimit means the user holds the maximum number of keys.
var ErrKeyLimit = fmt.Errorf("at most %d API keys", maxKeys)

// CreateKey issues a new key. The key is returned once; only its hash is
// stored.
func (s *Store) CreateKey(ctx context.Context, userID int64, name string, now time.Time) (string, APIKey, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", APIKey{}, err
	}
	key := keyPrefix + hex.EncodeToString(buf)
	k := APIKey{Prefix: key[:len(keyPrefix)+6], Name: strings.TrimSpace(name), CreatedAt: now}

	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return "", APIKey{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, userID); err != nil {
		return "", APIKey{}, err
	}
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&n); err != nil {
		return "", APIKey{}, err
	}
	if n >= maxKeys {
		return "", APIKey{}, ErrKeyLimit
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO api_keys (user_id, prefix, key_hash, name, created_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5) RETURNING id`,
		userID, k.Prefix, hashKey(key), k.Name, now).Scan(&k.ID); err != nil {
		return "", APIKey{}, err
	}
	return key, k, tx.Commit()
}

// Keys lists the user's live keys.
func (s *Store) Keys(ctx context.Context, userID int64) ([]APIKey, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT id, prefix, COALESCE(name, ''), created_at, last_used_at FROM api_keys
		WHERE user_id = $1 AND revoked_at IS NULL ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Prefix, &k.Name, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey ends a key.
func (s *Store) RevokeKey(ctx context.Context, userID, id int64, now time.Time) error {
	res, err := s.pg.ExecContext(ctx, `
		UPDATE api_keys SET revoked_at = $3 WHERE user_id = $1 AND id = $2 AND revoked_at IS NULL`, userID, id, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// KeyOwner resolves a presented key to its user and key id, and records its
// use. A revoked or unknown key returns ErrNotFound.
func (s *Store) KeyOwner(ctx context.Context, key string, now time.Time) (userID, keyID int64, err error) {
	if !strings.HasPrefix(key, keyPrefix) {
		return 0, 0, ErrNotFound
	}
	err = s.pg.QueryRowContext(ctx, `
		UPDATE api_keys SET last_used_at = $2
		WHERE key_hash = $1 AND revoked_at IS NULL
		RETURNING user_id, id`, hashKey(key), now).Scan(&userID, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	return userID, keyID, err
}

// hashKey is SHA-256: keys are 192 random bits, so a slow password hash
// would add nothing but latency to every API request.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
