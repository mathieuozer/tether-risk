package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store persists users, subscriptions, usage and USDT invoices.
type Store struct {
	pg  *sql.DB
	cfg *Config
}

func NewStore(pg *sql.DB, cfg *Config) *Store { return &Store{pg: pg, cfg: cfg} }

// User is a bot user.
type User struct {
	ID             int64
	Username       string
	FirstName      string
	ClientLang     string // Telegram language_code, as sent
	Lang           string // chosen with /language; "" to follow the client
	TrialStartedAt *time.Time
}

// Touch records that a user was seen, creating them on first contact.
func (s *Store) Touch(ctx context.Context, u User) (User, error) {
	err := s.pg.QueryRowContext(ctx, `
		INSERT INTO bot_users (user_id, username, first_name, client_lang)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''))
		ON CONFLICT (user_id) DO UPDATE SET
			username     = COALESCE(EXCLUDED.username, bot_users.username),
			first_name   = COALESCE(EXCLUDED.first_name, bot_users.first_name),
			client_lang  = COALESCE(EXCLUDED.client_lang, bot_users.client_lang),
			last_seen_at = now()
		RETURNING trial_started_at, COALESCE(lang, ''), COALESCE(client_lang, '')`,
		u.ID, u.Username, u.FirstName, u.ClientLang).Scan(&u.TrialStartedAt, &u.Lang, &u.ClientLang)
	if err != nil {
		return u, fmt.Errorf("touch user %d: %w", u.ID, err)
	}
	return u, nil
}

// StartTrial grants the free trial if the user has never had one. It is safe
// to call concurrently: only the call that sets trial_started_at grants it.
func (s *Store) StartTrial(ctx context.Context, userID int64, now time.Time) (bool, error) {
	if s.cfg.Trial.Days <= 0 {
		return false, nil
	}
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE bot_users SET trial_started_at = $2 WHERE user_id = $1 AND trial_started_at IS NULL`,
		userID, now)
	if err != nil {
		return false, fmt.Errorf("start trial: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan, source, starts_at, expires_at, note)
		VALUES ($1, $2, 'trial', $3, $4, 'free trial')`,
		userID, s.cfg.Trial.Plan, now, now.Add(time.Duration(s.cfg.Trial.Days)*24*time.Hour)); err != nil {
		return false, fmt.Errorf("start trial: %w", err)
	}
	return true, tx.Commit()
}

// Subscriptions returns every subscription a user has had.
func (s *Store) Subscriptions(ctx context.Context, userID int64) ([]Subscription, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT id, user_id, plan, source::text, starts_at, expires_at,
		       COALESCE(stars_charge_id, ''), is_recurring, renewal_canceled_at, revoked_at
		FROM subscriptions WHERE user_id = $1 ORDER BY starts_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("subscriptions: %w", err)
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.ID, &sub.UserID, &sub.Plan, &sub.Source, &sub.StartsAt, &sub.ExpiresAt,
			&sub.StarsChargeID, &sub.IsRecurring, &sub.RenewalCanceledAt, &sub.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// Access is Entitle over the user's stored subscriptions.
func (s *Store) Access(ctx context.Context, userID int64, now time.Time) (Access, error) {
	subs, err := s.Subscriptions(ctx, userID)
	if err != nil {
		return Access{}, err
	}
	return Entitle(s.cfg, subs, now), nil
}

// Day is the usage day for t: days run on UTC.
func Day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Consume takes one screen from today's allowance. limit <= 0 means no
// limit. It reports the count after this screen and whether it was allowed.
// The check and the increment are one statement, so concurrent screens by
// one user cannot both slip under the limit.
func (s *Store) Consume(ctx context.Context, userID int64, now time.Time, limit int) (int, bool, error) {
	if limit <= 0 {
		limit = 1 << 30
	}
	var used int
	err := s.pg.QueryRowContext(ctx, `
		INSERT INTO usage_daily (user_id, day, screens) VALUES ($1, $2, 1)
		ON CONFLICT (user_id, day) DO UPDATE SET screens = usage_daily.screens + 1
		WHERE usage_daily.screens < $3
		RETURNING screens`,
		userID, Day(now), limit).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return limit, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("consume: %w", err)
	}
	return used, true, nil
}

// Refund gives back a screen that failed, so an error costs the user nothing.
func (s *Store) Refund(ctx context.Context, userID int64, now time.Time) error {
	_, err := s.pg.ExecContext(ctx, `
		UPDATE usage_daily SET screens = GREATEST(screens - 1, 0)
		WHERE user_id = $1 AND day = $2`, userID, Day(now))
	return err
}

