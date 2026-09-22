package labels

import (
	"fmt"
	"sort"
	"time"
)

// Address poisoning (docs/DECISIONS.md D32).
//
// A poisoner watches for a real transfer between a victim and a
// counterparty, generates an address that starts and ends like the
// counterparty's, and sends the victim a worthless transfer from it. The
// look-alike now sits in the victim's history, and a victim who copies the
// "usual" address from there pays the poisoner.
//
// The signature is exact enough to label on. A dust transfer from S to V,
// where V has a real counterparty C with S's first four and last four
// characters, is a coincidence with odds of about one in two trillion. Of
// 31,007 such pairs in stored data, 30,937 (99.8%) arrived after V's first
// real transfer with C, 11,106 within the hour: poisoners react to what
// they see. The sender is labelled scam.

// PoisoningPair is one observed poisoning: Sender sent Victim dust while
// imitating Imitated, a real counterparty of Victim.
type PoisoningPair struct {
	Sender, Victim, Imitated string
	DustFirst                time.Time // first dust transfer from Sender to Victim
	RealFirst                time.Time // first real transfer between Victim and Imitated
	// StolenUSD is what Victim sent Sender in transfers of real value: the
	// poisoning worked.
	StolenUSD float64
}

// JudgePoisoning labels senders with at least one pair whose dust came
// after the real transfer it imitates. Pairs the other way round are not
// reactive, and are the only place a coincidence of vanity addresses could
// hide.
func JudgePoisoning(pairs []PoisoningPair, confidence float64, chainID string) []Label {
	type agg struct {
		victims  map[string]bool
		imitated map[string]int
		stolen   float64
		example  PoisoningPair
		reactive bool
	}
	by := map[string]*agg{}
	for _, p := range pairs {
		if p.Sender == "" || p.Sender == p.Imitated || p.Sender == p.Victim {
			continue
		}
		a := by[p.Sender]
		if a == nil {
			a = &agg{victims: map[string]bool{}, imitated: map[string]int{}}
			by[p.Sender] = a
		}
		if p.DustFirst.Before(p.RealFirst) {
			continue
		}
		if !a.reactive || p.DustFirst.Before(a.example.DustFirst) {
			a.example = p
		}
		a.reactive = true
		a.victims[p.Victim] = true
		a.imitated[p.Imitated]++
		a.stolen += p.StolenUSD
	}

	senders := make([]string, 0, len(by))
	for s, a := range by {
		if a.reactive {
			senders = append(senders, s)
		}
	}
	sort.Strings(senders)
	out := make([]Label, 0, len(senders))
	for _, s := range senders {
		a := by[s]
		// The address it imitates most is the one its name should give.
		top, n := "", 0
		for im, k := range a.imitated {
			if k > n || (k == n && im < top) {
				top, n = im, k
			}
		}
		ex := a.example
		out = append(out, Label{
			Chain: chainID, Address: s,
			Entity:     fmt.Sprintf("Address poisoning (imitates %s)", short(top)),
			Category:   "scam",
			Confidence: confidence,
			Source:     "derived:poisoning",
			Evidence: map[string]any{
				"heuristic":        "lookalike_dust",
				"imitates":         top,
				"victims":          len(a.victims),
				"imitated_count":   len(a.imitated),
				"stolen_usd":       fmt.Sprintf("%.2f", a.stolen),
				"example_victim":   ex.Victim,
				"example_imitated": ex.Imitated,
				"real_first":       ex.RealFirst.UTC().Format(time.RFC3339),
				"dust_first":       ex.DustFirst.UTC().Format(time.RFC3339),
				"minutes_after":    int(ex.DustFirst.Sub(ex.RealFirst).Minutes()),
			},
		})
	}
	return out
}

// short is an address as reports print it, enough to recognise the
// imitation: the same four characters each end.
func short(a string) string {
	if len(a) < 12 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
}
