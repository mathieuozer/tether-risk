package screen

import (
	"context"
	"sort"

	"github.com/mozer/tether-risk/internal/scoring"
)

// poisoningTarget reports whether address-poisoning senders have dusted the
// address (docs/DECISIONS.md D32). It is a warning to the holder, not a
// finding against them: a victim did nothing, so it changes neither the
// verdict nor the confidence. The look-alikes sit in the address's history,
// and the note says not to copy addresses from there.
func (s *Service) poisoningTarget(ctx context.Context, chainID, address string, snapshotID int64) (scoring.Flag, bool) {
	rows, err := s.ch.QueryContext(ctx, `
		SELECT from_address FROM edges_by_to_current
		WHERE chain = ? AND to_address = ? AND total_usd_value < 1
		GROUP BY from_address LIMIT 2000`, chainID, address)
	if err != nil {
		return scoring.Flag{}, false
	}
	var senders []string
	for rows.Next() {
		var a string
		if rows.Scan(&a) == nil {
			senders = append(senders, a)
		}
	}
	rows.Close()
	if len(senders) == 0 {
		return scoring.Flag{}, false
	}
	byAddr, err := s.store.ForAddresses(ctx, snapshotID, chainID, senders)
	if err != nil {
		return scoring.Flag{}, false
	}
	imitated := map[string]int{}
	var n int
	for _, ls := range byAddr {
		for _, l := range ls {
			if l.Source == "derived:poisoning" {
				n++
				if im, ok := l.Evidence["imitates"].(string); ok {
					imitated[im]++
				}
				break
			}
		}
	}
	if n == 0 {
		return scoring.Flag{}, false
	}
	// The address imitated most often is the one the holder most likely
	// pays, so it is the example the note gives.
	ims := make([]string, 0, len(imitated))
	for a := range imitated {
		ims = append(ims, a)
	}
	sort.Slice(ims, func(i, j int) bool {
		if imitated[ims[i]] != imitated[ims[j]] {
			return imitated[ims[i]] > imitated[ims[j]]
		}
		return ims[i] < ims[j]
	})
	f := scoring.Flag{Code: "poisoning_target", Count: n}
	if len(ims) > 0 {
		f.Address = ims[0]
	}
	return f, true
}
