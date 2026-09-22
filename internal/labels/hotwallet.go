package labels

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/shopspring/decimal"
)

// Derived exchange hot wallets (docs/DECISIONS.md D28).
//
// An exchange's reserve wallets refill its hot wallets and the hot wallets
// sweep surplus back, so a wallet exchanging transfers both ways with one
// exchange's reserves is that exchange's wallet. Two-way flow is what
// separates a hot wallet from a customer withdrawing from reserves or an OTC
// desk being paid: those flows go one way. Service shape (hundreds of
// counterparties) separates a hot wallet from the exchange's own cold
// storage, which the reserve list already names. Exclusivity guards against a
// market maker trading with several exchanges.

// ReserveFlow is the traffic between one wallet and one exchange's reserves.
type ReserveFlow struct {
	Wallet     string
	Exchange   string
	InTx       uint64 // transfers from the reserves to the wallet
	InUSD      decimal.Decimal
	BackTx     uint64 // transfers from the wallet to the reserves
	BackUSD    decimal.Decimal
	Reserve    string // the reserve wallet with the most inflow, for evidence
	ReserveSrc string
}

// HotWalletCandidate is one wallet's judgement, accepted or not.
type HotWalletCandidate struct {
	Wallet         string
	Exchange       string
	Counterparties int
	Flow           ReserveFlow
	ExclusiveShare decimal.Decimal
	Accepted       bool
	Rejected       string
}

// JudgeHotWallets decides which wallets are exchange hot wallets from their
// reserve flows and counterparty counts. It is pure, so the rules are tested
// without a database.
func JudgeHotWallets(flows []ReserveFlow, counterparties map[string]int, rules config.DerivedHotWallet) []HotWalletCandidate {
	byWallet := map[string][]ReserveFlow{}
	for _, f := range flows {
		byWallet[f.Wallet] = append(byWallet[f.Wallet], f)
	}
	wallets := make([]string, 0, len(byWallet))
	for w := range byWallet {
		wallets = append(wallets, w)
	}
	sort.Strings(wallets) // determinism (docs/DECISIONS.md D6)

	minIn := decimal.NewFromFloat(rules.MinInflowUSD)
	minShare := decimal.NewFromFloat(rules.MinExclusiveShare)
	var out []HotWalletCandidate
	for _, w := range wallets {
		fs := byWallet[w]
		total := decimal.Zero
		best := fs[0]
		for _, f := range fs {
			v := f.InUSD.Add(f.BackUSD)
			total = total.Add(v)
			if v.GreaterThan(best.InUSD.Add(best.BackUSD)) ||
				(v.Equal(best.InUSD.Add(best.BackUSD)) && f.Exchange < best.Exchange) {
				best = f
			}
		}
		c := HotWalletCandidate{Wallet: w, Exchange: best.Exchange, Counterparties: counterparties[w], Flow: best}
		if total.IsPositive() {
			c.ExclusiveShare = best.InUSD.Add(best.BackUSD).Div(total)
		}
		switch {
		case int(best.InTx) < rules.MinTransfersEachWay || int(best.BackTx) < rules.MinTransfersEachWay:
			c.Rejected = fmt.Sprintf("one-way flow: %d in, %d back, need %d each way",
				best.InTx, best.BackTx, rules.MinTransfersEachWay)
		case best.InUSD.LessThan(minIn):
			c.Rejected = fmt.Sprintf("reserves sent $%s, need $%s", best.InUSD.StringFixed(0), minIn.StringFixed(0))
		case c.Counterparties < rules.MinCounterparties:
			c.Rejected = fmt.Sprintf("%d counterparties, need %d: not service-shaped", c.Counterparties, rules.MinCounterparties)
		case c.ExclusiveShare.LessThan(minShare):
			c.Rejected = fmt.Sprintf("only %s of its reserve flows are with %s", pct(c.ExclusiveShare), best.Exchange)
		default:
			c.Accepted = true
		}
		out = append(out, c)
	}
	return out
}

// exchangeName is the exchange a reserve label names: "HTX (proof-of-reserves
// wallet)" is HTX.
func exchangeName(l Label) string {
	name, _, _ := strings.Cut(l.Entity, " (")
	return strings.TrimSpace(name)
}

