package main

import (
	"context"
	"fmt"
	"time"

	"github.com/mozer/tether-risk/internal/billing"
)

// Controls against the product being used to launder (docs/DECISIONS.md
// D43): screening patterns worth a human's look, and payments from risky
// sources. Nothing here blocks a user by itself.

// screenRecord is a screen as the history keeps it.
func screenRecord(userID int64, chain, address, channel string, res *screenResponse) billing.ScreenRecord {
	r := billing.ScreenRecord{UserID: userID, Chain: chain, Address: address, Channel: channel}
	if res == nil {
		return r
	}
	r.Score, r.Coverage, r.Band = &res.Score, &res.Coverage, &res.Band
	if res.Verdict != nil && res.Verdict.Level != "" {
		v := res.Verdict.Level
		r.Verdict = &v
	}
	if res.Activity != nil && res.Activity.FirstSeen != "" {
		if t, err := time.Parse("2006-01-02", res.Activity.FirstSeen[:min(10, len(res.Activity.FirstSeen))]); err == nil {
			r.FirstSeen = &t
		}
	}
	return r
}

// checkPatterns raises a user's screening pattern to the admins when it
// crosses a threshold in billing.yaml, once a day per pattern.
func (b *bot) checkPatterns(ctx context.Context, userID int64, address string, now time.Time) {
	a := b.billing.Abuse
	if a.Window <= 0 {
		return
	}
	p, err := b.store.ScreenPatterns(ctx, userID, address, now.Add(-a.Window), a.FreshDays)
	if err != nil {
		b.log.Warn("screen patterns", "user", userID, "error", err)
		return
	}
	type check struct {
		on          bool
		kind, key   string
		description string
	}
	for _, c := range []check{
		{a.SameAddress > 0 && p.SameAddress >= a.SameAddress, "repeat_screen", address,
			fmt.Sprintf("screened %s %d times in %s: possibly testing whether their own wallet looks clean", address, p.SameAddress, a.Window)},
		{a.RiskyTargets > 0 && p.Risky >= a.RiskyTargets, "many_risky", "",
			fmt.Sprintf("screened %d distinct addresses answered risky in %s", p.Risky, a.Window)},
		{a.FreshTargets > 0 && p.Fresh >= a.FreshTargets, "many_fresh", "",
			fmt.Sprintf("screened %d distinct addresses first active within %d days, in %s", p.Fresh, a.FreshDays, a.Window)},
	} {
		if !c.on {
			continue
		}
		raised, err := b.store.RaiseFlag(ctx, userID, c.kind, c.key, c.description, now)
		if err != nil {
			b.log.Warn("raise flag", "user", userID, "error", err)
			continue
		}
		if raised {
			b.notifyAdmins(ctx, fmt.Sprintf("⚠️ Pattern to review, user %d: %s.\nNothing was blocked. /user %d shows their history.", userID, c.description, userID))
		}
	}
}

// payerRisk screens a paying address. It returns the reasons when the
// screen answers risky, nil when not; an error means the payment should be
// retried later rather than granted unscreened.
func (b *bot) payerRisk(ctx context.Context, from string) ([]string, error) {
	res, _, err := b.screen(ctx, "tron", from)
	if err != nil {
		return nil, err
	}
	if res.Verdict == nil || res.Verdict.Level != "high_risk" {
		return nil, nil
	}
	var why []string
	for _, r := range res.Verdict.Reasons {
		w := r.Code
		if r.Category != "" {
			w += " " + r.Category
		}
		if r.Pct > 0 {
			w += fmt.Sprintf(" %.1f%%", r.Pct)
		}
		why = append(why, w)
	}
	if res.OwnLabel != nil {
		why = append([]string{"listed: " + res.OwnLabel.Entity}, why...)
	}
	return why, nil
}
