package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mozer/tether-risk/internal/billing"
)

// The monitor rescreens watched addresses on a schedule and alerts when risk
// worsens (docs/DECISIONS.md D27). It uses the internal API directly, not the
// gate: a watch is paid for by the plan's watch allowance, not by daily
// screens.

// riskCategories are the categories whose first appearance is an alert. They
// match the report's risk checks.
var riskCategories = []string{
	"sanctions", "terrorist_financing", "darknet", "stolen_funds",
	"mixer", "scam", "high_risk_exchange", "gambling",
}

// bandRank orders bands so "worse" is a comparison.
var bandRank = map[string]int{"low": 1, "medium": 2, "high": 3}

// stateOf reduces a screening result to what alerts compare.
func stateOf(r *screenResponse) billing.WatchState {
	st := billing.WatchState{Band: r.Band, Score: r.Score, Coverage: r.Coverage,
		Listed: r.OwnLabel != nil || r.SanctionsOverride, RiskCategories: []string{}}
	seen := map[string]bool{}
	for _, d := range []*direction{r.Inbound, r.Outbound} {
		if d == nil {
			continue
		}
		for _, c := range d.Categories {
			if c.Pct > 0 && slices.Contains(riskCategories, c.Category) && !seen[c.Category] {
				seen[c.Category] = true
				st.RiskCategories = append(st.RiskCategories, c.Category)
			}
		}
	}
	slices.Sort(st.RiskCategories)
	return st
}

// worsening is what changed for the worse between two states.
type worsening struct {
	BandUp        bool
	NewCategories []string
	NewlyListed   bool
}

func (w worsening) any() bool { return w.BandUp || len(w.NewCategories) > 0 || w.NewlyListed }

// compare decides whether a new state warrants an alert. Only worsening
// counts: an address getting cleaner, or more of its value being traced, is
// not news to someone watching it for risk. The first check establishes the
// baseline and never alerts.
func compare(prev *billing.WatchState, cur billing.WatchState) worsening {
	if prev == nil {
		return worsening{}
	}
	var w worsening
	w.BandUp = bandRank[cur.Band] > bandRank[prev.Band]
	for _, c := range cur.RiskCategories {
		if !slices.Contains(prev.RiskCategories, c) {
			w.NewCategories = append(w.NewCategories, c)
		}
	}
	w.NewlyListed = cur.Listed && !prev.Listed
	return w
}

// wakeMonitor asks for a pass now, so a new watch gets its first check
// within moments rather than at the next interval.
func (b *bot) wakeMonitor() {
	select {
	case b.monitorWake <- struct{}{}:
	default:
	}
}

// monitor runs passes until ctx ends.
func (b *bot) monitor(ctx context.Context) {
	// Check often; each pass takes only watches due for a check.
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		if err := b.monitorPass(ctx); err != nil && ctx.Err() == nil {
			b.log.Warn("monitor pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-b.monitorWake:
		}
	}
}

// monitorPass checks every watch due. Watches of users without a plan that
// includes watches are skipped: they stay listed, and resume if the user
// subscribes again.
func (b *bot) monitorPass(ctx context.Context) error {
	now := b.now()
	due, err := b.store.DueWatches(ctx, now.Add(-b.billing.Monitor.Interval), b.billing.Monitor.PerPass)
	if err != nil {
		return err
	}
	for _, w := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		acc, err := b.access(ctx, w.UserID)
		if err != nil {
			return err
		}
		if !acc.Admin && (acc.Plan == nil || acc.Watches == 0) {
			_ = b.store.TouchWatch(ctx, w.ID, now)
			continue
		}
		b.checkWatch(ctx, w, now)
	}
	return nil
}

func (b *bot) checkWatch(ctx context.Context, w billing.Watch, now time.Time) {
	select {
	case b.screening <- struct{}{}:
	case <-ctx.Done():
		return
	}
	res, _, err := b.screen(ctx, w.Chain, w.Address)
	<-b.screening
	if err != nil {
		b.log.Warn("watch check failed", "watch", w.ID, "address", w.Address, "error", err)
		_ = b.store.TouchWatch(ctx, w.ID, now)
		return
	}
	cur := stateOf(res)
	change := compare(w.Last, cur)
	if change.any() {
		b.say(ctx, w.UserID, alertText(b.langOf(ctx, w.UserID), w, w.Last, cur, change))
	}
	if err := b.store.SetWatchState(ctx, w.ID, cur, now, change.any()); err != nil {
		b.log.Error("record watch state", "watch", w.ID, "error", err)
	}
}

func alertText(lang string, w billing.Watch, prev *billing.WatchState, cur billing.WatchState, ch worsening) string {
	var sb strings.Builder
	name := ""
	if w.Label != "" {
		name = w.Label + "\n"
	}
	sb.WriteString(t(lang, "alert_title", name, w.Address))
	if ch.BandUp {
		sb.WriteString(t(lang, "alert_band", bandWord(lang, prev.Band), bandWord(lang, cur.Band), prev.Score, cur.Score))
	}
	if len(ch.NewCategories) > 0 {
		names := make([]string, len(ch.NewCategories))
		for i, c := range ch.NewCategories {
			names[i] = categoryWord(lang, c)
		}
		sb.WriteString(t(lang, "alert_category", strings.Join(names, ", ")))
	}
	if ch.NewlyListed {
		sb.WriteString(t(lang, "alert_listed"))
	}
	sb.WriteString(t(lang, "alert_footer"))
	return sb.String()
}

// categoryWord names a category for a chat message.
func categoryWord(lang, c string) string {
	en := map[string]string{
		"sanctions": "Sanctions", "terrorist_financing": "Terrorist Financing", "darknet": "Darknet Market",
		"stolen_funds": "Stolen Funds", "mixer": "Mixer", "scam": "Scam",
		"high_risk_exchange": "High-Risk Exchange", "gambling": "Gambling",
	}
	tr := map[string]string{
		"sanctions": "Yaptırımlar", "terrorist_financing": "Terörün Finansmanı", "darknet": "Darknet Pazarı",
		"stolen_funds": "Çalıntı Fonlar", "mixer": "Karıştırıcı (Mixer)", "scam": "Dolandırıcılık",
		"high_risk_exchange": "Yüksek Riskli Borsa", "gambling": "Kumar",
	}
	m := en
	if lang == langTR {
		m = tr
	}
	if n, ok := m[c]; ok {
		return n
	}
	return fmt.Sprint(c)
}
