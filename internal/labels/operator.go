package labels

import (
	"math/big"
	"sort"
	"time"
)

// Naming wallets by who created them (docs/DECISIONS.md D31).
//
// A TRON account exists once something pays to create it. An exchange's
// reserve wallets are the exchange's own, proven by its signed list, so two
// links through account creation are as good as the list:
//
//   - whoever created a reserve wallet is the exchange: HTX's reserves were
//     created with transfers of up to 951M TRX, which only the owner makes.
//     A creation below minCreatorTRX is not counted: anyone can send a
//     fraction of a TRX to an address, and two reserves were first reached
//     by 0.1 and 1 TRX;
//   - whatever a reserve wallet created is the exchange's: reserves are cold
//     wallets and never pay out to customers, so what they create is internal
//     (Poloniex's reserve TWhDfw…5M2 created 265 of its deposit wallets).
//
// Wallets created by a hot wallet are deliberately not named. A hot wallet
// pays customers, and a customer's fresh wallet is created by that payout,
// so "created by an exchange's hot wallet" describes customers too.

// Activation is who created an account, as stored.
type Activation struct {
	Address   string
	Activator string
	TxID      string
	Amount    *big.Int // in sun
	At        time.Time
}

// ReserveOperators names the creators of reserve wallets and the wallets
// reserves created. reserves are reserve-list labels; named holds every
// label that already names an exchange, by address, so a wallet named for
// one exchange is never renamed for another.
func ReserveOperators(acts []Activation, reserves []Label, named map[string][]Label,
	minCreatorTRX int64, confidence float64, chainID string) []Label {

	minSun := new(big.Int).Mul(big.NewInt(minCreatorTRX), big.NewInt(1_000_000))

	reserveAt := map[string]Label{}
	for _, r := range reserves {
		reserveAt[r.Address] = r
	}
	// sameExchange reports whether addr is already named for exchange; other
	// reports whether it is named for a different one.
	check := func(addr, exchange string) (same, other bool) {
		for _, l := range named[addr] {
			if n := exchangeName(l); n == exchange {
				same = true
			} else if n != "" {
				other = true
			}
		}
		return
	}

	out := map[string]Label{}
	add := func(addr string, r Label, heuristic string, a Activation) {
		exchange := exchangeName(r)
		if addr == "" || exchange == "" {
			return
		}
		if _, ok := reserveAt[addr]; ok {
			return
		}
		if same, other := check(addr, exchange); same || other {
			return
		}
		if _, done := out[addr]; done {
			return
		}
		suffix := " (created its reserve wallet)"
		if heuristic == "created_by_reserve" {
			suffix = " (created by its reserve wallet)"
		}
		trx := new(big.Rat).SetFrac(a.Amount, big.NewInt(1_000_000)).FloatString(6)
		out[addr] = Label{
			Chain: chainID, Address: addr,
			Entity:     exchange + suffix,
			Category:   r.Category, // the reserve list's: identity, not a KYC tier (D19)
			Confidence: confidence,
			Source:     "derived:operator",
			Evidence: map[string]any{
				"heuristic":   heuristic,
				"exchange":    exchange,
				"reserve":     r.Address,
				"activation":  a.TxID,
				"created":     a.Address,
				"creator":     a.Activator,
				"trx":         trx,
				"activated":   a.At.Format(time.DateOnly),
				"reserve_src": r.Source,
			},
		}
	}

	for _, a := range acts {
		if a.Activator == "" {
			continue
		}
		if r, ok := reserveAt[a.Address]; ok && a.Amount != nil && a.Amount.Cmp(minSun) >= 0 {
			add(a.Activator, r, "created_reserve", a)
		}
		if r, ok := reserveAt[a.Activator]; ok {
			add(a.Address, r, "created_by_reserve", a)
		}
	}

	addrs := make([]string, 0, len(out))
	for a := range out {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	labels := make([]Label, 0, len(addrs))
	for _, a := range addrs {
		labels = append(labels, out[a])
	}
	return labels
}

// OperatorInputs splits the current labels into those that name an address
// for ReserveOperators and the addresses ReserveOperators itself named last
// time. Its own output must not be an input: read as "already named", it
// made a rerun produce nothing and withdraw every label it had made.
func OperatorInputs(current []Label) (named map[string][]Label, previous map[string]bool) {
	named, previous = map[string][]Label{}, map[string]bool{}
	for _, l := range current {
		if l.Source == "derived:operator" {
			previous[l.Address] = true
			continue
		}
		named[l.Address] = append(named[l.Address], l)
	}
	return named, previous
}
