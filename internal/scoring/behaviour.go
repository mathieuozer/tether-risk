package scoring

import (
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/shopspring/decimal"
)

// Flag is a behaviour note on an address's own activity (docs/DECISIONS.md
// D28). Flags are shown with the result and never enter the score: they say
// what the address did, not what it is.
type Flag struct {
	Code      string          // pass_through, new_address, high_volume_new
	InUSD     decimal.Decimal // pass_through
	OutUSD    decimal.Decimal // pass_through
	VolumeUSD decimal.Decimal // high_volume_new
	Days      int             // pass_through: span of activity
	AgeDays   int             // new_address, high_volume_new
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
