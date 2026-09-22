// Command validate is the acceptance harness for SPEC.md §9.
//
//	validate sanctions    every sanctioned address must land in the High band
//	validate falsepos     ordinary addresses must land Low
//	validate dust         synthesised dust must barely move a score
//	validate stability    the same input must produce identical output
//	validate compare      divergence table against externally-obtained scores
//	validate all          every check
//
// SPEC.md §9 calls this a real harness rather than ad-hoc scripts, and makes a
// sanctions miss a build-breaking bug. Exit code is non-zero when a
// build-breaking check fails, so CI can gate on it.
package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/graph"
	"github.com/mozer/tether-risk/internal/labels"
	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/mozer/tether-risk/internal/screen"
	"github.com/mozer/tether-risk/internal/store"
	"github.com/shopspring/decimal"
)

func main() {
	var (
		configDir = flag.String("config", "config", "configuration directory")
		chainID   = flag.String("chain", "tron", "chain")
		limit     = flag.Int("limit", 50, "maximum addresses per check")
		compareIn = flag.String("compare-file", "testdata/external/scores.csv",
			"CSV of externally-obtained scores: address,vendor,score,band")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "all"
	}

	code, err := run(ctx, cmd, *configDir, *chainID, *limit, *compareIn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

// checkResult is the outcome of one validation check.
type checkResult struct {
	Name string

	Passed  int
	Failed  int
	Skipped int

	// BuildBreaking marks a check whose failure must fail the build.
	// SPEC.md §9.1: any sanctions miss is a build-breaking bug.
	BuildBreaking bool

	Failures []string
	Notes    []string
}

func (c checkResult) ok() bool { return c.Failed == 0 }

func run(ctx context.Context, cmd, configDir, chainID string, limit int, compareFile string) (int, error) {
	cfg, err := config.Load(configDir)
	if err != nil {
		return 2, err
	}

	ch, err := store.OpenClickHouse(ctx)
	if err != nil {
		return 2, err
	}
	defer ch.Close()

	pg, err := store.OpenPostgres(ctx)
	if err != nil {
		return 2, err
	}
	defer pg.Close()

	svc := screen.NewService(ch, pg, cfg)

	var results []checkResult

	switch cmd {
	case "sanctions":
		r, err := checkSanctionsRecall(ctx, svc, pg, chainID, limit)
		if err != nil {
			return 2, err
		}
		results = append(results, r)

	case "falsepos":
		r, err := checkFalsePositives(ctx, svc, pg, chainID, limit)
		if err != nil {
			return 2, err
		}
		results = append(results, r)

	case "dust":
		r, err := checkDustResistance(ctx, cfg)
		if err != nil {
			return 2, err
		}
		results = append(results, r)

	case "stability":
		r, err := checkStability(ctx, svc, pg, chainID, limit)
		if err != nil {
			return 2, err
		}
		results = append(results, r)

	case "verdict":
		r, err := checkVerdictBenchmark(ctx, svc, ch, pg, chainID, limit)
		if err != nil {
			return 2, err
		}
		results = append(results, r)

	case "compare":
		r, err := checkExternalComparison(ctx, svc, chainID, compareFile)
		if err != nil {
			return 2, err
		}
		results = append(results, r)

	case "all":
		for _, fn := range []func() (checkResult, error){
			func() (checkResult, error) { return checkSanctionsRecall(ctx, svc, pg, chainID, limit) },
			func() (checkResult, error) { return checkFalsePositives(ctx, svc, pg, chainID, limit) },
			func() (checkResult, error) { return checkDustResistance(ctx, cfg) },
			func() (checkResult, error) { return checkStability(ctx, svc, pg, chainID, limit) },
			func() (checkResult, error) { return checkExternalComparison(ctx, svc, chainID, compareFile) },
		} {
			r, err := fn()
			if err != nil {
				return 2, err
			}
			results = append(results, r)
		}

	default:
		return 2, fmt.Errorf("unknown check %q", cmd)
	}

	_ = labels.NewStore(pg)
	return report(results), nil
}

// ---------------------------------------------------------------------------
// 1. Sanctions recall — SPEC.md §9.1
// ---------------------------------------------------------------------------

// checkSanctionsRecall asserts every sanctioned address lands in the High
// band. SPEC.md §9.1: "Any miss is a build-breaking bug."
func checkSanctionsRecall(ctx context.Context, svc *screen.Service, pg *sql.DB, chainID string, limit int) (checkResult, error) {
	res := checkResult{Name: "sanctions recall", BuildBreaking: true}

	rows, err := pg.QueryContext(ctx, `
		SELECT address FROM labels
		WHERE chain = $1 AND category IN ('sanctions','terrorist_financing')
		  AND valid_to_snapshot IS NULL
		ORDER BY address LIMIT $2`, chainID, limit)
	if err != nil {
		return res, err
	}
	defer rows.Close()

	var addresses []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return res, err
		}
		addresses = append(addresses, a)
	}
	if err := rows.Err(); err != nil {
		return res, err
	}

	if len(addresses) == 0 {
		res.Notes = append(res.Notes,
			"no sanctioned addresses in the label set; run the labeler first")
		return res, nil
	}

	for _, addr := range addresses {
		out, err := svc.Screen(ctx, chainID, addr)
		if err != nil {
			res.Failed++
			res.Failures = append(res.Failures, fmt.Sprintf("%s: screening failed: %v", addr, err))
			continue
		}
		if out.Band != "high" {
			res.Failed++
			res.Failures = append(res.Failures, fmt.Sprintf(
				"%s is sanctioned but banded %q (score %s) — SPEC.md §9.1 calls this build-breaking",
				addr, out.Band, out.Score.StringFixed(2)))
			continue
		}
		res.Passed++
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// 2. False positives — SPEC.md §9.2
// ---------------------------------------------------------------------------

// checkFalsePositives asserts that ordinary addresses land Low.
//
// The sample is drawn from addresses carrying an `exchange` label, which are
// the clearest available stand-in for "ordinary" until a hand-picked set
// exists. SPEC.md §9.2 asks for exchange deposit addresses and long-lived
// personal wallets specifically; that set has to be assembled by a human, and
// the check says so rather than pretending an automatic sample is equivalent.
func checkFalsePositives(ctx context.Context, svc *screen.Service, pg *sql.DB, chainID string, limit int) (checkResult, error) {
	res := checkResult{Name: "false positives"}

	rows, err := pg.QueryContext(ctx, `
		SELECT address FROM labels
		WHERE chain = $1 AND category IN ('exchange','dex')
		  AND valid_to_snapshot IS NULL
		ORDER BY address LIMIT $2`, chainID, limit)
	if err != nil {
		return res, err
	}
	defer rows.Close()

	var addresses []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return res, err
		}
		addresses = append(addresses, a)
	}

	if len(addresses) == 0 {
		res.Notes = append(res.Notes,
			"no exchange-labelled addresses available, so this check cannot run. "+
				"It needs a hand-picked set of ordinary deposit addresses and "+
				"long-lived personal wallets (SPEC.md §9.2).")
		return res, nil
	}

	for _, addr := range addresses {
		out, err := svc.Screen(ctx, chainID, addr)
		if err != nil {
			res.Failed++
			res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", addr, err))
			continue
		}
		if out.Band != "low" {
			res.Failed++
			res.Failures = append(res.Failures, fmt.Sprintf(
				"%s is an ordinary exchange address but banded %q (score %s) — investigate",
				addr, out.Band, out.Score.StringFixed(2)))
			continue
		}
		res.Passed++
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// 3. Dust resistance — SPEC.md §9.3
// ---------------------------------------------------------------------------

// checkDustResistance synthesises dust into a clean profile and asserts the
// score barely moves.
//
// Run against synthesised traversal results rather than the database, so it
// tests the scoring rule itself and needs no particular chain data present.
func checkDustResistance(ctx context.Context, cfg *config.Config) (checkResult, error) {
	res := checkResult{Name: "dust resistance", BuildBreaking: true}

	clean, dusted, err := dustScenario(cfg)
	if err != nil {
		return res, err
	}

	delta := dusted.Sub(clean).Abs()
	tolerance := decimal.NewFromFloat(0.5)

	res.Notes = append(res.Notes, fmt.Sprintf(
		"clean score %s, after 500 synthesised dust transfers %s, delta %s",
		clean.StringFixed(4), dusted.StringFixed(4), delta.StringFixed(4)))

	if delta.GreaterThan(tolerance) {
		res.Failed++
		res.Failures = append(res.Failures, fmt.Sprintf(
			"dust moved the score by %s, above the %s tolerance; SPEC.md §7 requires "+
				"inbound dust never meaningfully move a score",
			delta.StringFixed(4), tolerance.StringFixed(2)))
		return res, nil
	}
	res.Passed++
	return res, nil
}

// ---------------------------------------------------------------------------
// 4. Stability — SPEC.md §9.4
// ---------------------------------------------------------------------------

// checkStability re-runs the same addresses against the same snapshot and
// asserts identical output.
func checkStability(ctx context.Context, svc *screen.Service, pg *sql.DB, chainID string, limit int) (checkResult, error) {
	res := checkResult{Name: "stability", BuildBreaking: true}

	rows, err := pg.QueryContext(ctx, `
		SELECT DISTINCT address FROM runs WHERE chain = $1 ORDER BY address LIMIT $2`,
		chainID, limit)
	if err != nil {
		return res, err
	}
	defer rows.Close()

	var addresses []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return res, err
		}
		addresses = append(addresses, a)
	}

	if len(addresses) == 0 {
		res.Notes = append(res.Notes, "no previous runs to compare against; screen some addresses first")
		return res, nil
	}

	for _, addr := range addresses {
		first, err := svc.Screen(ctx, chainID, addr)
		if err != nil {
			res.Failed++
			res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", addr, err))
			continue
		}
		second, err := svc.Screen(ctx, chainID, addr)
		if err != nil {
			res.Failed++
			res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", addr, err))
			continue
		}

		a, b := fingerprint(first), fingerprint(second)
		if a != b {
			res.Failed++
			res.Failures = append(res.Failures, fmt.Sprintf(
				"%s produced different output on re-run:\n     %s\n     %s", addr, a, b))
			continue
		}
		res.Passed++
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// 5. External comparison — SPEC.md §9.5
// ---------------------------------------------------------------------------

// checkExternalComparison produces the divergence table.
//
// SPEC.md §9.5: "Do not tune weights to match them — record and explain
// divergence." So this check never fails the build on divergence; divergence
// is the output, not the error. It fails only if a listed address cannot be
// screened at all.
func checkExternalComparison(ctx context.Context, svc *screen.Service, chainID, path string) (checkResult, error) {
	res := checkResult{Name: "external comparison"}

	f, err := os.Open(path)
	if err != nil {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"no comparison data at %s. Obtaining public scores is a manual step "+
				"(docs/PLAN.md F4); the file is address,vendor,score,band.", path))
		return res, nil
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1

	type row struct {
		address, vendor, band string
		score                 decimal.Decimal
	}
	var rows []row

	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, fmt.Errorf("read %s: %w", path, err)
		}
		if len(rec) < 4 || strings.EqualFold(strings.TrimSpace(rec[0]), "address") {
			continue // header or short line
		}
		score, err := decimal.NewFromString(strings.TrimSpace(rec[2]))
		if err != nil {
			continue
		}
		rows = append(rows, row{
			address: strings.TrimSpace(rec[0]),
			vendor:  strings.TrimSpace(rec[1]),
			score:   score,
			band:    strings.ToLower(strings.TrimSpace(rec[3])),
		})
	}

	if len(rows) == 0 {
		res.Notes = append(res.Notes, "comparison file has no usable rows")
		return res, nil
	}

	fmt.Println("\nDIVERGENCE TABLE (SPEC.md §9.5)")
	fmt.Println("Weights are never tuned to match these. Divergence is the finding.")
	fmt.Printf("\n%-36s %-12s %8s %8s %8s  %-10s %-10s %s\n",
		"address", "vendor", "theirs", "ours", "delta", "their band", "our band", "our coverage")
	fmt.Println(strings.Repeat("-", 130))

	for _, rw := range rows {
		out, err := svc.Screen(ctx, chainID, rw.address)
		if err != nil {
			res.Failed++
			res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", rw.address, err))
			continue
		}
		delta := out.Score.Sub(rw.score)
		fmt.Printf("%-36s %-12s %8s %8s %8s  %-10s %-10s %s%%\n",
			rw.address, rw.vendor,
			rw.score.StringFixed(1), out.Score.StringFixed(1), delta.StringFixed(1),
			rw.band, out.Band,
			out.Coverage.Mul(decimal.NewFromInt(100)).StringFixed(1))
		res.Passed++
	}

	res.Notes = append(res.Notes,
		"divergence is expected and is not a failure. Our coverage column is the "+
			"main explanatory variable: a low coverage figure means we traced the "+
			"same flows but could not name the counterparties.")
	return res, nil
}

