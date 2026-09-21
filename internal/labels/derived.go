package labels

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/shopspring/decimal"
)

// Derived deposit-wallet detection.
//
// SPEC.md §6: "An address that repeatedly forwards funds to a known exchange
// hot wallet, with little other activity, is a deposit wallet of that
// exchange. This is what closes most of the gap against commercial feeds."
//
// The instruction is to implement it conservatively and record the reasoning
// in `evidence`. Conservative here means biased toward missing deposit wallets
// rather than inventing them: a wrong deposit label attributes a stranger's
// funds to an exchange they never used, and because the label terminates
// traversal (SPEC.md §7) nothing downstream can discover the error.

// DepositCandidate is an address that may be a deposit wallet, with the
// numbers behind the judgement.
type DepositCandidate struct {
	Address string

	// The known hot wallet receiving most of this address's outbound value.
	HotWallet       string
	HotWalletEntity string

	TransfersToHotWallet int
	TotalOutboundTx      int

	ValueToHotWallet decimal.Decimal
	TotalOutbound    decimal.Decimal
	ShareToHotWallet decimal.Decimal

	// OtherRecipients is how many distinct addresses other than the hot wallet
	// received value. A genuine deposit wallet forwards to one place.
	OtherRecipients int

	Accepted bool
	Rejected string // why, when not accepted
}

