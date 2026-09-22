package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/ingest"
	"github.com/mozer/tether-risk/internal/pricing"
	"github.com/mozer/tether-risk/internal/store"
)

// checkFreeze measures how often Tether freezes the counterparties of a wallet
// it has just frozen (docs/DECISIONS.md D34).
//
// For a sample of frozen wallets F, F's history is fetched and every
// counterparty C with at least $1 of flow in the 30 days before F's freeze is
// taken. The outcome is C's own freeze time, which the on-chain blacklist gives
// for every address, so only F's history is needed. A counterparty already
// frozen at F's freeze has no lead time and is left out. The control is the
// same measure around ordinary fetched wallets, each paired with the freeze
// time of one sampled F.
func checkFreeze(ctx context.Context, cfg *config.Config, ch, pg *sql.DB, chainID string, limit int) (checkResult, error) {
	res := checkResult{Name: "freeze contagion"}

	frozenAt := map[string]time.Time{}
	rows, err := pg.QueryContext(ctx, `
		SELECT address, (evidence->>'added_at')::timestamptz FROM labels
		WHERE chain = $1 AND source = 'tether_blacklist' AND valid_to_snapshot IS NULL`, chainID)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var a string
		var t time.Time
		if err := rows.Scan(&a, &t); err != nil {
			rows.Close()
			return res, err
		}
		frozenAt[a] = t.UTC()
	}
	rows.Close()

	services := map[string]bool{}
	rows, err = pg.QueryContext(ctx, `
		SELECT DISTINCT address FROM labels WHERE chain = $1 AND valid_to_snapshot IS NULL
		  AND category IN ('exchange','named_service','unnamed_service','dex','high_risk_exchange')`, chainID)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return res, err
		}
		services[a] = true
	}
	rows.Close()

	// Frozen from 2025 until 90 days ago, so every outcome window has closed.
	cutoff := time.Now().UTC().AddDate(0, 0, -90)
	sample, err := pgAddrs(ctx, pg, `
		SELECT address FROM labels
		WHERE chain = $1 AND source = 'tether_blacklist' AND valid_to_snapshot IS NULL
		  AND (evidence->>'added_at')::timestamptz >= '2025-01-01'
		  AND (evidence->>'added_at')::timestamptz < now() - interval '90 days'
		ORDER BY md5(address || 'freeze') LIMIT $2`, chainID, limit)
	if err != nil {
		return res, err
	}
	fmt.Printf("frozen sample: %d wallets frozen 2025-01-01 .. %s\n", len(sample), cutoff.Format("2006-01-02"))

	fetchFailed, err := fetchFreezeWindows(ctx, cfg, ch, pg, chainID, sample, frozenAt)
	if err != nil {
		return res, err
	}
	if fetchFailed > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("%d frozen wallets could not be fetched and are left out", fetchFailed))
	}

	var treated []anchor
	for _, a := range sample {
		treated = append(treated, anchor{addr: a, at: frozenAt[a]})
	}
	control, err := pgAddrs(ctx, pg, `
		SELECT f.address FROM address_freshness f
		WHERE f.chain = $1 AND NOT f.truncated
		  AND NOT EXISTS (SELECT 1 FROM labels l WHERE l.chain = f.chain AND l.address = f.address AND l.valid_to_snapshot IS NULL)
		ORDER BY md5(f.address || 'freeze-control') LIMIT $2`, chainID, limit)
	if err != nil {
		return res, err
	}
	var controls []anchor
	for i, a := range control {
		if len(treated) == 0 {
			break
		}
		controls = append(controls, anchor{addr: a, at: treated[i%len(treated)].at})
	}

	t, err := contagion(ctx, ch, chainID, treated, frozenAt, services)
	if err != nil {
		return res, err
	}
	c, err := contagion(ctx, ch, chainID, controls, frozenAt, services)
	if err != nil {
		return res, err
	}
	fmt.Printf("\nCounterparties of wallets Tether froze (flow in the 30 days before the freeze):\n")
	t.print()
	fmt.Printf("\nControl: counterparties of ordinary screened wallets, same windows:\n")
	c.print()
	res.Passed = t.anchors
	return res, nil
}

type anchor struct {
	addr string
	at   time.Time
}

