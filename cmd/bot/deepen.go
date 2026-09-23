package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Follow-up after an unfinished screen (docs/DECISIONS.md D28).
//
// A first screen of a fresh address can cover a few percent of its value:
// its counterparties are queued for the ingest worker, and the dead ends where
// the trail stops are queued ring by ring. Each rescreen after the worker
// catches up reaches one ring further. Measured on TJBsbT…8Y5P: 2.2% → 10.8%
// → 26.6% → 38.7% → 59.7% → 71.0% over five rounds. A customer should not
// have to know to do that by hand, so the bot does it and sends the final
// result. Rounds use the internal API directly and cost no daily screens.

// tracingDone reports whether a result has nothing left to trace.
func tracingDone(r *screenResponse) bool {
	d := r.Depth
	return d == nil || (!d.StillFetching && d.FrontierPending == 0 && d.Traced >= d.Counterparties)
}

// startFollowUp begins a background follow-up for an unfinished screen and
// reports whether one is now running for it. One per user and address; at
// most deepen.concurrent at once.
func (b *bot) startFollowUp(userID int64, lang, chain, address string, first *screenResponse) bool {
	if first == nil || tracingDone(first) {
		return false
	}
	key := fmt.Sprintf("%d/%s/%s", userID, chain, address)
	b.mu.Lock()
	if b.following[key] {
		b.mu.Unlock()
		return true
	}
	if len(b.following) >= b.billing.Deepen.Concurrent {
		b.mu.Unlock()
		return false
	}
	b.following[key] = true
	b.mu.Unlock()

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() {
			b.mu.Lock()
			delete(b.following, key)
			b.mu.Unlock()
		}()
		b.followUp(b.root, userID, lang, chain, address, first)
	}()
	return true
}

func (b *bot) followUp(ctx context.Context, userID int64, lang, chain, address string, first *screenResponse) {
	cfg := b.billing.Deepen
	deadline := b.now().Add(cfg.MaxDuration)
	final, prev := first, first.Coverage
	rounds, stalls := 0, 0

	for rounds < cfg.MaxRounds && b.now().Before(deadline) {
		// Wait for the worker to fetch what the last screen queued.
		for waited := time.Duration(0); waited < cfg.RoundWait; waited += cfg.Poll {
			if !b.sleep(ctx, cfg.Poll) {
				return
			}
			if n, err := b.store.FetchBacklog(ctx); err == nil && n == 0 {
				break
			}
		}

		select {
		case b.screening <- struct{}{}:
		case <-ctx.Done():
			return
		}
		res, _, err := b.screen(ctx, chain, address)
		<-b.screening
		rounds++
		if err != nil {
			b.log.Warn("follow-up round failed", "address", address, "round", rounds, "error", err)
			if stalls++; stalls >= 2 {
				break
			}
			continue
		}
		final = res
		gain := (res.Coverage - prev) * 100
		prev = res.Coverage
		if tracingDone(res) {
			break
		}
		// Two rounds in a row that add almost nothing: the trail is as deep
		// as the hop limit and stored history allow.
		if gain < cfg.MinGainPct {
			if stalls++; stalls >= 2 {
				break
			}
		} else {
			stalls = 0
		}
	}

	b.log.Info("follow-up done", "user", userID, "address", address, "rounds", rounds,
		"coverage_first", first.Coverage, "coverage_final", final.Coverage)
	if err := b.store.RecordScreen(ctx, screenRecord(userID, chain, address, "followup", final), b.now()); err != nil {
		b.log.Error("record follow-up", "user", userID, "error", err)
	}
	b.say(ctx, userID, followUpText(lang, first, final))
}

// followUpText is the message that closes a follow-up: what changed, then
// the final summary. A result that barely moved is said in one line rather
// than repeating the whole report.
func followUpText(lang string, first, final *screenResponse) string {
	moved := final.Band != first.Band || abs(final.Coverage-first.Coverage) >= 0.01 || abs(final.Score-first.Score) >= 0.5
	was := t(lang, "fu_state", bandWord(lang, first.Band), first.Score, pctText(lang, first.Coverage))
	now := t(lang, "fu_state", bandWord(lang, final.Band), final.Score, pctText(lang, final.Coverage))
	if !moved {
		return t(lang, "fu_same", final.Address, now)
	}
	var sb strings.Builder
	sb.WriteString(t(lang, "fu_title", final.Address))
	sb.WriteString(t(lang, "fu_change", was, now))
	sb.WriteString(summary(final, lang, false))
	return sb.String()
}

func pctText(lang string, v float64) string {
	s := fmt.Sprintf("%.1f", v*100)
	switch lang {
	case langTR:
		return "%" + strings.Replace(s, ".", ",", 1)
	case langRU:
		return strings.Replace(s, ".", ",", 1) + "%"
	}
	return s + "%"
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// realSleep waits d or until ctx ends, reporting whether it waited fully.
func realSleep(ctx context.Context, d time.Duration) bool {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-tm.C:
		return true
	}
}
