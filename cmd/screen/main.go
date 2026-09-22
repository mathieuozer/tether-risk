// Command screen scores a single address and prints the breakdown.
//
// This is the triage output in text form; cmd/api serves the same result as
// JSON. SPEC.md §1: every response must carry the framing that this is a
// triage tool and not a regulated AML determination.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/ingest"
	"github.com/mozer/tether-risk/internal/report"
	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/mozer/tether-risk/internal/screen"
	"github.com/mozer/tether-risk/internal/store"
	"github.com/shopspring/decimal"
)

const disclaimer = "This is an automated triage and pre-screening result built on open data. " +
	"It is not a regulated AML determination and must not be used as one."

func main() {
	var (
		chainID   = flag.String("chain", "tron", "chain")
		configDir = flag.String("config", "config", "configuration directory")
		pdfOut    = flag.String("pdf", "", "also write a PDF report to this path")
		fetch     = flag.Bool("fetch", true,
			"fetch the address and queue its counterparties before scoring; false scores stored data only")
		format = flag.String("format", "detailed",
			"output format: detailed (per-direction breakdown) or summary (combined connections list)")
	)
	flag.Parse()

	address := flag.Arg(0)
	if address == "" {
		fmt.Fprintln(os.Stderr, "usage: screen [flags] <address>")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *format != "detailed" && *format != "summary" {
		fmt.Fprintf(os.Stderr, "unknown -format %q; use detailed or summary\n", *format)
		os.Exit(2)
	}

	if err := run(ctx, *configDir, *chainID, address, *pdfOut, *format, *fetch); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configDir, chainID, address, pdfOut, format string, fetch bool) error {
	cfg, err := config.Load(configDir)
	if err != nil {
		return err
	}

	ch, err := store.OpenClickHouse(ctx)
	if err != nil {
		return err
	}
	defer ch.Close()

	pg, err := store.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer pg.Close()

	svc := screen.NewService(ch, pg, cfg)
	if fetch {
		quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
		p, err := ingest.NewPrefetcher(chainID, cfg, ch, pg, quiet)
		if err != nil {
			return err
		}
		svc.WithPrefetch(chainID, p)
	}
	res, err := svc.Screen(ctx, chainID, address)
	if err != nil {
		return err
	}

	if format == "summary" {
		fmt.Println()
		fmt.Print(report.Connections(connectionsInput(res)))
		fmt.Println()
	} else {
		print(res)
	}

	if pdfOut != "" {
		f, err := os.Create(pdfOut)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := report.Render(f, res, time.Now()); err != nil {
			return fmt.Errorf("render report: %w", err)
		}
		fmt.Printf("report written to %s\n\n", pdfOut)
	}
	return nil
}

func print(res *scoring.Result) {
	fmt.Printf("\nAddress:    %s\n", res.Address)
	fmt.Printf("Chain:      %s\n", res.Chain)
	fmt.Printf("Score:      %s / 100\n", res.Score.StringFixed(1))
	fmt.Printf("Band:       %s\n", strings.ToUpper(res.Band))

	if res.SanctionsOverride {
		fmt.Println("            ^ forced to HIGH by a direct sanctions hit on this address")
	}

	// A label on the queried address itself is a finding independent of any
	// traced exposure, and is printed before the breakdown so it cannot be
	// missed. An address on a scam blacklist with no traced value scores zero;
	// reporting only "LOW" for it would omit the most important thing known.
	if l := res.OwnLabel; l != nil {
		fmt.Println()
		fmt.Println("  *** THIS ADDRESS IS DIRECTLY LISTED ***")
		fmt.Printf("  %s\n", l.Entity)
		fmt.Printf("  category %s, source %s, confidence %.2f\n", l.Category, l.Source, l.Confidence)
		if l.Conflicted {
			fmt.Println("  sources disagree about this address; see label_conflicts")
		}
		fmt.Println("  This is a direct listing, not exposure traced through other addresses.")
	}
	if res.BandCappedByAbuseRule {
		fmt.Println("            ^ capped: the only evidence is unverified abuse reports")
	}

	// SPEC.md §7: coverage in every response, and prominently when low.
	fmt.Printf("Coverage:   %s%%\n", res.Coverage.Mul(decimal.NewFromInt(100)).StringFixed(1))
	if res.LowConfidence {
		fmt.Println()
		fmt.Println("  *** LOW CONFIDENCE ***")
		fmt.Println("  Less than 40% of traced value could be attributed to a known entity.")
		fmt.Println("  The score below describes only the part we could identify. Treat the")
		fmt.Println("  unattributed remainder as unknown, not as clean.")
	}

	if d := res.Depth; d != nil {
		if d.FetchError != "" {
			fmt.Printf("\n  could not refresh from the chain; stored data used: %s\n", d.FetchError)
		}
		if d.StillFetching {
			fmt.Println("History:    still being fetched; figures are partial, screen again shortly")
		}
		if d.HistoryTruncated {
			fmt.Println("History:    truncated at the per-address fetch limit; activity covers the most recent part")
		}
		if d.FrontierPending > 0 {
			fmt.Printf("Frontier:   %d addresses where the trail stops are queued (%d pending, %d genuine ends)\n",
				d.FrontierQueued, d.FrontierPending, d.FrontierEnded)
		}
		if d.Counterparties > 0 {
			state := "complete"
			if d.Traced < d.Counterparties {
				state = "in progress, screen again later for a deeper result"
			}
			fmt.Printf("Traced:     %d of %d counterparties (%s)\n", d.Traced, d.Counterparties, state)
		}
	}

	printDirection("INBOUND  (where funds came from)", res.Inbound)
	printDirection("OUTBOUND (where funds went)", res.Outbound)

	fmt.Printf("\nLabel snapshot: %d\n", res.LabelSnapshotID)
	fmt.Printf("Config version: %s\n", res.ConfigVersion)
	fmt.Printf("\n%s\n\n", wrap(disclaimer, 74))
}

func printDirection(title string, d *scoring.DirectionResult) {
	if d == nil {
		return
	}
	fmt.Printf("\n%s\n", title)
	fmt.Println(strings.Repeat("-", len(title)))

	if len(d.Categories) == 0 && d.UnattributedPct.IsZero() {
		// "No traced value" and "we hold history we have not priced" are
		// different findings, and only the first means the address is quiet.
		if d.HasUnpricedData {
			fmt.Printf("  NO USABLE VALUE: %d transfers exist here but carry no USD price,\n",
				d.UnpricedTransfers)
			fmt.Println("  so nothing could be traced. This is a pricing gap, not an inactive")
			fmt.Println("  address. Run `price backfill` and screen again.")
			return
		}
		fmt.Println("  no traced value")
		return
	}

	fmt.Printf("  score %s, coverage %s%%\n",
		d.Score.StringFixed(1), d.Coverage.Mul(decimal.NewFromInt(100)).StringFixed(1))

	for _, c := range d.Categories {
		fmt.Printf("    %-22s %7s%%   (weight %s, contributes %s)\n",
			c.Category, c.Pct.StringFixed(1), c.Weight.StringFixed(0), c.Contribution.StringFixed(2))
	}
	if d.UnattributedPct.IsPositive() {
		fmt.Printf("    %-22s %7s%%   (unknown — not counted as clean)\n",
			"unattributed", d.UnattributedPct.StringFixed(1))
	}

	// SPEC.md §7 requires the result be honest about truncation.
	if d.FanoutCapped {
		fmt.Println("    note: fan-out cap was hit; this traversal is truncated")
	}
	if d.HopLimitReached {
		fmt.Println("    note: hop limit was reached; value beyond it is unknown")
	}

	if len(d.TopPaths) > 0 {
		fmt.Println("  top contributing paths:")
		for i, p := range d.TopPaths {
			if i >= 5 {
				break
			}
			name := p.Terminal.Entity
			if name == "" {
				name = p.Terminal.Address
			}
			fmt.Printf("    %d. %s (%s) via %d hop(s), contributing %s\n",
				i+1, name, p.Terminal.Category, p.HopCount(), p.Contribution.StringFixed(4))
		}
	}
}

func wrap(s string, width int) string {
	var out strings.Builder
	line := 0
	for _, word := range strings.Fields(s) {
		if line+len(word)+1 > width {
			out.WriteString("\n")
			line = 0
		} else if line > 0 {
			out.WriteString(" ")
			line++
		}
		out.WriteString(word)
		line += len(word)
	}
	return out.String()
}

// connectionsInput adapts a scoring result for the compact summary.
func connectionsInput(res *scoring.Result) report.ConnectionsInput {
	in := report.ConnectionsInput{
		Address:           res.Address,
		Chain:             res.Chain,
		Score:             res.Score.InexactFloat64(),
		Band:              res.Band,
		Coverage:          res.Coverage.InexactFloat64(),
		LowConfidence:     res.LowConfidence,
		SanctionsOverride: res.SanctionsOverride,
		BandCappedByAbuse: res.BandCappedByAbuseRule,
		Inbound:           connectionsDirection(res.Inbound),
		Outbound:          connectionsDirection(res.Outbound),
		Disclaimer:        disclaimer,
	}
	if l := res.OwnLabel; l != nil {
		in.OwnLabel = &report.ConnectionsOwnLabel{Entity: l.Entity, Category: l.Category}
	}
	if d := res.Depth; d != nil {
		in.Depth = &report.ConnectionsDepth{
			FetchError: d.FetchError, StillFetching: d.StillFetching, HistoryTruncated: d.HistoryTruncated,
			FrontierPending: d.FrontierPending, FrontierQueued: d.FrontierQueued,
			Counterparties: d.Counterparties,
			Traced:         d.Traced, TotalCounterparties: d.TotalCounterparties,
		}
	}
	if a := res.Activity; a != nil {
		act := &report.ConnectionsActivity{
			InUSD: a.InUSD.InexactFloat64(), OutUSD: a.OutUSD.InexactFloat64(),
			InTransfers: a.InTransfers, OutTransfers: a.OutTransfers,
			InCounterparties: a.InCounterparties, OutCounterparties: a.OutCounterparties,
			UnpricedTransfers: a.UnpricedTransfers, UnpricedTokens: a.UnpricedTokens,
		}
		if !a.FirstSeen.IsZero() {
			act.FirstSeen = a.FirstSeen.UTC().Format("2006-01-02")
			act.LastSeen = a.LastSeen.UTC().Format("2006-01-02")
		}
		for _, as := range a.Assets {
			act.Assets = append(act.Assets, report.ConnectionsAsset{
				Asset: as.Asset, USD: as.InUSD.Add(as.OutUSD).InexactFloat64(),
			})
		}
		in.Activity = act
	}
	return in
}

func connectionsDirection(d *scoring.DirectionResult) *report.ConnectionsDirection {
	if d == nil {
		return nil
	}
	out := &report.ConnectionsDirection{
		TracedWeight:    d.TotalTraced.InexactFloat64(),
		UnattributedPct: d.UnattributedPct.InexactFloat64(),
		FanoutCapped:    d.FanoutCapped,
		HopLimitReached: d.HopLimitReached,
	}
	for _, c := range d.Categories {
		out.Categories = append(out.Categories, report.ConnectionsCategory{
			Category: c.Category, Pct: c.Pct.InexactFloat64(),
		})
	}
	for _, c := range d.Connections {
		out.Entries = append(out.Entries, report.ConnectionsEntry{
			Address: c.Address, Entity: c.Entity, Category: c.Category,
			Pct: c.Pct.InexactFloat64(), MinHops: c.MinHops,
		})
	}
	for _, r := range d.UnattributedReasons {
		out.Reasons = append(out.Reasons, report.ConnectionsReason{Reason: r.Reason, Pct: r.Pct.InexactFloat64()})
	}
	return out
}
