package labels

import (
	"math/big"
	"testing"
	"time"
)

func TestReserveOperators(t *testing.T) {
	htx := func(addr string) Label {
		return Label{Address: addr, Entity: "HTX (proof-of-reserves wallet)", Category: "named_service", Source: "htx_por"}
	}
	polo := Label{Address: "TPoloReserve", Entity: "Poloniex (proof-of-reserves wallet)", Category: "named_service", Source: "poloniex_por"}
	reserves := []Label{htx("THtxReserve"), htx("THtxReserve2"), polo}
	named := map[string][]Label{
		"THtxDeposit": {{Address: "THtxDeposit", Entity: "HTX (sends to its reserves)", Source: "derived:deposit"}},
	}
	act := func(addr, by string) Activation {
		return Activation{Address: addr, Activator: by, TxID: "tx-" + addr, Amount: big.NewInt(951_554_000_000_000), At: time.Unix(1650000000, 0)}
	}
	acts := []Activation{
		act("THtxReserve", "TTreasury"),     // created a reserve: HTX
		act("TNewWallet", "TPoloReserve"),   // created by a reserve: Poloniex
		act("TPoloReserve", "THtxReserve2"), // a reserve created by another exchange's reserve: both stay as listed
		act("THtxReserve2", "THtxDeposit"),  // creator already named HTX: nothing new
		act("TCustomer", "THtxHotWallet"),   // created by a hot wallet: not named
		act("TOrphan", ""),                  // no creating transaction
		// Reached first by a fraction of a TRX: anyone could have sent it.
		{Address: "THtxReserve3", Activator: "TSpammer", Amount: big.NewInt(100_000)},
	}
	reserves = append(reserves, htx("THtxReserve3"))

	got := ReserveOperators(acts, reserves, named, 10, 0.8, "tron")
	want := map[string]string{
		"TTreasury":  "HTX (created its reserve wallet)",
		"TNewWallet": "Poloniex (created by its reserve wallet)",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d labels, want %d: %+v", len(got), len(want), got)
	}
	for _, l := range got {
		if want[l.Address] != l.Entity || l.Source != "derived:operator" || l.Category != "named_service" {
			t.Errorf("unexpected label %+v", l)
		}
	}
	if got[0].Evidence["trx"] != "951554000.000000" {
		t.Errorf("evidence trx = %v", got[0].Evidence["trx"])
	}
}

// A wallet named for one exchange is never renamed for another, whichever
// rule reaches it.
func TestReserveOperatorsKeepsExistingNames(t *testing.T) {
	reserves := []Label{{Address: "TPoloReserve", Entity: "Poloniex (proof-of-reserves wallet)", Source: "poloniex_por"}}
	named := map[string][]Label{"THtxHot": {{Address: "THtxHot", Entity: "HTX (hot wallet)", Source: "derived:hotwallet"}}}
	acts := []Activation{{Address: "TPoloReserve", Activator: "THtxHot", Amount: big.NewInt(1)}}
	if got := ReserveOperators(acts, reserves, named, 10, 0.8, "tron"); len(got) != 0 {
		t.Errorf("renamed an HTX wallet: %+v", got)
	}
}

// A rerun over the labels the last run wrote names the same wallets again.
// It once read its own labels as "already named", produced nothing, and
// withdrew all eleven (docs/DECISIONS.md D31).
func TestReserveOperatorsRerunIsStable(t *testing.T) {
	reserves := []Label{{Address: "TReserve", Entity: "HTX (proof-of-reserves wallet)", Source: "htx_por"}}
	acts := []Activation{{Address: "TReserve", Activator: "TTreasury", Amount: big.NewInt(99_000_000)}}
	first := ReserveOperators(acts, reserves, nil, 10, 0.8, "tron")
	if len(first) != 1 {
		t.Fatalf("first run: %+v", first)
	}
	named, previous := OperatorInputs(append(reserves, first...))
	second := ReserveOperators(acts, reserves, named, 10, 0.8, "tron")
	if len(second) != 1 || second[0].Address != "TTreasury" || !previous["TTreasury"] {
		t.Errorf("rerun: %+v, previous %v", second, previous)
	}
}