// DeriveHotWallets finds hot wallets of the exchanges whose reserve wallets
// are labelled. reserves are the reserve-list labels at the snapshot.
func DeriveHotWallets(ctx context.Context, ch *sql.DB, cfg *config.Config, reserves []Label, chainID string) ([]HotWalletCandidate, []Label, error) {
	rules := cfg.Weights.DerivedHotWallet
	if !rules.Enabled || len(reserves) == 0 {
		return nil, nil, nil
	}
	exchangeOf := map[string]Label{}
	addrs := make([]string, 0, len(reserves))
	for _, l := range reserves {
		if exchangeName(l) == "" {
			continue
		}
		exchangeOf[l.Address] = l
		addrs = append(addrs, l.Address)
	}
	sort.Strings(addrs)

	rows, err := ch.QueryContext(ctx, `
		SELECT wallet, reserve, sum(in_tx), sum(in_usd), sum(back_tx), sum(back_usd) FROM (
			SELECT to_address AS wallet, from_address AS reserve,
			       transfer_count AS in_tx, total_usd_value AS in_usd, toUInt64(0) AS back_tx, toDecimal128(0, 6) AS back_usd
			FROM edges_current WHERE chain = ? AND from_address IN (?) AND to_address NOT IN (?)
			UNION ALL
			SELECT from_address, to_address, toUInt64(0), toDecimal128(0, 6), transfer_count, total_usd_value
			FROM edges_by_to_current WHERE chain = ? AND to_address IN (?) AND from_address NOT IN (?))
		GROUP BY wallet, reserve`,
		chainID, addrs, addrs, chainID, addrs, addrs)
	if err != nil {
		return nil, nil, fmt.Errorf("reserve flows: %w", err)
	}
	type key struct{ wallet, exchange string }
	agg := map[key]*ReserveFlow{}
	topIn := map[key]decimal.Decimal{}
	for rows.Next() {
		var wallet, reserve string
		var inTx, backTx uint64
		var inUSD, backUSD decimal.Decimal
		if err := rows.Scan(&wallet, &reserve, &inTx, &inUSD, &backTx, &backUSD); err != nil {
			rows.Close()
			return nil, nil, err
		}
		l := exchangeOf[reserve]
		k := key{wallet, exchangeName(l)}
		f, ok := agg[k]
		if !ok {
			f = &ReserveFlow{Wallet: wallet, Exchange: k.exchange, InUSD: decimal.Zero, BackUSD: decimal.Zero}
			agg[k] = f
		}
		f.InTx += inTx
		f.BackTx += backTx
		f.InUSD = f.InUSD.Add(inUSD)
		f.BackUSD = f.BackUSD.Add(backUSD)
		if inUSD.GreaterThan(topIn[k]) || f.Reserve == "" {
			topIn[k], f.Reserve, f.ReserveSrc = inUSD, reserve, l.Source
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// Only wallets with flow both ways can pass; count counterparties for
	// those alone rather than for every address the reserves touched.
	flows := make([]ReserveFlow, 0, len(agg))
	var twoWay []string
	for _, f := range agg {
		flows = append(flows, *f)
		if f.InTx > 0 && f.BackTx > 0 {
			twoWay = append(twoWay, f.Wallet)
		}
	}
	sort.Slice(flows, func(i, j int) bool {
		if flows[i].Wallet != flows[j].Wallet {
			return flows[i].Wallet < flows[j].Wallet
		}
		return flows[i].Exchange < flows[j].Exchange
	})
	counterparties := map[string]int{}
	if len(twoWay) > 0 {
		crows, err := ch.QueryContext(ctx, `
			SELECT a, uniqExact(cp) FROM (
				SELECT from_address AS a, to_address AS cp FROM edges_current WHERE chain = ? AND from_address IN (?)
				UNION ALL
				SELECT to_address, from_address FROM edges_by_to_current WHERE chain = ? AND to_address IN (?))
			GROUP BY a`, chainID, twoWay, chainID, twoWay)
		if err != nil {
			return nil, nil, fmt.Errorf("counterparties: %w", err)
		}
		for crows.Next() {
			var a string
			var n uint64
			if err := crows.Scan(&a, &n); err != nil {
				crows.Close()
				return nil, nil, err
			}
			counterparties[a] = int(n)
		}
		crows.Close()
	}

	judged := JudgeHotWallets(flows, counterparties, rules)
	var out []Label
	for _, c := range judged {
		if !c.Accepted {
			continue
		}
		anchor := exchangeOf[c.Flow.Reserve]
		out = append(out, Label{
			Chain:      chainID,
			Address:    c.Wallet,
			Entity:     c.Exchange + " (hot wallet)",
			Category:   anchor.Category, // the reserve list's: identity, not a KYC tier (D19)
			Confidence: rules.Confidence,
			Source:     "derived:hotwallet",
			Evidence: map[string]any{
				"heuristic":       "reserve_two_way_flow",
				"exchange":        c.Exchange,
				"reserve_example": c.Flow.Reserve,
				"transfers_in":    c.Flow.InTx,
				"usd_in":          c.Flow.InUSD.StringFixed(2),
				"transfers_back":  c.Flow.BackTx,
				"usd_back":        c.Flow.BackUSD.StringFixed(2),
				"counterparties":  c.Counterparties,
				"exclusive_share": c.ExclusiveShare.StringFixed(4),
				"thresholds": map[string]any{
					"min_transfers_each_way": rules.MinTransfersEachWay,
					"min_inflow_usd":         rules.MinInflowUSD,
					"min_counterparties":     rules.MinCounterparties,
					"min_exclusive_share":    rules.MinExclusiveShare,
				},
			},
		})
	}
	return judged, out, nil
}
