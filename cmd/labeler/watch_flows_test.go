package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/mozer/tether-risk/internal/billing"
	"github.com/mozer/tether-risk/internal/labels"
)

func TestDueWatchesOnlyForWhatARescreenWouldReport(t *testing.T) {
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	before, after := at.Add(-time.Hour), at.Add(10*time.Minute)
	withScam := &billing.WatchState{RiskCategories: []string{"scam"}}
	byAddr := map[string][]watchRow{
		"W1": {{id: 1, address: "W1", checked: &before}},                 // checked before: due
		"W2": {{id: 2, address: "W2", checked: &after}},                  // checked after the transfer: seen
		"W3": {{id: 3, address: "W3", checked: &before, last: withScam}}, // already shows scam
		"W4": {{id: 4, address: "W4"}, {id: 5, address: "W4"}},           // never checked, two users
		"W5": {{id: 6, address: "W5", checked: &before}},                 // only an exchange contact
	}
	known := map[string][]labels.Label{
		"SCAM": {{Category: "scam"}},
		"EXCH": {{Category: "exchange"}},
		"SANC": {{Category: "sanctions"}},
	}
	flows := []flow{
		{"W1", "SCAM", at}, {"W1", "SANC", at}, // one watch, two reasons: due once
		{"W2", "SCAM", at},
		{"W3", "SCAM", at},
		{"W4", "SANC", at},
		{"W5", "EXCH", at},
	}
	if got := dueWatches(flows, byAddr, known); !reflect.DeepEqual(got, []int64{1, 4, 5}) {
		t.Errorf("due = %v, want [1 4 5]", got)
	}
	// A new category for W3 does trigger it.
	if got := dueWatches([]flow{{"W3", "SANC", at}}, byAddr, known); !reflect.DeepEqual(got, []int64{3}) {
		t.Errorf("new category: due = %v, want [3]", got)
	}
	// A check made within TronGrid's lag may not have seen the transfer.
	soon := at.Add(time.Minute)
	lagged := map[string][]watchRow{"W": {{id: 9, address: "W", checked: &soon}}}
	if got := dueWatches([]flow{{"W", "SCAM", at}}, lagged, known); !reflect.DeepEqual(got, []int64{9}) {
		t.Errorf("check within the lag: due = %v, want [9]", got)
	}
}