// leadCount keeps, per counterparty, the days from the anchor to Tether's
// freeze of it, or -1 when Tether has not frozen it.
type leadCount struct {
	leads []float64
}

func (s *leadCount) add(lead time.Duration) {
	if lead < 0 {
		s.leads = append(s.leads, -1)
		return
	}
	s.leads = append(s.leads, lead.Hours()/24)
}

// within counts counterparties frozen within days of the anchor.
func (s *leadCount) within(days float64) int {
	n := 0
	for _, l := range s.leads {
		if l >= 0 && l <= days {
			n++
		}
	}
	return n
}

// next30 is the share frozen in (d, d+30] days among those still unfrozen d
// days after the anchor: what a screen d days later should say.
func (s *leadCount) next30(d float64) (int, int) {
	var at, hit int
	for _, l := range s.leads {
		if l >= 0 && l <= d {
			continue
		}
		at++
		if l > d && l <= d+30 {
			hit++
		}
	}
	return hit, at
}

type contagionResult struct {
	anchors, withFlow, alreadyFrozen, servicesSkipped int
	strata                                            map[string]*leadCount
}

func (r *contagionResult) s(k string) *leadCount {
	if r.strata[k] == nil {
		r.strata[k] = &leadCount{}
	}
	return r.strata[k]
}

func (r contagionResult) print() {
	fmt.Printf("anchors %d, with flow %d, counterparties already frozen %d, services skipped %d\n",
		r.anchors, r.withFlow, r.alreadyFrozen, r.servicesSkipped)
	keys := make([]string, 0, len(r.strata))
	for k := range r.strata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("%-34s %7s %8s %8s %8s %8s\n", "stratum", "n", "7d", "30d", "90d", "365d")
	pct := func(a, n int) string { return fmt.Sprintf("%.2f%%", 100*float64(a)/float64(max(n, 1))) }
	for _, k := range keys {
		s := r.strata[k]
		n := len(s.leads)
		fmt.Printf("%-34s %7d %8s %8s %8s %8s\n", k, n, pct(s.within(7), n), pct(s.within(30), n), pct(s.within(90), n), pct(s.within(365), n))
	}
	fmt.Printf("\nnext 30 days, given still unfrozen d days after the anchor:\n%-34s", "stratum")
	ds := []float64{0, 3, 7, 14, 30, 60, 90, 180}
	for _, d := range ds {
		fmt.Printf(" %8s", fmt.Sprintf("d=%.0f", d))
	}
	fmt.Println()
	for _, k := range keys {
		fmt.Printf("%-34s", k)
		for _, d := range ds {
			hit, at := r.strata[k].next30(d)
			fmt.Printf(" %8s", pct(hit, at))
		}
		fmt.Println()
	}
}

