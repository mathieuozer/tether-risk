package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/mozer/tether-risk/internal/screen"
)

// compareSample reads a filled-in `validate sample` sheet and sets our
// answer beside the competitor's for each address: the verdict, and the
// categories both report. Agreement is counted per stratum. Nothing here
// feeds back into weights (SPEC.md §9.5): each divergence is a question to
// answer, not a number to fit.
func compareSample(ctx context.Context, svc *screen.Service, chainID string, header []string, recs [][]string) (checkResult, error) {
	res := checkResult{Name: "competitor comparison"}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	pct := func(rec []string, name string) float64 {
		v, _ := strconv.ParseFloat(strings.TrimSuffix(strings.ReplaceAll(get(rec, name), ",", "."), "%"), 64)
		return v
	}

	type tally struct{ n, agree, theirHigh, oursRisky, medium int }
	byStratum := map[string]*tally{}
	dir := filepath.Join(".data", "benchmark")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "compare-"+time.Now().UTC().Format("20060102-1504")+".csv")
	f, err := os.Create(path)
	if err != nil {
		return res, err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{"address", "stratum", "their_band", "their_score", "our_verdict", "our_confidence", "our_score",
		"coverage", "their_exchange", "our_exchange", "our_unnamed", "their_sanctions", "our_sanctions", "agreement"})

	fmt.Printf("\n%-36s %-8s %-7s %6s | %-13s %5s %6s | %s\n", "address", "stratum", "theirs", "score", "ours", "conf", "cov", "agreement")
	for _, rec := range recs {
		addr, band := get(rec, "address"), strings.ToLower(get(rec, "band"))
		if addr == "" || band == "" {
			continue // not screened in the competitor's tool yet
		}
		stratum := get(rec, "stratum")
		out, err := svc.Screen(ctx, chainID, addr)
		if err != nil || out.Verdict == nil {
			res.Failed++
			res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", addr, err))
			continue
		}
		v := out.Verdict
		risky := v.Level == scoring.VerdictHighRisk
		ours := "not risky"
		if risky {
			ours = "risky"
		}
		// Their high against our risky, their low against our not risky.
		// Medium is recorded, not scored: it says neither.
		agreement := "medium"
		switch {
		case strings.HasPrefix(band, "high"):
			agreement = map[bool]string{true: "agree", false: "they_risky"}[risky]
		case strings.HasPrefix(band, "low"):
			agreement = map[bool]string{true: "we_risky", false: "agree"}[risky]
		}
		t := byStratum[stratum]
		if t == nil {
			t = &tally{}
			byStratum[stratum] = t
		}
		t.n++
		switch agreement {
		case "agree":
			t.agree++
		case "medium":
			t.medium++
		}
		if strings.HasPrefix(band, "high") {
			t.theirHigh++
		}
		if risky {
			t.oursRisky++
		}

		shares := scoring.CombinedShares(out)
		cov := out.Coverage.InexactFloat64() * 100
		fmt.Printf("%-36s %-8s %-7s %6s | %-13s %4d%% %5.1f%% | %s\n", addr, stratum, band, get(rec, "score"),
			ours, v.ConfidencePct, cov, agreement)
		_ = w.Write([]string{addr, stratum, band, get(rec, "score"), ours, strconv.Itoa(v.ConfidencePct),
			out.Score.StringFixed(1), fmt.Sprintf("%.1f", cov),
			fmt.Sprintf("%.1f", pct(rec, "exchange")), fmt.Sprintf("%.1f", shares["exchange"]), fmt.Sprintf("%.1f", shares["unnamed_service"]),
			fmt.Sprintf("%.2f", pct(rec, "sanctions")), fmt.Sprintf("%.2f", shares["sanctions"]), agreement})
		res.Passed++
	}

	fmt.Printf("\n%-8s %4s %7s %9s %11s %10s\n", "stratum", "n", "agree", "medium", "their high", "our risky")
	names := make([]string, 0, len(byStratum))
	for s := range byStratum {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		t := byStratum[s]
		fmt.Printf("%-8s %4d %7d %9d %11d %10d\n", s, t.n, t.agree, t.medium, t.theirHigh, t.oursRisky)
	}
	res.Notes = append(res.Notes, "rows written to "+path,
		"each disagreement is a question to answer, not a reason to move weights (SPEC.md §9.5)")
	return res, nil
}