// ---------------------------------------------------------------------------

func report(results []checkResult) int {
	fmt.Println()
	fmt.Println("VALIDATION REPORT")
	fmt.Println(strings.Repeat("=", 70))

	exit := 0
	for _, r := range results {
		status := "PASS"
		switch {
		case !r.ok() && r.BuildBreaking:
			status = "FAIL (build-breaking)"
			exit = 1
		case !r.ok():
			status = "FAIL"
			if exit == 0 {
				exit = 1
			}
		case r.Passed == 0:
			status = "SKIPPED"
		}

		fmt.Printf("\n%-24s %s   (%d passed, %d failed)\n", r.Name, status, r.Passed, r.Failed)
		for _, n := range r.Notes {
			fmt.Printf("    note: %s\n", n)
		}
		for i, f := range r.Failures {
			if i >= 10 {
				fmt.Printf("    ... and %d more\n", len(r.Failures)-10)
				break
			}
			fmt.Printf("    - %s\n", f)
		}
	}

	fmt.Println()
	if exit == 0 {
		fmt.Println("all checks passed")
	} else {
		fmt.Println("validation failed")
	}
	return exit
}

// fingerprint renders a result deterministically, so two runs can be compared
// exactly rather than approximately.
//
// SPEC.md §9.4 asks for identical output, not merely a similar score, so this
// includes the full category breakdown and the coverage figure. A run that
// produced the same number by a different route has not demonstrated
// determinism.
func fingerprint(r *scoring.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "score=%s band=%s coverage=%s snapshot=%d config=%s",
		r.Score.StringFixed(8), r.Band, r.Coverage.StringFixed(8),
		r.LabelSnapshotID, r.ConfigVersion)

	for _, d := range []*scoring.DirectionResult{r.Inbound, r.Outbound} {
		if d == nil {
			continue
		}
		fmt.Fprintf(&b, " |%s score=%s cov=%s unattr=%s",
			d.Direction, d.Score.StringFixed(8),
			d.Coverage.StringFixed(8), d.UnattributedPct.StringFixed(8))
		for _, c := range d.Categories {
			fmt.Fprintf(&b, " %s=%s", c.Category, c.Pct.StringFixed(8))
		}
	}
	return b.String()
}