// contagion takes, for each anchor, the counterparties with at least $1 of
// flow in the 30 days before the anchor time, and counts when Tether froze
// them.
func contagion(ctx context.Context, ch *sql.DB, chainID string, anchors []anchor,
	frozenAt map[string]time.Time, services map[string]bool) (contagionResult, error) {
	r := contagionResult{strata: map[string]*leadCount{}}
	for _, a := range anchors {
		if a.at.IsZero() {
			continue
		}
		r.anchors++
		rows, err := ch.QueryContext(ctx, `
			SELECT cp,
			       sumIf(v, dir = 'in')  AS paid_in,
			       sumIf(v, dir = 'out') AS paid_out,
			       max(block_time)       AS last
			FROM (
				SELECT to_address AS cp, 'out' AS dir, toFloat64(ifNull(usd_value, 0)) AS v, block_time
				FROM transfers WHERE chain = ? AND from_address = ?
				  AND block_time >= ? AND block_time < ?
				UNION ALL
				SELECT from_address AS cp, 'in' AS dir, toFloat64(ifNull(usd_value, 0)) AS v, block_time
				FROM transfers_by_to WHERE chain = ? AND to_address = ?
				  AND block_time >= ? AND block_time < ?
			)
			WHERE v >= 1 AND cp != ?
			GROUP BY cp`,
			chainID, a.addr, a.at.Add(-30*24*time.Hour), a.at,
			chainID, a.addr, a.at.Add(-30*24*time.Hour), a.at, a.addr)
		if err != nil {
			return r, err
		}
		any := false
		for rows.Next() {
			var cp string
			var in, out float64
			var last time.Time
			if err := rows.Scan(&cp, &in, &out, &last); err != nil {
				rows.Close()
				return r, err
			}
			any = true
			if services[cp] {
				r.servicesSkipped++
				continue
			}
			lead := time.Duration(-1)
			if t, ok := frozenAt[cp]; ok {
				if !t.After(a.at) {
					r.alreadyFrozen++
					continue
				}
				lead = t.Sub(a.at)
			}
			r.s("all").add(lead)
			// "received" means the counterparty got money from the anchor.
			switch {
			case out > 0 && in > 0:
				r.s("dir: both").add(lead)
			case out > 0:
				r.s("dir: received from it").add(lead)
			default:
				r.s("dir: paid it").add(lead)
			}
			v := in + out
			switch {
			case v >= 10000:
				r.s("value: >= $10k").add(lead)
			case v >= 1000:
				r.s("value: $1k-10k").add(lead)
			default:
				r.s("value: < $1k").add(lead)
			}
			gap := a.at.Sub(last.UTC())
			switch {
			case gap <= 24*time.Hour:
				r.s("last flow: < 1 day before").add(lead)
			case gap <= 7*24*time.Hour:
				r.s("last flow: 1-7 days before").add(lead)
			default:
				r.s("last flow: 7-30 days before").add(lead)
			}
			if out > 0 && gap <= 7*24*time.Hour && v >= 1000 {
				r.s("combo: received >=$1k, <7d").add(lead)
			}
		}
		rows.Close()
		if any {
			r.withFlow++
		}
	}
	return r, nil
}

// fetchFreezeWindows stores each sampled wallet's USDT transfers in the 30
// days before its freeze, unless its history is already stored. Reading only
// the window takes a page or two where the whole history of a busy wallet
// takes fifty (docs/DECISIONS.md D34).
func fetchFreezeWindows(ctx context.Context, cfg *config.Config, ch, pg *sql.DB, chainID string,
	sample []string, frozenAt map[string]time.Time) (int, error) {
	chainCfg, _ := cfg.Chain(chainID)
	ad, err := ingest.NewAdapter(chainID, chainCfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return 0, err
	}
	tr, ok := ad.(*tron.Adapter)
	if !ok {
		return 0, fmt.Errorf("freeze windows need the TRON adapter")
	}
	writer := store.NewTransferWriter(ch, pg)
	pricer := pricing.New(cfg, pg)

	var failed int
	var mu sync.Mutex
	work := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range work {
				var known bool
				_ = pg.QueryRowContext(ctx, `SELECT true FROM address_freshness WHERE chain = $1 AND address = $2`, chainID, a).Scan(&known)
				if known {
					continue
				}
				if err := fetchWindow(ctx, tr, writer, pricer, a, frozenAt[a]); err != nil {
					mu.Lock()
					failed++
					mu.Unlock()
				}
			}
		}()
	}
	for i, a := range sample {
		if ctx.Err() != nil {
			break
		}
		if i%100 == 0 {
			fmt.Printf("  fetching %d/%d\n", i, len(sample))
		}
		work <- a
	}
	close(work)
	wg.Wait()
	return failed, ctx.Err()
}

func fetchWindow(ctx context.Context, tr *tron.Adapter, w *store.TransferWriter, pricer *pricing.Pricer, addr string, at time.Time) error {
	cur := tr.USDTWindowCursor(addr, at.Add(-30*24*time.Hour), at)
	for page := 0; page < 50; page++ {
		p, err := tr.FetchAddress(ctx, addr, cur)
		if err != nil {
			return err
		}
		for i := range p.Transfers {
			t := &p.Transfers[i]
			v, basis, err := pricer.Price(ctx, t.Asset, t.RawValue, t.BlockTime)
			if err != nil {
				return err
			}
			t.USDValue, t.PriceBasis = v, basis
		}
		if _, err := w.WritePage(ctx, addr, p.PageKey, p.Transfers); err != nil {
			return err
		}
		if p.Next.Done {
			return nil
		}
		cur = p.Next
	}
	return nil
}
