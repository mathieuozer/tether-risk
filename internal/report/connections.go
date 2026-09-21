package report

import (
	"fmt"
	"sort"
	"strings"
)

// ConnectionsInput is what the compact "connections" summary needs.
//
// It is a plain struct rather than *scoring.Result so the Telegram bot, which
// consumes the HTTP API as any other client would, can build one from the
// API's JSON without importing the scoring package.
type ConnectionsInput struct {
	Address string
	Chain   string

	Score float64 // 0-100
	Band  string

	Coverage      float64 // 0-1
	LowConfidence bool

	SanctionsOverride bool
	BandCappedByAbuse bool

	// OwnLabel is set when the queried address itself carries a label.
	OwnLabel *ConnectionsOwnLabel

	Inbound  *ConnectionsDirection
	Outbound *ConnectionsDirection

	Disclaimer string
}

type ConnectionsOwnLabel struct {
	Entity   string
	Category string
}

type ConnectionsDirection struct {
	// TracedWeight is the direction's total traced path weight (a share of
	// value decayed per hop, not USD). Weighting by it combines the two
	// directions exactly as overall coverage does, so the combined
	// unattributed share always equals 100% minus coverage.
	TracedWeight    float64
	Categories      []ConnectionsCategory
	UnattributedPct float64 // 0-100
	FanoutCapped    bool
	HopLimitReached bool
}

type ConnectionsCategory struct {
	Category string
	Pct      float64 // 0-100 of this direction's traced value
}

// minListedPct is the share below which a category moves to the
// "less than 0.1%" list.
const minListedPct = 0.1

// Connections renders a compact, chat-sized summary: inbound and outbound
// exposure combined into one list of connection shares.
//
// The two directions are combined weighted by traced path weight, the same
// weighting overall coverage uses, so the list and the coverage line agree.
// What this summary must not lose, however compact it gets, is everything
// SPEC.md §7 makes mandatory: the unattributed share (unknown, not clean),
// coverage, the low-confidence warning, and a direct listing or sanctions hit
// on the address itself.
func Connections(in ConnectionsInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "🔵 Address: %s\n\n", in.Address)
	fmt.Fprintf(&b, "⛓ Blockchain: %s\n\n", chainName(in.Chain))

	if in.OwnLabel != nil {
		name := in.OwnLabel.Entity
		if name == "" {
			name = displayCategory(in.OwnLabel.Category)
		}
		fmt.Fprintf(&b, "🚫 This address is directly listed: %s (%s)\n\n",
			name, displayCategory(in.OwnLabel.Category))
	}
	if in.SanctionsOverride {
		b.WriteString("🚫 Direct sanctions match. Risk is High regardless of score.\n\n")
	}

	shares, unattributed, total := combine(in.Inbound, in.Outbound)

	if total <= 0 {
		b.WriteString("Connections of the address:\n\n  •   No traced value\n\n")
	} else {
		var listed, minor []ConnectionsCategory
		for _, s := range shares {
			if s.Pct >= minListedPct {
				listed = append(listed, s)
			} else if s.Pct > 0 {
				minor = append(minor, s)
			}
		}

		b.WriteString("Connections of the address:\n\n")
		for _, s := range listed {
			fmt.Fprintf(&b, "  •   %s - %.1f%%\n", displayCategory(s.Category), s.Pct)
		}
		if unattributed > 0 {
			fmt.Fprintf(&b, "  •   Unattributed (unknown, not clean) - %.1f%%\n", unattributed)
		}
		if len(minor) > 0 {
			b.WriteString("\nLess than 0.1%:\n\n")
			for _, s := range minor {
				fmt.Fprintf(&b, "  •   %s\n", displayCategory(s.Category))
			}
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "📈 Risk level: %s (%.1f / 100)\n", titleCase(in.Band), in.Score)
	fmt.Fprintf(&b, "🎯 Coverage: %.1f%%\n", in.Coverage*100)

	if in.LowConfidence {
		fmt.Fprintf(&b, "\n⚠️ Low confidence: only %.1f%% of traced value reached a known entity. "+
			"The rest is unknown, not clean.\n", in.Coverage*100)
	}
	if in.BandCappedByAbuse {
		b.WriteString("\n⚠️ Band capped: the only evidence is unverified abuse reports.\n")
	}
	if truncated(in.Inbound) || truncated(in.Outbound) {
		b.WriteString("\nℹ️ Traversal was truncated; value beyond the limit is unknown.\n")
	}
	if in.Disclaimer != "" {
		fmt.Fprintf(&b, "\n%s\n", in.Disclaimer)
	}
	return b.String()
}

// combine merges the two directions into one set of shares of total traced
// value, sorted largest first with ties broken by name so output is stable.
func combine(dirs ...*ConnectionsDirection) ([]ConnectionsCategory, float64, float64) {
	var total float64
	for _, d := range dirs {
		if d != nil && d.TracedWeight > 0 {
			total += d.TracedWeight
		}
	}
	if total <= 0 {
		return nil, 0, 0
	}

	byCategory := map[string]float64{}
	var unattributed float64
	for _, d := range dirs {
		if d == nil || d.TracedWeight <= 0 {
			continue
		}
		w := d.TracedWeight / total
		for _, c := range d.Categories {
			byCategory[c.Category] += c.Pct * w
		}
		unattributed += d.UnattributedPct * w
	}

	out := make([]ConnectionsCategory, 0, len(byCategory))
	for c, pct := range byCategory {
		out = append(out, ConnectionsCategory{Category: c, Pct: pct})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pct != out[j].Pct {
			return out[i].Pct > out[j].Pct
		}
		return out[i].Category < out[j].Category
	})
	return out, unattributed, total
}

func truncated(d *ConnectionsDirection) bool {
	return d != nil && (d.FanoutCapped || d.HopLimitReached)
}

var categoryNames = map[string]string{
	"sanctions":           "Sanctions",
	"terrorist_financing": "Terrorist Financing",
	"darknet":             "Darknet Market",
	"stolen_funds":        "Stolen Funds",
	"mixer":               "Mixer",
	"scam":                "Scam",
	"high_risk_exchange":  "High-Risk Exchange",
	"gambling":            "Gambling",
	"unnamed_service":     "Unnamed service",
	"dust":                "Dust",
	"dex":                 "DEX",
	"exchange":            "Exchange",
}

func displayCategory(c string) string {
	if n, ok := categoryNames[c]; ok {
		return n
	}
	return titleCase(strings.ReplaceAll(c, "_", " "))
}

var chainNames = map[string]string{
	"tron":     "Tron (TRX)",
	"ethereum": "Ethereum (ETH)",
	"bsc":      "BNB Smart Chain (BSC)",
}

func chainName(c string) string {
	if n, ok := chainNames[c]; ok {
		return n
	}
	return c
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}
