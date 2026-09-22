package scoring

import (
	"sort"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/shopspring/decimal"
)

// Flag is a behaviour note on an address's own activity (docs/DECISIONS.md
// D28). Flags are shown with the result and never enter the score: they say
// what the address did, not what it is.
type Flag struct {
	Code      string          // pass_through, new_address, high_volume_new, round_split, parked_funds, poisoning_target
	InUSD     decimal.Decimal // pass_through
	OutUSD    decimal.Decimal // pass_through
	VolumeUSD decimal.Decimal // high_volume_new
	Days      int             // pass_through: span of activity
	AgeDays   int             // new_address, high_volume_new
	Count     int             // round_split: recipients; parked_funds: wallets; poisoning_target: look-alikes
	AmountUSD decimal.Decimal // round_split: each amount; parked_funds: total held
	Minutes   int             // round_split: window
	Address   string          // poisoning_target: one address a look-alike imitated
}

// OutTransfer is one outbound edge as seen for splitting and parking.
type OutTransfer struct {
	To        string
	USD       decimal.Decimal
	Transfers uint64
	First     time.Time
	Last      time.Time
	// ToFetched: the recipient's own history is stored. ToSentUSD is what it
	// has sent to anyone since, known only when ToFetched.
	ToFetched bool
	ToSentUSD decimal.Decimal
}

// FlowFlags derives splitting and parking notes from an address's outbound
// transfers (docs/DECISIONS.md D30).
func FlowFlags(outs []OutTransfer, rules config.Behaviour) []Flag {
	var flags []Flag

	rs := rules.RoundSplit
	if rs.MinRecipients > 0 && rs.RoundToUSD > 0 {
		unit := decimal.NewFromFloat(rs.RoundToUSD)
		groups := map[string][]OutTransfer{}
		for _, o := range outs {
			if o.Transfers != 1 || o.USD.LessThan(decimal.NewFromFloat(rs.MinAmountUSD)) {
				continue
			}
			rem := o.USD.Mod(unit)
			if rem.GreaterThan(decimal.NewFromInt(1)) && unit.Sub(rem).GreaterThan(decimal.NewFromInt(1)) {
				continue
			}
			key := o.USD.Round(0).String()
			groups[key] = append(groups[key], o)
		}
		var best Flag
		for _, g := range groups {
			sort.Slice(g, func(i, j int) bool { return g[i].First.Before(g[j].First) })
			// The largest set of same-amount transfers inside the window.
			for i := range g {
				j := i
				for j+1 < len(g) && g[j+1].First.Sub(g[i].First) <= time.Duration(rs.MaxMinutes)*time.Minute {
					j++
				}
				n := j - i + 1
				if n >= rs.MinRecipients && n > best.Count {
					best = Flag{Code: "round_split", Count: n, AmountUSD: g[i].USD.Round(0),
						Minutes: max(1, int(g[j].First.Sub(g[i].First).Minutes()+0.5))}
				}
			}
		}
		if best.Count > 0 {
			flags = append(flags, best)
		}
	}

	pk := rules.Parked
	if pk.MinWallets > 0 {
		var n int
		held := decimal.Zero
		for _, o := range outs {
			if o.ToFetched && o.ToSentUSD.IsZero() && o.USD.GreaterThanOrEqual(decimal.NewFromFloat(pk.MinAmountUSD)) {
				n++
				held = held.Add(o.USD)
			}
		}
		if n >= pk.MinWallets && held.GreaterThanOrEqual(decimal.NewFromFloat(pk.MinTotalUSD)) {
			flags = append(flags, Flag{Code: "parked_funds", Count: n, AmountUSD: held.Round(0)})
		}
	}
	return flags
}

// BehaviourFlags derives notes from an address's activity as seen at now.
func BehaviourFlags(a *Activity, now time.Time, rules config.Behaviour) []Flag {
	if a == nil || a.FirstSeen.IsZero() {
		return nil
	}
	var out []Flag

	span := daysBetween(a.FirstSeen, a.LastSeen)
	pt := rules.PassThrough
	minVol := decimal.NewFromFloat(pt.MinVolumeUSD)
	if pt.MaxDays > 0 && a.InUSD.GreaterThanOrEqual(minVol) && a.OutUSD.GreaterThanOrEqual(minVol) && span <= pt.MaxDays {
		larger := decimal.Max(a.InUSD, a.OutUSD)
		retained := a.InUSD.Sub(a.OutUSD).Abs().Div(larger)
		if retained.LessThanOrEqual(decimal.NewFromFloat(pt.MaxRetainedShare)) {
			out = append(out, Flag{Code: "pass_through", InUSD: a.InUSD, OutUSD: a.OutUSD, Days: max(span, 1)})
		}
	}

	na := rules.NewAddress
	if age := daysBetween(a.FirstSeen, now); na.MaxAgeDays > 0 && age <= na.MaxAgeDays {
		volume := a.InUSD.Add(a.OutUSD)
		if na.HighVolumeUSD > 0 && volume.GreaterThanOrEqual(decimal.NewFromFloat(na.HighVolumeUSD)) {
			out = append(out, Flag{Code: "high_volume_new", VolumeUSD: volume, AgeDays: age})
		} else {
			out = append(out, Flag{Code: "new_address", AgeDays: age})
		}
	}
	return out
}

func daysBetween(a, b time.Time) int {
	if b.Before(a) {
		return 0
	}
	return int(b.Sub(a).Hours() / 24)
}