// DeriveDeposits finds deposit wallets feeding known hot wallets.
//
// Thresholds come from config (SPEC.md §6 requires they be configuration, not
// constants), and every judgement — including the rejections — is returned so
// the heuristic's precision can be measured rather than assumed.
func DeriveDeposits(ctx context.Context, ch *sql.DB, cfg *config.Config, snapshotLabels map[string]Label, chainID string) ([]DepositCandidate, []Label, error) {
	rules := cfg.Weights.DerivedDeposit
	if !rules.Enabled {
		return nil, nil, nil
	}

	// Hot wallets are the anchor: an address is a deposit wallet *of* a known
	// exchange, so with no known exchanges there is nothing to derive. Saying
	// so plainly beats returning an empty result that reads like "no deposit
	// wallets exist".
	hot := make([]string, 0, len(snapshotLabels))
	for addr, l := range snapshotLabels {
		if l.Category == "exchange" || l.Category == "high_risk_exchange" {
			hot = append(hot, addr)
		}
	}
	if len(hot) == 0 {
		return nil, nil, fmt.Errorf(
			"no exchange labels in this snapshot, so no deposit wallets can be derived; " +
				"populate config/curated_labels.yaml with exchange hot wallets first")
	}
	sort.Strings(hot) // determinism (docs/DECISIONS.md D6)

	// One pass over the edge table: for every address that sends to a known
	// hot wallet, gather its complete outbound profile. The profile has to
	// cover *all* outbound value, not just what went to the hot wallet —
	// "little or no outbound activity to anything else" is the discriminating
	// condition, and it cannot be evaluated from the hot-wallet edges alone.
	rows, err := ch.QueryContext(ctx, `
		WITH candidates AS (
			SELECT DISTINCT from_address
			FROM edges_current
			WHERE chain = ? AND to_address IN (?)
		)
		SELECT
			e.from_address,
			e.to_address,
			sum(e.total_usd_value) AS usd,
			sum(e.transfer_count)  AS transfers
		FROM edges_current e
		INNER JOIN candidates c ON e.from_address = c.from_address
		WHERE e.chain = ?
		GROUP BY e.from_address, e.to_address
		ORDER BY e.from_address, usd DESC, e.to_address`,
		chainID, hot, chainID)
	if err != nil {
		return nil, nil, fmt.Errorf("query deposit candidates: %w", err)
	}
	defer rows.Close()

	type outflow struct {
		to        string
		usd       decimal.Decimal
		transfers uint64
	}
	profiles := map[string][]outflow{}
	var order []string

	for rows.Next() {
		var from, to string
		var usd decimal.Decimal
		var transfers uint64
		if err := rows.Scan(&from, &to, &usd, &transfers); err != nil {
			return nil, nil, err
		}
		if _, seen := profiles[from]; !seen {
			order = append(order, from)
		}
		profiles[from] = append(profiles[from], outflow{to: to, usd: usd, transfers: transfers})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	sort.Strings(order)

	isHot := make(map[string]bool, len(hot))
	for _, h := range hot {
		isHot[h] = true
	}

	minShare := decimal.NewFromFloat(rules.MinOutboundShare)
	maxOther := decimal.NewFromFloat(rules.MaxOtherShare)

	var candidates []DepositCandidate
	var out []Label

	for _, addr := range order {
		flows := profiles[addr]

		// Pick the hot wallet receiving the most value from this address.
		var (
			best      outflow
			bestFound bool
			total     = decimal.Zero
			totalTx   int
			others    int
		)
		for _, f := range flows {
			total = total.Add(f.usd)
			totalTx += int(f.transfers)
			if isHot[f.to] {
				if !bestFound || f.usd.GreaterThan(best.usd) {
					best, bestFound = f, true
				}
			} else if f.usd.IsPositive() {
				others++
			}
		}
		if !bestFound {
			continue
		}

		c := DepositCandidate{
			Address:              addr,
			HotWallet:            best.to,
			TransfersToHotWallet: int(best.transfers),
			TotalOutboundTx:      totalTx,
			ValueToHotWallet:     best.usd,
			TotalOutbound:        total,
			OtherRecipients:      others,
		}
		if l, ok := snapshotLabels[best.to]; ok {
			c.HotWalletEntity = l.Entity
		}

		// An address whose outbound value is entirely unpriced cannot be
		// judged on value share. Rejecting is the conservative choice:
		// treating zero as "100% to the hot wallet" would label on no
		// evidence at all.
		if !total.IsPositive() {
			c.Rejected = "no priced outbound value to evaluate"
			candidates = append(candidates, c)
			continue
		}
		c.ShareToHotWallet = best.usd.Div(total)

		switch {
		case c.TransfersToHotWallet < rules.MinTransfers:
			c.Rejected = fmt.Sprintf("only %d transfers to the hot wallet, need %d",
				c.TransfersToHotWallet, rules.MinTransfers)

		case c.ShareToHotWallet.LessThan(minShare):
			c.Rejected = fmt.Sprintf("hot wallet received %s of outbound value, need %s",
				pct(c.ShareToHotWallet), pct(minShare))

		case decimal.NewFromInt(1).Sub(c.ShareToHotWallet).GreaterThan(maxOther):
			c.Rejected = fmt.Sprintf("%s of outbound value went elsewhere, limit is %s",
				pct(decimal.NewFromInt(1).Sub(c.ShareToHotWallet)), pct(maxOther))

		default:
			c.Accepted = true
		}

		candidates = append(candidates, c)
		if !c.Accepted {
			continue
		}

		entity := c.HotWalletEntity
		if entity == "" {
			entity = c.HotWallet
		}
		out = append(out, Label{
			Chain:   chainID,
			Address: addr,
			Entity:  entity + " (deposit wallet)",
			// A deposit wallet belongs to its exchange, so it inherits the
			// exchange category. That is the whole point: funds reaching it
			// have reached that exchange.
			Category:   categoryOf(snapshotLabels, c.HotWallet),
			Confidence: rules.Confidence,
			Source:     "derived:deposit",
			// SPEC.md §6 requires the reasoning be recorded. Everything needed
			// to recompute the judgement by hand is stored, so a reviewer can
			// check the label rather than trust it.
			Evidence: map[string]any{
				"heuristic":               "deposit_wallet",
				"hot_wallet":              c.HotWallet,
				"hot_wallet_entity":       c.HotWalletEntity,
				"transfers_to_hot_wallet": c.TransfersToHotWallet,
				"total_outbound_tx":       c.TotalOutboundTx,
				"value_to_hot_wallet":     c.ValueToHotWallet.String(),
				"total_outbound_value":    c.TotalOutbound.String(),
				"share_to_hot_wallet":     c.ShareToHotWallet.StringFixed(4),
				"other_recipients":        c.OtherRecipients,
				"thresholds": map[string]any{
					"min_transfers":      rules.MinTransfers,
					"min_outbound_share": rules.MinOutboundShare,
					"max_other_share":    rules.MaxOtherShare,
				},
			},
		})
	}

	return candidates, out, nil
}

func categoryOf(labels map[string]Label, address string) string {
	if l, ok := labels[address]; ok {
		return l.Category
	}
	return "exchange"
}

func pct(d decimal.Decimal) string {
	return d.Mul(decimal.NewFromInt(100)).StringFixed(1) + "%"
}

// Precision measures the heuristic against a hand-labelled sample.
//
// SPEC.md §6 requires this be measured and the result written into
// METHODOLOGY.md. An unmeasured heuristic is a guess with a confidence score
// attached, and the confidence is the part that makes it dangerous.
type Precision struct {
	TruePositives  int
	FalsePositives int
	FalseNegatives int
	Sample         int
	Unknown        int
}

// Value returns precision, and whether enough of the sample was labelled for
// the number to mean anything.
func (p Precision) Value() (float64, bool) {
	judged := p.TruePositives + p.FalsePositives
	if judged == 0 {
		return 0, false
	}
	return float64(p.TruePositives) / float64(judged), true
}

// Recall returns recall over the labelled portion of the sample.
func (p Precision) Recall() (float64, bool) {
	relevant := p.TruePositives + p.FalseNegatives
	if relevant == 0 {
		return 0, false
	}
	return float64(p.TruePositives) / float64(relevant), true
}

// MeasurePrecision compares the heuristic's verdicts against ground truth.
// `truth` maps address to whether it really is a deposit wallet; addresses
// absent from it are counted as Unknown and excluded from the arithmetic
// rather than assumed negative.
func MeasurePrecision(candidates []DepositCandidate, truth map[string]bool) Precision {
	var p Precision
	p.Sample = len(candidates)

	for _, c := range candidates {
		actual, known := truth[c.Address]
		if !known {
			p.Unknown++
			continue
		}
		switch {
		case c.Accepted && actual:
			p.TruePositives++
		case c.Accepted && !actual:
			p.FalsePositives++
		case !c.Accepted && actual:
			p.FalseNegatives++
		}
	}
	return p
}
