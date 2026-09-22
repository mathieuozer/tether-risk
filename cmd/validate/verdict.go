package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/ingest"
	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/mozer/tether-risk/internal/screen"
	"github.com/mozer/tether-risk/internal/store"
)

// Verdict benchmark (docs/DECISIONS.md D29).
//
// Three sets whose right answer is known without trusting the verdict:
//
//   - listed: the address itself is frozen by Tether or on OFAC's list.
//     Every one must be high risk.
//   - exposed: unlabelled addresses that sent to or received from a listed
//     address directly. None may be called clean. A clean verdict here is
//     the error that costs a customer most.
//   - deposit: exchange deposit wallets named by the reserve heuristics.
//     None should be called high risk; that is the false alarm rate.
//
// The sets are drawn from stored data, so the benchmark spends no API
// budget. Results are written to .data/benchmark so runs can be compared.

// followRounds > 0 measures the final answer: each round screens with
// prefetching, so dead ends are queued, waits for the worker, and screens
// again, as the bot's follow-up does (docs/DECISIONS.md D28, D30).
var followRounds int

type benchSet struct {
	name, want string
	addrs      []string
}

func checkVerdictBenchmark(ctx context.Context, svc *screen.Service, cfg *config.Config, ch, pg *sql.DB, chainID string, limit int) (checkResult, error) {
	res := checkResult{Name: "verdict benchmark"}

	listed, err := pgAddrs(ctx, pg, `
		SELECT address FROM labels WHERE chain = $1 AND valid_to_snapshot IS NULL
		  AND source IN ('tether_blacklist', 'ofac')
		  AND address IN (SELECT address FROM address_freshness WHERE chain = $1)
		ORDER BY address LIMIT $2`, chainID, limit)
	if err != nil {
		return res, err
	}
	allListed, err := pgAddrs(ctx, pg, `
		SELECT address FROM labels WHERE chain = $1 AND valid_to_snapshot IS NULL
		  AND source IN ('tether_blacklist', 'ofac') LIMIT $2`, chainID, 1<<30)
	if err != nil {
		return res, err
	}
	exposed, err := exposedAddrs(ctx, ch, pg, chainID, allListed, limit)
	if err != nil {
		return res, err
	}
	deposit, err := pgAddrs(ctx, pg, `
		SELECT address FROM labels WHERE chain = $1 AND valid_to_snapshot IS NULL AND source = 'derived:deposit'
		  AND address IN (SELECT address FROM address_freshness WHERE chain = $1)
		ORDER BY address LIMIT $2`, chainID, limit)
	if err != nil {
		return res, err
	}

	sets := []benchSet{
		{"listed", "high_risk", listed},
		{"exposed", "not clear", exposed},
		{"deposit", "not high_risk", deposit},
	}

	dir := filepath.Join(".data", "benchmark")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "verdict-"+time.Now().UTC().Format("20060102-1504")+".csv")
	f, err := os.Create(path)
	if err != nil {
		return res, err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{"set", "address", "level", "confidence", "band", "score", "coverage", "top_reason", "ok"})

	if followRounds > 0 {
		p, err := ingest.NewPrefetcher(chainID, cfg, ch, pg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			return res, err
		}
		bg := &backgroundPrefetch{Worker: p, jobs: store.NewJobs(pg), chainID: chainID, queued: map[string]bool{}}
		svc.WithPrefetch(chainID, bg)
		for r := 1; r <= followRounds; r++ {
			for _, s := range sets {
				for _, a := range s.addrs {
					if _, err := svc.Screen(ctx, chainID, a); err != nil {
						fmt.Fprintf(os.Stderr, "round %d %s: %v\n", r, a, err)
					}
				}
			}
			bg.wait(ctx, pg, 20*time.Minute)
			fmt.Printf("follow-up round %d of %d done\n", r, followRounds)
		}
	}

	fmt.Println("\nVERDICT BENCHMARK")
	fmt.Printf("%-10s %5s %7s %8s %10s   %-16s %s\n", "set", "n", "clear", "caution", "high_risk", "must be", "misses")
	for _, s := range sets {
		counts := map[string]int{}
		misses := 0
		var covSum float64
		for _, a := range s.addrs {
			out, err := svc.Screen(ctx, chainID, a)
			if err != nil {
				res.Failures = append(res.Failures, fmt.Sprintf("%s %s: %v", s.name, a, err))
				res.Failed++
				continue
			}
			v := out.Verdict
			counts[v.Level]++
			covSum += out.Coverage.InexactFloat64()
			ok := verdictOK(s.want, v)
			if !ok {
				misses++
				res.Failures = append(res.Failures, fmt.Sprintf("%s: %s is %s (%s)", s.name, a, v.Level, topReason(v)))
			}
			_ = w.Write([]string{s.name, a, v.Level, v.Confidence, out.Band, out.Score.StringFixed(2),
				out.Coverage.StringFixed(4), topReason(v), fmt.Sprint(ok)})
		}
		n := len(s.addrs)
		fmt.Printf("%-10s %5d %7d %8d %10d   %-16s %d (%.1f%%)\n", s.name, n,
			counts[scoring.VerdictClear], counts[scoring.VerdictCaution], counts[scoring.VerdictHighRisk],
			s.want, misses, pctOf(misses, n))
		if n > 0 {
			res.Notes = append(res.Notes, fmt.Sprintf("%s: mean coverage %.1f%%", s.name, covSum/float64(n)*100))
		}
		res.Passed += n - misses
		res.Failed += misses
	}
	res.Notes = append(res.Notes, "per-address results in "+path)
	return res, nil
}

