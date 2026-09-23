package labels

import (
	"math/big"
	"testing"
	"time"

	"github.com/mozer/tether-risk/internal/chain/tron"
)

func TestTetherBlacklistReplaysEvents(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 1, 1, 0, m, 0, 0, time.UTC) }
	ev := func(m int, tx string, kv ...string) tron.ContractEvent {
		r := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			r[kv[i]] = kv[i+1]
		}
		return tron.ContractEvent{TxID: tx, Time: at(m), Result: r}
	}
	added := []tron.ContractEvent{
		ev(1, "a1", "_user", "TFrozen"),
		ev(2, "a2", "_user", "TReleased"),
		ev(5, "a3", "_user", "TRefrozen"),
		ev(9, "a4", "_user", "TRefrozen"), // frozen again after release
	}
	removed := []tron.ContractEvent{
		ev(3, "r1", "_user", "TReleased"),
		ev(6, "r2", "_user", "TRefrozen"),
	}
	destroyed := []tron.ContractEvent{
		ev(4, "d1", "_blackListedUser", "TFrozen", "_balance", "18529513880"),
		ev(4, "d2", "_blackListedUser", "TReleased", "_balance", "5"), // not listed any more
	}
	got := TetherBlacklist(added, removed, destroyed)
	if len(got) != 2 || got[0].Address != "TFrozen" || got[1].Address != "TRefrozen" {
		t.Fatalf("got %+v, want TFrozen and TRefrozen", got)
	}
	if got[1].AddedTx != "a4" {
		t.Errorf("refrozen address dated from %s, want the latest freeze a4", got[1].AddedTx)
	}
	labels := TetherLabels(got, 1.0)
	if labels[0].Category != "frozen_funds" || labels[0].Evidence["destroyed_usdt"] != "18529.513880" {
		t.Errorf("label = %+v", labels[0])
	}
}

// Tether blacklisted its own contract; a mistaken send there is not a
// contact with frozen funds (D46).
func TestTetherLabelsLeaveOutTokenContracts(t *testing.T) {
	frozen := []FrozenAddress{
		{Address: tron.USDTContract, DestroyedRaw: new(big.Int)},
		{Address: "TTmnEn8CEjQnJpHwEvkRxBK25EJfKyPLE8", DestroyedRaw: new(big.Int)},
	}
	got := TetherLabels(frozen, 1)
	if len(got) != 1 || got[0].Address != "TTmnEn8CEjQnJpHwEvkRxBK25EJfKyPLE8" {
		t.Errorf("labels = %+v, want only the wallet", got)
	}
}
