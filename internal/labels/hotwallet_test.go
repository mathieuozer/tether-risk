package labels

import (
	"testing"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/shopspring/decimal"
)

func TestJudgeHotWallets(t *testing.T) {
	rules := config.DerivedHotWallet{MinTransfersEachWay: 3, MinInflowUSD: 100000,
		MinCounterparties: 250, MinExclusiveShare: 0.9}
	usd := decimal.NewFromInt
	flows := []ReserveFlow{
		// Refilled and sweeping back, thousands of customers: HTX's hot wallet.
		{Wallet: "THot", Exchange: "HTX", InTx: 5937, InUSD: usd(37_687_003_937), BackTx: 4548, BackUSD: usd(11_498_704_646)},
		// Paid by reserves, never sends back: a withdrawal or a desk.
		{Wallet: "TOneWay", Exchange: "Poloniex", InTx: 8, InUSD: usd(1_500_000_000)},
		// Two-way but a handful of counterparties: cold storage.
		{Wallet: "TCold", Exchange: "HTX", InTx: 6, InUSD: usd(51_000_000), BackTx: 3, BackUSD: usd(45_000_000)},
		// Two-way with two exchanges: a market maker, not either exchange.
		{Wallet: "TMaker", Exchange: "HTX", InTx: 3, InUSD: usd(175_100_000), BackTx: 3, BackUSD: usd(10_792_500)},
		{Wallet: "TMaker", Exchange: "Poloniex", InTx: 89, InUSD: usd(42_807_705), BackTx: 16, BackUSD: usd(32_149_690)},
	}
	cps := map[string]int{"THot": 4171, "TOneWay": 2, "TCold": 4, "TMaker": 1741}
	got := map[string]HotWalletCandidate{}
	for _, c := range JudgeHotWallets(flows, cps, rules) {
		got[c.Wallet] = c
	}
	if c := got["THot"]; !c.Accepted || c.Exchange != "HTX" {
		t.Errorf("hot wallet: %+v", c)
	}
	for _, w := range []string{"TOneWay", "TCold", "TMaker"} {
		if got[w].Accepted {
			t.Errorf("%s accepted: %+v", w, got[w])
		}
	}
	if got["TMaker"].Rejected == "" || got["TCold"].Rejected == "" {
		t.Error("rejections carry no reason")
	}
}