// backgroundPrefetch queues the benchmark's follow-up fetches at background
// priority. A benchmark is not a customer: queued at customer priority, one
// run put 7,160 jobs ahead of every real follow-up.
type backgroundPrefetch struct {
	*ingest.Worker
	jobs    *store.Jobs
	chainID string
	queued  map[string]bool
}

func (b *backgroundPrefetch) Queue(ctx context.Context, address string, _ int) error {
	b.queued[address] = true
	return b.jobs.EnqueueBackground(ctx, b.chainID, address)
}

// wait waits until none of the addresses this benchmark queued is pending
// or running, or until the limit. It watches only its own jobs, so other
// background work does not hold it up beyond the limit.
func (b *backgroundPrefetch) wait(ctx context.Context, pg *sql.DB, limit time.Duration) {
	addrs := make([]string, 0, len(b.queued))
	for a := range b.queued {
		addrs = append(addrs, a)
	}
	deadline := time.Now().Add(limit)
	for len(addrs) > 0 && time.Now().Before(deadline) {
		var n int
		if err := pg.QueryRowContext(ctx, `SELECT count(*) FROM fetch_jobs WHERE chain = $1 AND address = ANY($2) AND state IN ('pending','running')`,
			b.chainID, addrs).Scan(&n); err != nil || n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}

func verdictOK(want string, v *scoring.Verdict) bool {
	switch want {
	case "high_risk":
		return v.Level == scoring.VerdictHighRisk
	case "not clear":
		return v.Level != scoring.VerdictClear
	case "not high_risk":
		return v.Level != scoring.VerdictHighRisk
	}
	return false
}

func topReason(v *scoring.Verdict) string {
	if len(v.Reasons) == 0 {
		return ""
	}
	r := v.Reasons[0]
	if r.Category != "" {
		return r.Code + ":" + r.Category
	}
	return r.Code
}

func pctOf(a, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(a) / float64(n) * 100
}

func pgAddrs(ctx context.Context, pg *sql.DB, q, chainID string, limit int) ([]string, error) {
	rows, err := pg.QueryContext(ctx, q, chainID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// exposedAddrs are unlabelled, fetched addresses with at least $1,000 of
// direct flow to or from a listed address, the largest flows first.
func exposedAddrs(ctx context.Context, ch, pg *sql.DB, chainID string, listed []string, limit int) ([]string, error) {
	if len(listed) == 0 {
		return nil, nil
	}
	// Chunked: ClickHouse limits query size, and the list runs to thousands.
	flow := map[string]float64{}
	for start := 0; start < len(listed); start += 1000 {
		chunk := listed[start:min(start+1000, len(listed))]
		rows, err := ch.QueryContext(ctx, `
			SELECT a, sum(v) FROM (
				SELECT from_address AS a, total_usd_value AS v FROM edges_by_to_current WHERE chain = ? AND to_address IN (?)
				UNION ALL
				SELECT to_address, total_usd_value FROM edges_current WHERE chain = ? AND from_address IN (?))
			GROUP BY a`, chainID, chunk, chainID, chunk)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a string
			var usd float64
			if err := rows.Scan(&a, &usd); err != nil {
				rows.Close()
				return nil, err
			}
			flow[a] += usd
		}
		rows.Close()
	}
	var cands []string
	for a, usd := range flow {
		if usd >= 1000 {
			cands = append(cands, a)
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if flow[cands[i]] != flow[cands[j]] {
			return flow[cands[i]] > flow[cands[j]]
		}
		return cands[i] < cands[j]
	})
	isListed := make(map[string]bool, len(listed))
	for _, a := range listed {
		isListed[a] = true
	}
	var out []string
	for _, a := range cands {
		if isListed[a] {
			continue
		}
		var labelled, fetched bool
		if err := pg.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM labels WHERE chain = $1 AND address = $2 AND valid_to_snapshot IS NULL),
			       EXISTS (SELECT 1 FROM address_freshness WHERE chain = $1 AND address = $2)`,
			chainID, a).Scan(&labelled, &fetched); err != nil {
			return nil, err
		}
		if !labelled && fetched {
			out = append(out, a)
		}
		if len(out) == limit {
			break
		}
	}
	sort.Strings(out)
	return out, nil
}
