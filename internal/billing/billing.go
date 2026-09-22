package billing

import (
	"errors"
	"time"
)

// Subscription is one paid, granted or trial period.
type Subscription struct {
	ID        int64
	UserID    int64
	Plan      string
	Source    string // trial, stars, usdt, grant
	StartsAt  time.Time
	ExpiresAt time.Time

	StarsChargeID     string
	IsRecurring       bool
	RenewalCanceledAt *time.Time
	RevokedAt         *time.Time
}

// Active reports whether the subscription grants access at t.
func (s Subscription) Active(t time.Time) bool {
	return s.RevokedAt == nil && !t.Before(s.StartsAt) && t.Before(s.ExpiresAt)
}

// Access is what a user may do right now.
type Access struct {
	// Plan is the best active plan; nil means no access.
	Plan *Plan
	// Until is when that plan's access ends.
	Until time.Time
	// Source is how the plan was obtained.
	Source string
	// Renews reports that a Stars subscription will renew by itself.
	Renews bool
	// Admin has every feature and no daily limit.
	Admin bool
}

// Entitle decides a user's access at t from their subscriptions.
//
// The highest-ranked active plan wins. Where one plan is covered by several
// periods — a renewal paid before the current one ends, or USDT on top of
// Stars — access runs to the latest end among them that is contiguous with
// now, so a renewal shows as one unbroken period.
func Entitle(cfg *Config, subs []Subscription, t time.Time) Access {
	var best Access
	for _, s := range subs {
		if !s.Active(t) {
			continue
		}
		p, ok := cfg.Plan(s.Plan)
		if !ok {
			continue // a plan removed from billing.yaml grants nothing
		}
		if best.Plan == nil || p.Rank > best.Plan.Rank {
			best = Access{Plan: p, Until: s.ExpiresAt, Source: s.Source}
		}
	}
	if best.Plan == nil {
		return best
	}
	// Extend through back-to-back periods of the same plan.
	for changed := true; changed; {
		changed = false
		for _, s := range subs {
			if s.RevokedAt != nil || s.Plan != best.Plan.ID {
				continue
			}
			if !s.StartsAt.After(best.Until) && s.ExpiresAt.After(best.Until) {
				best.Until = s.ExpiresAt
				changed = true
			}
		}
	}
	for _, s := range subs {
		if s.Source == "stars" && s.IsRecurring && s.RenewalCanceledAt == nil &&
			s.RevokedAt == nil && s.Plan == best.Plan.ID && s.Active(t) {
			best.Renews = true
		}
	}
	return best
}

// PaidPeriodStart is where a newly paid period of a plan begins: at the end
// of the user's current access to that plan if it is still running, so paying
// early never loses days, otherwise now.
func PaidPeriodStart(cfg *Config, subs []Subscription, plan string, t time.Time) time.Time {
	var same []Subscription
	for _, s := range subs {
		if s.Plan == plan {
			same = append(same, s)
		}
	}
	a := Entitle(cfg, same, t)
	if a.Plan != nil && a.Until.After(t) {
		return a.Until
	}
	return t
}

// TrialDue reports whether a user should be granted the free trial: never
// had one, and the trial is configured.
func TrialDue(cfg *Config, trialStartedAt *time.Time) bool {
	return cfg.Trial.Days > 0 && trialStartedAt == nil
}

// ErrNoAmountFree means every invoice amount for a plan is in use.
var ErrNoAmountFree = errors.New("no free invoice amount; try again shortly")

// maxCents is how many distinct amounts one plan can have outstanding:
// price + 0.01 up to price + 0.99.
const maxCents = 99

// microPerCent is one US cent in micro-USDT.
const microPerCent = 10_000

// PickAmount chooses the amount that identifies a new USDT invoice: the
// plan's price plus a number of cents that no recent invoice uses. taken holds
// the amounts of invoices that are still open or could still be paid late.
//
// seed picks the starting point so concurrent customers spread out rather
// than all trying price + 0.01 first; any value is correct.
func PickAmount(price int64, taken map[int64]bool, seed int) (int64, error) {
	if seed < 0 {
		seed = -seed
	}
	for i := 0; i < maxCents; i++ {
		cents := int64((seed+i)%maxCents + 1)
		amount := price + cents*microPerCent
		if !taken[amount] {
			return amount, nil
		}
	}
	return 0, ErrNoAmountFree
}

// Invoice is an outstanding USDT invoice.
type Invoice struct {
	ID        int64
	UserID    int64
	Plan      string
	Address   string
	Amount    int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Payment is an incoming USDT transfer to the payment address.
type Payment struct {
	Tx        string
	From      string
	Amount    int64
	BlockTime time.Time
}

// Match finds the invoice a payment settles: same exact amount, paid after
// the invoice was issued and no later than its expiry plus grace. With
// several candidates, which PickAmount prevents, the newest wins, so a stale
// invoice can never capture a fresh payment.
func Match(p Payment, open []Invoice, grace time.Duration) (Invoice, bool) {
	var best Invoice
	var found bool
	for _, inv := range open {
		if inv.Amount != p.Amount {
			continue
		}
		if p.BlockTime.Before(inv.CreatedAt) || p.BlockTime.After(inv.ExpiresAt.Add(grace)) {
			continue
		}
		if !found || inv.CreatedAt.After(best.CreatedAt) {
			best, found = inv, true
		}
	}
	return best, found
}
