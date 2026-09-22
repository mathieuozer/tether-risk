package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/config"
)

// traceTx follows a controlled test transfer (docs/DECISIONS.md D19).
//
// It prints what the chain shows and a curated_labels.yaml snippet, and
// writes nothing. A curated label terminates traversal at confidence 0.95, so
// a person reviews every entry before it goes in.
func traceTx(ctx context.Context, cfg *config.Config, chainID, txID, fromExchange, toExchange string, log *slog.Logger) error {
	if chainID != "tron" {
		return fmt.Errorf("trace-tx supports tron only")
	}
	chainCfg, ok := cfg.Chain(chainID)
	if !ok {
		return fmt.Errorf("chain %s is not declared in sources.yaml", chainID)
	}
	key := os.Getenv("TRONGRID_API_KEY")
	client := tron.NewClient(tron.Options{
		BaseURL:           chainCfg.URL,
		APIKey:            key,
		RequestsPerSecond: chainCfg.RequestRate(key != ""),
		Logger:            log,
	})

	transfers, err := client.TransferEvents(ctx, txID)
	if err != nil {
		return err
	}

	today := time.Now().UTC().Format("2006-01-02")
	var snippet strings.Builder

	for _, t := range transfers {
		fmt.Printf("transfer   %s  %s  %s\n", t.Time.Format(time.RFC3339), amount(t), t.Asset)
		fmt.Printf("  from     %s   (withdrawal wallet of %s)\n", t.From, orUnknown(fromExchange))
		fmt.Printf("  to       %s   (your deposit address at %s)\n", t.To, orUnknown(toExchange))
		fmt.Printf("  evidence %s\n", txURL(t.TxID))

		if !tron.IsCanonicalToken(t.Contract) {
			// A counterfeit token proves nothing about who controls either
			// end. Refuse rather than print a label that would be believed.
			fmt.Printf("  NOT A RECOGNISED STABLECOIN (contract %s); nothing here can be used as evidence\n\n", t.Contract)
			continue
		}

		snippet.WriteString(labelYAML(t.From, fromExchange, "withdrawal hot wallet", txURL(t.TxID), today,
			"Sender of a controlled test withdrawal from this exchange."))

		// The sweep usually follows within hours, but can take days for a
		// small deposit. Report what is visible now; running again later
		// picks up what has happened since.
		fees, err := client.NativeInbound(ctx, t.To, t.Time)
		if err != nil {
			return err
		}
		for _, f := range fees {
			fmt.Printf("  top-up   %s  %s TRX from %s   %s\n",
				f.Time.Format(time.RFC3339), trx(f.Value), f.From, txURL(f.TxID))
		}

		out, err := client.Outbound(ctx, t.To, t.Time)
		if err != nil {
			return err
		}
		var swept bool
		for _, s := range out {
			if s.Contract != t.Contract {
				continue
			}
			swept = true
			fmt.Printf("  sweep    %s  %s %s to %s   %s\n",
				s.Time.Format(time.RFC3339), amount(s), s.Asset, s.To, txURL(s.TxID))
			snippet.WriteString(labelYAML(s.To, toExchange, "deposit collection wallet", txURL(s.TxID), today,
				"Destination of the sweep from a controlled test deposit to this exchange."))
		}
		if !swept {
			fmt.Println("  sweep    not yet; the exchange has not moved the deposit. Run again later.")
		}
		fmt.Println()
	}

	if snippet.Len() > 0 {
		fmt.Println("# Review before adding to config/curated_labels.yaml.")
		fmt.Println("# Check each address on the evidence link, and that the entity names are right.")
		fmt.Print(snippet.String())
	}
	return nil
}

func labelYAML(address, exchange, role, evidence, date, note string) string {
	return fmt.Sprintf(`  - chain: tron
    address: %q
    entity: %q
    category: exchange
    evidence: %q
    verified: %s
    note: >
      %s Established by a transaction the maintainer made,
      not taken from any third-party list (docs/DECISIONS.md D19).
`, address, orUnknown(exchange)+" "+role, evidence, date, note)
}

func orUnknown(s string) string {
	if s == "" {
		return "UNKNOWN EXCHANGE"
	}
	return s
}

func txURL(id string) string { return "https://tronscan.org/#/transaction/" + id }

// amount formats a stablecoin amount. Every recognised stablecoin's decimals
// are known; anything else is shown raw rather than guessed.
func amount(m tron.Movement) string {
	dec := map[string]int{"USDT": 6, "USDC": 6, "TUSD": 18, "USDD": 18}[m.Asset]
	if dec == 0 {
		return m.Value.String() + " (raw)"
	}
	return new(big.Rat).SetFrac(m.Value, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)).FloatString(2)
}

func trx(v *big.Int) string {
	return new(big.Rat).SetFrac(v, big.NewInt(1_000_000)).FloatString(2)
}
