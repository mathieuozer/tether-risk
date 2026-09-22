package scoring

import (
	"testing"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/shopspring/decimal"
)

func behaviourRules() config.Behaviour {
	var r config.Behaviour
	r.PassThrough.MinVolumeUSD, r.PassThrough.MaxRetainedShare, r.PassThrough.MaxDays = 10000, 0.05, 30
	r.NewAddress.MaxAgeDays, r.NewAddress.HighVolumeUSD = 30, 1_000_000
	return r
}

func codes(fs []Flag) map[string]Flag {
	out := map[string]Flag{}
	for _, f := range fs {
		out[f.Code] = f
	}
	return out
}

func TestBehaviourFlags(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	d := func(n int) time.Time { return now.AddDate(0, 0, -n) }
	usd := decimal.NewFromInt

	// TJBsbT…8Y5P as measured: $7.11M in and out over 11 days, 12 days old.
	got := codes(BehaviourFlags(&Activity{InUSD: usd(7_110_000), OutUSD: usd(7_105_000),
		FirstSeen: d(12), LastSeen: d(1)}, now, behaviourRules()))
	if f, ok := got["pass_through"]; !ok || f.Days != 11 {
		t.Errorf("pass-through missed: %+v", got)
	}
	if f, ok := got["high_volume_new"]; !ok || f.AgeDays != 12 {
		t.Errorf("high-volume new address missed: %+v", got)
	}

	// TNwf8V…crmL: receives only, 40 days old. Nothing to note.
	if got := BehaviourFlags(&Activity{InUSD: usd(19_600), FirstSeen: d(40), LastSeen: d(3)}, now, behaviourRules()); len(got) != 0 {
		t.Errorf("flags on a receive-only old address: %+v", got)
	}

	// Keeps a quarter of what came in: not pass-through.
	if _, ok := codes(BehaviourFlags(&Activity{InUSD: usd(100_000), OutUSD: usd(75_000),
		FirstSeen: d(200), LastSeen: d(190)}, now, behaviourRules()))["pass_through"]; ok {
		t.Error("retaining 25% flagged as pass-through")
	}

	// New, small: plain new address.
	if _, ok := codes(BehaviourFlags(&Activity{InUSD: usd(500), FirstSeen: d(3), LastSeen: d(1)}, now, behaviourRules()))["new_address"]; !ok {
		t.Error("new address missed")
	}
}
