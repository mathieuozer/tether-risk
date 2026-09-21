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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mozer/tether-risk/internal/config"
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
	)
	flag.Parse()

	address := flag.Arg(0)
	if address == "" {
		fmt.Fprintln(os.Stderr, "usage: screen [flags] <address>")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *configDir, *chainID, address, *pdfOut); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configDir, chainID, address, pdfOut string) error {
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

	res, err := screen.NewService(ch, pg, cfg).Screen(ctx, chainID, address)
	if err != nil {
		return err
	}

	print(res)

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
