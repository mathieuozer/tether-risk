package scoring

import (
	"testing"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/shopspring/decimal"
)

func d(f float64) decimal.Decimal { return decimal.NewFromFloat(f) }

// As measured for TVnZo4…gRFR on 2026-09-28 (D48): its direct payers had
// taken millions from OFAC terrorist-financing and Tether-frozen addresses,
// and the report showed none of it.
func TestJudgeContacts(t *testing.T) {
	rules := config.CounterpartyContact{Enabled: true, MinUSD: 10_000, MaxListed: 2}
	listed := map[string]ListedAddress{
		"TQ5H49": {Category: "terrorist_financing", Entity: "Arash Estaki ALIVAND"},
		"TRJqS9": {Category: "frozen_funds", Entity: "Frozen by Tether (USDT blacklist)"},
		"TL34x6": {Category: "frozen_funds", Entity: "Frozen by Tether (USDT blacklist)"},
		"TQ5mpZ": {Category: "terrorist_financing", Entity: "ANSARALLAH"},
	}
	cps := []ContactCounterparty{
		{Address: "TNYhZC", Category: "unnamed_service", Inbound: true, FlowUSD: d(26_000), TotalUSD: d(90_334_968), Fetched: true},
		{Address: "TB1R8k", Category: "unnamed_service", Inbound: true, FlowUSD: d(29_550), TotalUSD: d(391_160_505), Fetched: true},
		{Address: "TCCS8K", Inbound: true, FlowUSD: d(109_890), Fetched: false},
		{Address: "TQ1shn", Category: "unnamed_service", Inbound: true, FlowUSD: d(29_700), TotalUSD: d(684_394_046), Fetched: true},
		{Address: "TClean", Inbound: true, FlowUSD: d(500_000), TotalUSD: d(1_000_000), Fetched: true},
		{Address: "TSmall", Inbound: true, FlowUSD: d(100), TotalUSD: d(50_000), Fetched: true},
	}
	flows := []ContactFlow{
		{Counterparty: "TNYhZC", Inbound: true, Listed: "TQ5H49", USD: d(4_114_000), Transfers: 8},
		{Counterparty: "TB1R8k", Inbound: true, Listed: "TRJqS9", USD: d(5_548_721), Transfers: 13},
		{Counterparty: "TCCS8K", Inbound: true, Listed: "TL34x6", USD: d(1_689_436), Transfers: 2},
		{Counterparty: "TQ1shn", Inbound: true, Listed: "TQ5mpZ", USD: d(1_300_000), Transfers: 4},
		{Counterparty: "TQ1shn", Inbound: true, Listed: "TRJqS9", USD: d(2_000), Transfers: 1},
		{Counterparty: "TSmall", Inbound: true, Listed: "TRJqS9", USD: d(9_999), Transfers: 1},
		{Counterparty: "TClean", Inbound: true, Listed: "TUnknown", USD: d(1_000_000), Transfers: 1}, // not listed
		// What TNYhZC paid listed addresses is its outbound side, which is
		// not the side it paid this address from.
		{Counterparty: "TNYhZC", Inbound: false, Listed: "TQ5mpZ", USD: d(9_000_000), Transfers: 1},
	}
	got := JudgeContacts(cps, flows, listed, rules)
	if len(got) != 2 {
		t.Fatalf("want 2 contacts (max_listed), got %d: %+v", len(got), got)
	}
	if got[0].Address != "TB1R8k" || got[1].Address != "TNYhZC" {
		t.Errorf("largest risky flow first: got %s, %s", got[0].Address, got[1].Address)
	}
	if s := got[1].SharePct.StringFixed(1); s != "4.6" {
		t.Errorf("TNYhZC share = %s, want 4.6", s)
	}
	if r := got[1].Risks[0]; r.Category != "terrorist_financing" || r.Entity != "Arash Estaki ALIVAND" || r.Addresses != 1 {
		t.Errorf("TNYhZC risk = %+v", r)
	}

	rules.MaxListed = 10
	got = JudgeContacts(cps, flows, listed, rules)
	if len(got) != 4 {
		t.Fatalf("want 4 contacts, got %d", len(got))
	}
	// Two categories, largest first, on one counterparty.
	var q1 *Contact
	var partial *Contact
	for i := range got {
		switch got[i].Address {
		case "TQ1shn":
			q1 = &got[i]
		case "TCCS8K":
			partial = &got[i]
		}
	}
	if q1 == nil || len(q1.Risks) != 2 || q1.Risks[0].Category != "terrorist_financing" || q1.Risks[1].Category != "frozen_funds" {
		t.Errorf("TQ1shn risks = %+v", q1)
	}
	if q1 != nil && q1.SharePct.StringFixed(2) != "0.19" {
		t.Errorf("TQ1shn share = %s", q1.SharePct.StringFixed(2))
	}
	// An unfetched counterparty is shown, but without a share: its total is
	// not known.
	if partial == nil || !partial.Partial || !partial.SharePct.IsZero() {
		t.Errorf("TCCS8K = %+v", partial)
	}
	// Below min_usd, and flows with unlisted addresses, are not contacts.
	for _, c := range got {
		if c.Address == "TSmall" || c.Address == "TClean" {
			t.Errorf("%s should not be listed", c.Address)
		}
	}
}