// dustScenario builds a clean profile and the same profile buried in
// synthesised dust, and returns both scores.
//
// SPEC.md §9.3: synthesise dust transfers into a clean address and assert the
// score barely moves. Built from traversal results directly rather than from
// stored chain data, so the check tests the scoring rule itself and does not
// depend on any particular address being ingested.
func dustScenario(cfg *config.Config) (clean, dusted decimal.Decimal, err error) {
	scorer := scoring.New(cfg)

	mk := func(paths []graph.Path) *graph.Result {
		r := &graph.Result{
			Chain: "tron", Address: "TCleanAddress", Direction: graph.Inbound,
			Paths: paths, TotalTraced: decimal.Zero, Attributed: decimal.Zero,
		}
		for _, p := range paths {
			r.TotalTraced = r.TotalTraced.Add(p.Contribution)
			if p.Terminal.Attributed() {
				r.Attributed = r.Attributed.Add(p.Contribution)
			}
		}
		return r
	}

	labelled := func(addr, category string, contribution float64) graph.Path {
		return graph.Path{
			Hops:         []string{addr},
			Shares:       []decimal.Decimal{decimal.NewFromInt(1)},
			Contribution: decimal.NewFromFloat(contribution),
			Terminal: graph.Terminal{
				Address: addr, Entity: addr, Category: category, Reason: "labelled",
			},
		}
	}

	// A clean profile: all inbound value from a regulated exchange.
	base := []graph.Path{labelled("TExchange", "exchange", 1.0)}

	cleanRes, err := scorer.ScoreDirection(mk(base))
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}

	// The same profile, plus 500 dusting senders. Each dust path carries the
	// tiny contribution a sub-$1 transfer actually produces against a profile
	// holding real value.
	dustedPaths := append([]graph.Path(nil), base...)
	for i := 0; i < 500; i++ {
		dustedPaths = append(dustedPaths, graph.Path{
			Hops:         []string{fmt.Sprintf("TDuster%03d", i)},
			Shares:       []decimal.Decimal{decimal.NewFromFloat(0.000001)},
			Contribution: decimal.NewFromFloat(0.0000005),
			Terminal: graph.Terminal{
				Address: fmt.Sprintf("TDuster%03d", i), Category: "dust", Reason: "dust",
			},
		})
	}

	dustedRes, err := scorer.ScoreDirection(mk(dustedPaths))
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}

	return cleanRes.Score, dustedRes.Score, nil
}