// Used is how many screens the user has run today.
func (s *Store) Used(ctx context.Context, userID int64, now time.Time) (int, error) {
	var used int
	err := s.pg.QueryRowContext(ctx,
		`SELECT screens FROM usage_daily WHERE user_id = $1 AND day = $2`, userID, Day(now)).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return used, err
}

// StarsPayment is a successful Telegram Stars payment.
type StarsPayment struct {
	UserID     int64
	Plan       string
	Amount     int64
	ChargeID   string
	Recurring  bool
	ExpiresAt  time.Time // subscription_expiration_date; zero if absent
	ReceivedAt time.Time
}

// RecordStars turns a Stars payment into a subscription period. Telegram can
// deliver an update twice; the charge id is unique, so the second delivery
// changes nothing and reports false.
func (s *Store) RecordStars(ctx context.Context, p StarsPayment) (bool, error) {
	if _, ok := s.cfg.Plan(p.Plan); !ok {
		return false, fmt.Errorf("stars payment %s: unknown plan %q", p.ChargeID, p.Plan)
	}
	expires := p.ExpiresAt
	if expires.IsZero() || !expires.After(p.ReceivedAt) {
		expires = p.ReceivedAt.Add(StarsPeriod)
	}
	res, err := s.pg.ExecContext(ctx, `
		INSERT INTO subscriptions
			(user_id, plan, source, starts_at, expires_at, amount, currency, stars_charge_id, is_recurring)
		VALUES ($1, $2, 'stars', $3, $4, $5, 'XTR', $6, $7)
		ON CONFLICT (stars_charge_id) DO NOTHING`,
		p.UserID, p.Plan, p.ReceivedAt, expires, p.Amount, p.ChargeID, p.Recurring)
	if err != nil {
		return false, fmt.Errorf("record stars payment %s: %w", p.ChargeID, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RenewingCharges are the charge ids of a user's active Stars subscriptions
// that will renew, newest first.
func (s *Store) RenewingCharges(ctx context.Context, userID int64, now time.Time) ([]string, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT stars_charge_id FROM subscriptions
		WHERE user_id = $1 AND source = 'stars' AND is_recurring
		  AND renewal_canceled_at IS NULL AND revoked_at IS NULL AND expires_at > $2
		ORDER BY starts_at DESC`, userID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MarkRenewalCanceled records that a Stars subscription will not renew.
func (s *Store) MarkRenewalCanceled(ctx context.Context, chargeID string, now time.Time) error {
	_, err := s.pg.ExecContext(ctx,
		`UPDATE subscriptions SET renewal_canceled_at = $2 WHERE stars_charge_id = $1`, chargeID, now)
	return err
}

// RevokeCharge ends the period a Stars charge paid for, after a refund.
func (s *Store) RevokeCharge(ctx context.Context, chargeID, note string, now time.Time) (int64, error) {
	var userID int64
	err := s.pg.QueryRowContext(ctx, `
		UPDATE subscriptions SET revoked_at = $2, note = $3
		WHERE stars_charge_id = $1 AND revoked_at IS NULL
		RETURNING user_id`, chargeID, now, note).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return userID, err
}

// ChargeOwner returns the user a Stars charge belongs to.
func (s *Store) ChargeOwner(ctx context.Context, chargeID string) (int64, error) {
	var userID int64
	err := s.pg.QueryRowContext(ctx,
		`SELECT user_id FROM subscriptions WHERE stars_charge_id = $1`, chargeID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("no payment with charge id %s", chargeID)
	}
	return userID, err
}

// Grant gives a user a plan for some days without payment, continuing any
// period of that plan they already hold.
func (s *Store) Grant(ctx context.Context, userID int64, plan string, days int, note string, now time.Time) (time.Time, error) {
	if _, ok := s.cfg.Plan(plan); !ok {
		return time.Time{}, fmt.Errorf("unknown plan %q", plan)
	}
	if days < 1 {
		return time.Time{}, fmt.Errorf("days must be at least 1")
	}
	subs, err := s.Subscriptions(ctx, userID)
	if err != nil {
		return time.Time{}, err
	}
	start := PaidPeriodStart(s.cfg, subs, plan, now)
	end := start.Add(time.Duration(days) * 24 * time.Hour)
	_, err = s.pg.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan, source, starts_at, expires_at, note)
		VALUES ($1, $2, 'grant', $3, $4, $5)`, userID, plan, start, end, note)
	return end, err
}

// Revoke ends all of a user's access now. Stars renewals must be cancelled
// with Telegram separately; the caller does that.
func (s *Store) Revoke(ctx context.Context, userID int64, note string, now time.Time) (int64, error) {
	res, err := s.pg.ExecContext(ctx, `
		UPDATE subscriptions SET revoked_at = $2, note = $3
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > $2`, userID, now, note)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FindUser resolves "12345" or "@name" to a known user id.
func (s *Store) FindUser(ctx context.Context, ref string) (int64, error) {
	ref = strings.TrimSpace(ref)
	var id int64
	if _, err := fmt.Sscan(ref, &id); err == nil && id > 0 {
		return id, nil
	}
	name := strings.TrimPrefix(ref, "@")
	err := s.pg.QueryRowContext(ctx,
		`SELECT user_id FROM bot_users WHERE lower(username) = lower($1)`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("no user %s has used the bot", ref)
	}
	return id, err
}

// EnsureUser creates a user row if missing, for grants to someone who has
// not yet opened the bot.
func (s *Store) EnsureUser(ctx context.Context, userID int64) error {
	_, err := s.pg.ExecContext(ctx,
		`INSERT INTO bot_users (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, userID)
	return err
}

// ---------------------------------------------------------------------------
// USDT invoices
// ---------------------------------------------------------------------------

// CreateInvoice issues a USDT invoice with an amount no recent invoice uses.
// An advisory lock serialises issuance so two customers can never be given
// the same amount.
func (s *Store) CreateInvoice(ctx context.Context, userID int64, plan, address string, now time.Time, seed int) (Invoice, error) {
	p, ok := s.cfg.Plan(plan)
	if !ok {
		return Invoice{}, fmt.Errorf("unknown plan %q", plan)
	}
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return Invoice{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('usdt_invoices:' || $1))`, address); err != nil {
		return Invoice{}, err
	}

	// A customer asking again gets their open invoice back, not a second
	// amount to confuse them.
	var inv Invoice
	err = tx.QueryRowContext(ctx, `
		SELECT id, user_id, plan, address, amount, created_at, expires_at FROM usdt_invoices
		WHERE user_id = $1 AND plan = $2 AND address = $3 AND state = 'open' AND expires_at > $4
		ORDER BY created_at DESC LIMIT 1`, userID, plan, address, now).
		Scan(&inv.ID, &inv.UserID, &inv.Plan, &inv.Address, &inv.Amount, &inv.CreatedAt, &inv.ExpiresAt)
	if err == nil {
		return inv, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Invoice{}, err
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT amount FROM usdt_invoices
		WHERE address = $1 AND state = 'open' AND expires_at + $3::interval > $2`,
		address, now, pgInterval(s.cfg.USDT.Grace))
	if err != nil {
		return Invoice{}, err
	}
	taken := map[int64]bool{}
	for rows.Next() {
		var a int64
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return Invoice{}, err
		}
		taken[a] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Invoice{}, err
	}

	amount, err := PickAmount(p.PriceMicroUSDT, taken, seed)
	if err != nil {
		return Invoice{}, err
	}
	inv = Invoice{UserID: userID, Plan: plan, Address: address, Amount: amount,
		CreatedAt: now, ExpiresAt: now.Add(s.cfg.USDT.InvoiceTTL)}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO usdt_invoices (user_id, plan, address, amount, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		userID, plan, address, amount, inv.CreatedAt, inv.ExpiresAt).Scan(&inv.ID); err != nil {
		return Invoice{}, err
	}
	return inv, tx.Commit()
}

// OpenInvoices are the invoices on an address that a payment could still
// settle: open, and not past expiry plus grace.
func (s *Store) OpenInvoices(ctx context.Context, address string, now time.Time) ([]Invoice, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT id, user_id, plan, address, amount, created_at, expires_at FROM usdt_invoices
		WHERE address = $1 AND state = 'open' AND expires_at + $3::interval > $2
		ORDER BY created_at`, address, now, pgInterval(s.cfg.USDT.Grace))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invoice
	for rows.Next() {
		var inv Invoice
		if err := rows.Scan(&inv.ID, &inv.UserID, &inv.Plan, &inv.Address, &inv.Amount,
			&inv.CreatedAt, &inv.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// Settle marks an invoice paid and grants the period it bought. It reports
// false, and grants nothing, if the invoice or the transaction was already
// used: a rescan of the address must never grant twice.
func (s *Store) Settle(ctx context.Context, inv Invoice, p Payment, now time.Time) (time.Time, bool, error) {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE usdt_invoices SET state = 'paid', paid_tx = $2, paid_at = $3
		WHERE id = $1 AND state = 'open'
		  AND NOT EXISTS (SELECT 1 FROM subscriptions WHERE usdt_tx = $2)`,
		inv.ID, p.Tx, now)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("settle invoice %d: %w", inv.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return time.Time{}, false, nil
	}

	subs, err := s.Subscriptions(ctx, inv.UserID)
	if err != nil {
		return time.Time{}, false, err
	}
	start := PaidPeriodStart(s.cfg, subs, inv.Plan, now)
	end := start.Add(s.cfg.Period())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscriptions (user_id, plan, source, starts_at, expires_at, amount, currency, usdt_tx)
		VALUES ($1, $2, 'usdt', $3, $4, $5, 'USDT', $6)`,
		inv.UserID, inv.Plan, start, end, p.Amount, p.Tx); err != nil {
		return time.Time{}, false, fmt.Errorf("settle invoice %d: %w", inv.ID, err)
	}
	return end, true, tx.Commit()
}

// KnownTx reports whether a transaction already paid for something or was
// already recorded as unmatched.
func (s *Store) KnownTx(ctx context.Context, txID string) (bool, error) {
	var known bool
	err := s.pg.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM subscriptions WHERE usdt_tx = $1)
		    OR EXISTS (SELECT 1 FROM usdt_unmatched WHERE tx = $1)`, txID).Scan(&known)
	return known, err
}

// RecordUnmatched keeps a payment no invoice claimed. It reports true the
// first time, so admins are told once.
func (s *Store) RecordUnmatched(ctx context.Context, p Payment) (bool, error) {
	res, err := s.pg.ExecContext(ctx, `
		INSERT INTO usdt_unmatched (tx, from_address, amount, block_time)
		VALUES ($1, $2, $3, $4) ON CONFLICT (tx) DO NOTHING`,
		p.Tx, p.From, p.Amount, p.BlockTime)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ScannedTo is how far the watcher has read the payment address.
func (s *Store) ScannedTo(ctx context.Context, address string) (time.Time, bool, error) {
	var t time.Time
	err := s.pg.QueryRowContext(ctx,
		`SELECT scanned_to FROM usdt_watch WHERE address = $1`, address).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	return t, err == nil, err
}

// SetScannedTo advances the watcher's position.
func (s *Store) SetScannedTo(ctx context.Context, address string, t time.Time) error {
	_, err := s.pg.ExecContext(ctx, `
		INSERT INTO usdt_watch (address, scanned_to) VALUES ($1, $2)
		ON CONFLICT (address) DO UPDATE SET scanned_to = GREATEST(usdt_watch.scanned_to, EXCLUDED.scanned_to)`,
		address, t)
	return err
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

// Stats is an overview for admins.
type Stats struct {
	Users, ActiveUsers7d int
	ActiveByPlanSource   map[string]int // "pro/stars" -> count of users
	Stars30d             int64
	USDT30d              int64 // micro-USDT
	Unmatched            int
}

func (s *Store) Stats(ctx context.Context, now time.Time) (Stats, error) {
	st := Stats{ActiveByPlanSource: map[string]int{}}
	if err := s.pg.QueryRowContext(ctx, `
		SELECT count(*), count(*) FILTER (WHERE last_seen_at > $1) FROM bot_users`,
		now.Add(-7*24*time.Hour)).Scan(&st.Users, &st.ActiveUsers7d); err != nil {
		return st, err
	}
	rows, err := s.pg.QueryContext(ctx, `
		SELECT plan || '/' || source::text, count(DISTINCT user_id) FROM subscriptions
		WHERE revoked_at IS NULL AND starts_at <= $1 AND expires_at > $1
		GROUP BY 1 ORDER BY 1`, now)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return st, err
		}
		st.ActiveByPlanSource[k] = n
	}
	rows.Close()
	if err := s.pg.QueryRowContext(ctx, `
		SELECT COALESCE(sum(amount) FILTER (WHERE source = 'stars'), 0),
		       COALESCE(sum(amount) FILTER (WHERE source = 'usdt'), 0)
		FROM subscriptions WHERE revoked_at IS NULL AND created_at > $1`,
		now.Add(-30*24*time.Hour)).Scan(&st.Stars30d, &st.USDT30d); err != nil {
		return st, err
	}
	err = s.pg.QueryRowContext(ctx,
		`SELECT count(*) FROM usdt_unmatched WHERE resolved_at IS NULL`).Scan(&st.Unmatched)
	return st, err
}

func pgInterval(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}
