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

func flowRules() config.Behaviour {
	r := behaviourRules()
	r.RoundSplit.MinAmountUSD, r.RoundSplit.RoundToUSD, r.RoundSplit.MinRecipients, r.RoundSplit.MaxMinutes = 10000, 1000, 3, 60
	r.Parked.MinAmountUSD, r.Parked.MinWallets, r.Parked.MinTotalUSD = 10000, 2, 50000
	return r
}

// TPJZrw…uBhM as measured: $1,000,000 to six fresh wallets within three
// minutes, none of which has sent anything since.
func TestFlowFlagsSplitAndParked(t *testing.T) {
	at := time.Date(2026, 9, 21, 18, 53, 0, 0, time.UTC)
	var outs []OutTransfer
	for i := 0; i < 6; i++ {
		outs = append(outs, OutTransfer{To: string(rune('A' + i)), USD: decimal.NewFromInt(1_000_000), Transfers: 1,
			First: at.Add(time.Duration(i*30) * time.Second), ToFetched: true, ToSentUSD: decimal.Zero})
	}
	// A normal payment, and a round one to a wallet that did move it on.
	outs = append(outs,
		OutTransfer{To: "X", USD: decimal.NewFromFloat(203752.4), Transfers: 2, First: at, ToFetched: true, ToSentUSD: decimal.NewFromInt(5)},
		OutTransfer{To: "Y", USD: decimal.NewFromInt(400000), Transfers: 1, First: at, ToFetched: true, ToSentUSD: decimal.NewFromInt(400000)})

	got := codes(FlowFlags(outs, flowRules()))
	if f, ok := got["round_split"]; !ok || f.Count != 6 || !f.AmountUSD.Equal(decimal.NewFromInt(1_000_000)) || f.Minutes > 3 {
		t.Errorf("round split: %+v", got)
	}
	if f, ok := got["parked_funds"]; !ok || f.Count != 6 || !f.AmountUSD.Equal(decimal.NewFromInt(6_000_000)) {
		t.Errorf("parked funds: %+v", got)
	}

	// Unfetched recipients are never assumed parked; ordinary amounts are not a split.
	for i := range outs {
		outs[i].ToFetched = false
		outs[i].USD = decimal.NewFromFloat(123456.78)
	}
	if got := FlowFlags(outs, flowRules()); len(got) != 0 {
		t.Errorf("flags on unfetched, non-round payments: %+v", got)
	}
}
