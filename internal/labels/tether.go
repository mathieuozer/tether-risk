package labels

import (
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/mozer/tether-risk/internal/chain/tron"
)

// Tether's USDT blacklist (docs/DECISIONS.md D29).
//
// Tether freezes USDT at addresses tied to hacks, scams, sanctions and law
// enforcement requests, by calling addBlackList on its own contract. The
// events are on chain: an authoritative, first-party record that costs
// nothing to read and needs no inference. An address is frozen after its
// latest AddedBlackList and before any later RemovedBlackList.

// FrozenAddress is an address Tether currently blacklists.
type FrozenAddress struct {
	Address string
	AddedAt time.Time
	AddedTx string
	// DestroyedRaw is USDT (6 decimals) Tether destroyed at the address.
	DestroyedRaw *big.Int
}

// TetherBlacklist folds the contract's events into the current list. Events
// may arrive in any order; they are replayed by time, then transaction id,
// so the result is deterministic (docs/DECISIONS.md D6).
func TetherBlacklist(added, removed, destroyed []tron.ContractEvent) []FrozenAddress {
	type ev struct {
		tron.ContractEvent
		add bool
	}
	var all []ev
	for _, e := range added {
		all = append(all, ev{e, true})
	}
	for _, e := range removed {
		all = append(all, ev{e, false})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].Time.Equal(all[j].Time) {
			return all[i].Time.Before(all[j].Time)
		}
		return all[i].TxID < all[j].TxID
	})

	current := map[string]*FrozenAddress{}
	for _, e := range all {
		addr := e.Result["_user"]
		if addr == "" {
			continue
		}
		if e.add {
			current[addr] = &FrozenAddress{Address: addr, AddedAt: e.Time, AddedTx: e.TxID, DestroyedRaw: new(big.Int)}
		} else {
			delete(current, addr)
		}
	}
	for _, e := range destroyed {
		f, ok := current[e.Result["_blackListedUser"]]
		if !ok {
			continue
		}
		if v, ok := new(big.Int).SetString(e.Result["_balance"], 10); ok {
			f.DestroyedRaw.Add(f.DestroyedRaw, v)
		}
	}

	out := make([]FrozenAddress, 0, len(current))
	for _, f := range current {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// TetherLabels turns the blacklist into labels.
func TetherLabels(frozen []FrozenAddress, confidence float64) []Label {
	out := make([]Label, 0, len(frozen))
	for _, f := range frozen {
		ev := map[string]any{
			"contract":  tron.USDTContract,
			"added_at":  f.AddedAt.Format(time.RFC3339),
			"added_tx":  f.AddedTx,
			"reproduce": "https://tronscan.org/#/transaction/" + f.AddedTx,
			"note":      "Tether called addBlackList for this address on its USDT contract; USDT here cannot move.",
		}
		if f.DestroyedRaw.Sign() > 0 {
			ev["destroyed_usdt"] = usdtString(f.DestroyedRaw)
		}
		out = append(out, Label{
			Chain:      "tron",
			Address:    f.Address,
			Entity:     "Frozen by Tether (USDT blacklist)",
			Category:   "frozen_funds",
			Confidence: confidence,
			Source:     "tether_blacklist",
			Evidence:   ev,
		})
	}
	return out
}

func usdtString(raw *big.Int) string {
	q, r := new(big.Int).QuoRem(raw, big.NewInt(1_000_000), new(big.Int))
	return fmt.Sprintf("%s.%06d", q.String(), r.Int64())
}
