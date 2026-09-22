package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/ingest"
	"github.com/mozer/tether-risk/internal/screen"
	"github.com/mozer/tether-risk/internal/store"
)

// checkRing measures what the first ring buys (docs/DECISIONS.md D31): each
// address is screened without it, then with it, and coverage, confidence and
// time are compared. The addresses are fetched, unlabelled wallets, so the
// difference is the ring's alone.
func checkRing(ctx context.Context, cfg *config.Config, ch, pg *sql.DB, chainID string, limit int) (checkResult, error) {
	res := checkResult{Name: "first ring"}
	p, err := ingest.NewPrefetcher(chainID, cfg, ch, pg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return res, err
	}
	// A measurement is not a customer: its follow-ups queue in the background.
	bg := &backgroundPrefetch{Worker: p, jobs: store.NewJobs(pg), chainID: chainID, queued: map[string]bool{}}
	without := screen.NewService(ch, pg, cfg).WithPrefetch(chainID, bg).WithRingBudget(0)
	with := screen.NewService(ch, pg, cfg).WithPrefetch(chainID, bg)

	addrs, err := pgAddrs(ctx, pg, `
		SELECT f.address FROM address_freshness f WHERE f.chain = $1
		  AND NOT EXISTS (SELECT 1 FROM labels l WHERE l.chain = f.chain AND l.address = f.address AND l.valid_to_snapshot IS NULL)
		ORDER BY md5(f.address || 'ring2') LIMIT $2`, chainID, limit)
	if err != nil {
		return res, err
	}

	fmt.Printf("\n%-36s %9s %9s %7s %7s %6s %7s %7s\n", "address", "cov off", "cov on", "conf off", "conf on", "ring", "s off", "s on")
	var sumOff, sumOn, sumConfOff, sumConfOn, secs, secsOff float64
	var n, improved int
	for _, a := range addrs {
		t0 := time.Now()
		off, err := without.Screen(ctx, chainID, a)
		if err != nil {
			res.Failed++
			continue
		}
		d0 := time.Since(t0).Seconds()
		t := time.Now()
		on, err := with.Screen(ctx, chainID, a)
		if err != nil {
			res.Failed++
			continue
		}
		d := time.Since(t).Seconds()
		co, cn := off.Coverage.InexactFloat64()*100, on.Coverage.InexactFloat64()*100
		fo, fn := 0, 0
		if off.Verdict != nil {
			fo = off.Verdict.ConfidencePct
		}
		if on.Verdict != nil {
			fn = on.Verdict.ConfidencePct
		}
		ring := 0
		if on.Depth != nil {
			ring = on.Depth.RingFetched
		}
		fmt.Printf("%-36s %8.1f%% %8.1f%% %6d%% %6d%% %6d %7.1f %7.1f\n", a, co, cn, fo, fn, ring, d0, d)
		sumOff, sumOn, sumConfOff, sumConfOn, secs, secsOff = sumOff+co, sumOn+cn, sumConfOff+float64(fo), sumConfOn+float64(fn), secs+d, secsOff+d0
		n++
		if cn > co+0.5 {
			improved++
		}
		res.Passed++
	}
	if n > 0 {
		fmt.Printf("\nmean coverage %.1f%% -> %.1f%%, mean confidence %.1f%% -> %.1f%%, improved %d of %d, mean screen %.1fs -> %.1fs\n",
			sumOff/float64(n), sumOn/float64(n), sumConfOff/float64(n), sumConfOn/float64(n), improved, n, secsOff/float64(n), secs/float64(n))
	}
	return res, nil
}
