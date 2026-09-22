package labels

import (
	"testing"
	"time"
)

func TestJudgePoisoning(t *testing.T) {
	real := time.Date(2026, 6, 15, 7, 0, 0, 0, time.UTC)
	pairs := []PoisoningPair{
		// Reacts eleven minutes after the victim paid its usual counterparty,
		// and a second victim later paid it.
		{Sender: "TKbGx1PoisonerAAAAAAAAAAAAAAAAxY9z", Victim: "TVictim1", Imitated: "TKbGx1RealCounterpartyBBBBBBBxY9z",
			RealFirst: real, DustFirst: real.Add(11 * time.Minute)},
		{Sender: "TKbGx1PoisonerAAAAAAAAAAAAAAAAxY9z", Victim: "TVictim2", Imitated: "TKbGx1RealCounterpartyBBBBBBBxY9z",
			RealFirst: real, DustFirst: real.Add(2 * time.Hour), StolenUSD: 12500},
		// Dust before any real transfer: not a reaction, so not labelled.
		{Sender: "TVanity9999", Victim: "TVictim3", Imitated: "TVanity0009999", RealFirst: real, DustFirst: real.Add(-time.Hour)},
		// Degenerate rows are ignored.
		{Sender: "TSame", Victim: "TVictim4", Imitated: "TSame", RealFirst: real, DustFirst: real},
	}
	got := JudgePoisoning(pairs, 0.9, "tron")
	if len(got) != 1 {
		t.Fatalf("got %d labels, want 1: %+v", len(got), got)
	}
	l := got[0]
	if l.Category != "scam" || l.Source != "derived:poisoning" || l.Entity != "Address poisoning (imitates TKbGx1…xY9z)" {
		t.Errorf("label %+v", l)
	}
	if l.Evidence["victims"] != 2 || l.Evidence["stolen_usd"] != "12500.00" || l.Evidence["minutes_after"] != 11 {
		t.Errorf("evidence %+v", l.Evidence)
	}
}
