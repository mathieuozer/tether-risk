package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/labels"
)

// derivePoisoning labels address-poisoning senders from stored edges
// (docs/DECISIONS.md D32). The rule is rerun in full, so a label it no
// longer produces is withdrawn. It returns the new snapshot, or 0.
func derivePoisoning(ctx context.Context, cfg *config.Config, ch *sql.DB, st *labels.Store,
	snapshotID int64, chainID string) (int64, error) {

	rules := cfg.Weights.DerivedPoisoning
	started := time.Now()
	// Dust edges whose sender starts and ends like a real counterparty of
	// the recipient; plus what the recipient later paid the sender.
	// Joined on the recipient and the look-alike key together, so only
	// matching pairs are ever built: filtering after a join on the recipient
	// alone ran past the connection's read timeout.
	rows, err := ch.QueryContext(ctx, `
		WITH ? AS pre, ? AS suf
		SELECT d.s, d.v, c.cp, d.fs, c.fs, ifNull(p.usd, 0)
		FROM (SELECT from_address AS s, to_address AS v, min(first_seen) AS fs,
		             concat(substring(from_address, 1, pre), substring(from_address, -suf)) AS k
		      FROM edges_current WHERE chain = ?
		      GROUP BY s, v HAVING sum(total_usd_value) < ?) AS d
		INNER JOIN (
			SELECT v, cp, min(fs) AS fs, any(k) AS k FROM (
				SELECT from_address AS v, to_address AS cp, first_seen AS fs,
				       concat(substring(to_address, 1, pre), substring(to_address, -suf)) AS k
				FROM edges_current WHERE chain = ? AND total_usd_value >= ?
				UNION ALL
				SELECT to_address, from_address, first_seen,
				       concat(substring(from_address, 1, pre), substring(from_address, -suf))
				FROM edges_by_to_current WHERE chain = ? AND total_usd_value >= ?)
			GROUP BY v, cp) AS c ON c.v = d.v AND c.k = d.k
		LEFT JOIN (
			SELECT from_address AS v, to_address AS s, sum(total_usd_value) AS usd FROM edges_current
			WHERE chain = ? AND total_usd_value >= 1 GROUP BY v, s) AS p ON p.v = d.v AND p.s = d.s
		WHERE c.cp != d.s
		SETTINGS max_memory_usage = 8000000000, join_use_nulls = 1`,
		rules.MatchPrefix, rules.MatchSuffix, chainID, rules.MaxDustUSD,
		chainID, rules.MinRealUSD, chainID, rules.MinRealUSD, chainID)
	if err != nil {
		return 0, fmt.Errorf("poisoning pairs: %w", err)
	}
	var pairs []labels.PoisoningPair
	for rows.Next() {
		var p labels.PoisoningPair
		if err := rows.Scan(&p.Sender, &p.Victim, &p.Imitated, &p.DustFirst, &p.RealFirst, &p.StolenUSD); err != nil {
			rows.Close()
			return 0, err
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	src, _ := cfg.Source("derived:poisoning")
	derived := labels.JudgePoisoning(pairs, src.Confidence, chainID)
	var stolen float64
	var succeeded int
	for _, p := range pairs {
		stolen += p.StolenUSD
		if p.StolenUSD > 0 {
			succeeded++
		}
	}
	fmt.Printf("poisoning:           %d senders labelled from %d look-alike pairs (%s); %d pairs where the victim then paid, $%.0f\n",
		len(derived), len(pairs), time.Since(started).Round(time.Second), succeeded, stolen)

	previous, err := st.BySources(ctx, snapshotID, chainID, []string{"derived:poisoning"})
	if err != nil {
		return 0, err
	}
	keep := map[string]bool{}
	for _, l := range derived {
		keep[l.Address] = true
	}
	var stale []string
	for _, l := range previous {
		if !keep[l.Address] {
			stale = append(stale, l.Address)
		}
	}
	if len(derived) == 0 && len(stale) == 0 {
		return 0, nil
	}
	snap, err := st.OpenSnapshot(ctx, "derived poisoning")
	if err != nil {
		return 0, err
	}
	if len(stale) > 0 {
		n, err := st.RetireAddresses(ctx, snap, "derived:poisoning", chainID, stale)
		if err != nil {
			return 0, err
		}
		fmt.Printf("poisoning withdrawn: %d\n", n)
	}
	if _, err := st.Upsert(ctx, snap, derived); err != nil {
		return 0, err
	}
	if _, err := st.SealSnapshot(ctx, snap); err != nil {
		return 0, err
	}
	return snap, nil
}
