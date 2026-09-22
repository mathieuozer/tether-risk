package screen

import (
	"context"
	"fmt"
	"sort"

	"github.com/mozer/tether-risk/internal/scoring"
)

// Checking a payment before it is sent (docs/DECISIONS.md D34).
//
// The question a sender has is not "what is this address" but "may I pay
// it". Besides screening the recipient, two things only the sender's own
// history can answer: whether the recipient is a look-alike of someone the
// sender really pays, which catches a poisoning our labels have not yet
// seen, and whether the sender has ever paid the recipient before.

// PreSend is the answer to "may From pay To".
type PreSend struct {
	// Recipient is the ordinary screen of To.
	Recipient *scoring.Result

	// LookalikeOf is a real counterparty of From that To imitates, empty when
	// none does. It is the finding that outranks everything else.
	LookalikeOf string
	// LookalikeUSD is what From has exchanged with that counterparty.
	LookalikeUSD float64

	// PaidBefore is what From has sent To before; FirstPayment is true when
	// From has never paid it.
	PaidBefore   float64
	FirstPayment bool

	// SenderKnown is false when From's history could not be read, and the
	// look-alike and first-payment checks did not run.
	SenderKnown bool
}

// lookalikeKey is what a poisoner copies: the first and last four
// characters, the parts wallets show (docs/DECISIONS.md D32).
func lookalikeKey(a string) string {
	if len(a) < 8 {
		return a
	}
	return a[:4] + a[len(a)-4:]
}

// PreSend screens to, and, when from is given, checks it against from's own
// history.
func (s *Service) PreSend(ctx context.Context, chainID, from, to string) (*PreSend, error) {
	if from != "" && from == to {
		return nil, fmt.Errorf("sender and recipient are the same address")
	}
	rec, err := s.Screen(ctx, chainID, to)
	if err != nil {
		return nil, err
	}
	out := &PreSend{Recipient: rec}
	if from == "" {
		return out, nil
	}

	if p, ok := s.prefetch[chainID]; ok {
		fctx, cancel := context.WithTimeout(ctx, fetchBudget)
		_, _ = p.FetchAddress(fctx, from, 0)
		cancel()
	}

	// Every real counterparty of from, both directions. $100 is the bar the
	// poisoning labeller uses for "real" (D32): dust is what poisoners send.
	rows, err := s.ch.QueryContext(ctx, `
		SELECT cp, sum(v) FROM (
			SELECT to_address AS cp, toFloat64(total_usd_value) AS v FROM edges_current
			WHERE chain = ? AND from_address = ?
			UNION ALL
			SELECT from_address AS cp, toFloat64(total_usd_value) AS v FROM edges_by_to_current
			WHERE chain = ? AND to_address = ?
		) GROUP BY cp`, chainID, from, chainID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type cp struct {
		addr string
		usd  float64
	}
	var real []cp
	for rows.Next() {
		var c cp
		if err := rows.Scan(&c.addr, &c.usd); err != nil {
			return nil, err
		}
		out.SenderKnown = true
		if c.usd >= 100 && c.addr != to {
			real = append(real, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !out.SenderKnown {
		return out, nil
	}

	key := lookalikeKey(to)
	sort.Slice(real, func(i, j int) bool {
		if real[i].usd != real[j].usd {
			return real[i].usd > real[j].usd
		}
		return real[i].addr < real[j].addr
	})
	for _, c := range real {
		if lookalikeKey(c.addr) == key {
			out.LookalikeOf, out.LookalikeUSD = c.addr, c.usd
			break
		}
	}

	if err := s.ch.QueryRowContext(ctx, `
		SELECT toFloat64(sum(total_usd_value)) FROM edges_current
		WHERE chain = ? AND from_address = ? AND to_address = ?`, chainID, from, to).Scan(&out.PaidBefore); err != nil {
		return nil, err
	}
	out.FirstPayment = out.PaidBefore < 1
	return out, nil
}
