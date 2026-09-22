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

	// Activity is the address's own history; nil when not available.
	Activity *ConnectionsActivity

	// Depth is how far stored history reached; nil when not computed.
	Depth *ConnectionsDepth

	Disclaimer string
}

// ConnectionsActivity is what the address itself did, before attribution.
type ConnectionsActivity struct {
	InUSD, OutUSD                       float64
	InTransfers, OutTransfers           uint64
	InCounterparties, OutCounterparties uint64
	FirstSeen, LastSeen                 string // YYYY-MM-DD, empty if unknown
	Assets                              []ConnectionsAsset
	UnpricedTransfers, UnpricedTokens   uint64
}

type ConnectionsAsset struct {
	Asset string
	USD   float64 // in plus out
}

// ConnectionsDepth says whether tracing had finished when this was scored.
type ConnectionsDepth struct {
	FrontierPending     int // dead ends not yet fetched
	FrontierQueued      int // of those, queued by this screen
	FetchError          string
	StillFetching       bool
	HistoryTruncated    bool
	Counterparties      int // queued for tracing, most active first
	Traced              int
	TotalCounterparties int
}

// ConnectionsEntry is one identified counterparty.
type ConnectionsEntry struct {
	Address  string
	Entity   string
	Category string
	Pct      float64 // 0-100 of its direction's traced value
	MinHops  int
}

// ConnectionsReason is part of the unattributed share with one cause.
type ConnectionsReason struct {
	Reason string
	Pct    float64 // 0-100 of its direction's traced value
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

	Entries []ConnectionsEntry
	Reasons []ConnectionsReason
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

	if a := in.Activity; a != nil && (a.InTransfers+a.OutTransfers+a.UnpricedTransfers) > 0 {
		b.WriteString("📊 Activity\n\n")
		fmt.Fprintf(&b, "  •   Received: %s in %s from %s\n",
			usd(a.InUSD), plural(a.InTransfers, "transfer"), plural(a.InCounterparties, "address"))
		fmt.Fprintf(&b, "  •   Sent: %s in %s to %s\n",
			usd(a.OutUSD), plural(a.OutTransfers, "transfer"), plural(a.OutCounterparties, "address"))
		if a.FirstSeen != "" {
			fmt.Fprintf(&b, "  •   Active: %s → %s\n", a.FirstSeen, a.LastSeen)
		}
		if len(a.Assets) > 0 {
			parts := make([]string, 0, len(a.Assets))
			for _, as := range a.Assets {
				parts = append(parts, as.Asset+" "+usd(as.USD))
			}
			fmt.Fprintf(&b, "  •   Assets: %s\n", strings.Join(parts, " · "))
		}
		if a.UnpricedTransfers > 0 {
			fmt.Fprintf(&b, "  •   Unrecognised tokens: %s of %s, not valued (typical of spam and airdrops)\n",
				plural(a.UnpricedTransfers, "transfer"), plural(a.UnpricedTokens, "token"))
		}
		b.WriteString("\n")
	}

	writeDepth(&b, in.Depth)

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
			for _, r := range combineReasons(in.Inbound, in.Outbound) {
				if r.Pct >= minListedPct {
					fmt.Fprintf(&b, "        ◦ %s - %.1f%%\n", reasonText(r.Reason), r.Pct)
				}
			}
		}
		if len(minor) > 0 {
			b.WriteString("\nLess than 0.1%:\n\n")
			for _, s := range minor {
				fmt.Fprintf(&b, "  •   %s\n", displayCategory(s.Category))
			}
		}
		b.WriteString("\n")

		writeEntries(&b, in)
		writeChecks(&b, in, shares)
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

// writeEntries lists identified counterparties per direction, with an
// approximate dollar figure: the share times that direction's volume, which
// is how proportional (haircut) attribution allocates value.
func writeEntries(b *strings.Builder, in ConnectionsInput) {
	type side struct {
		title  string
		d      *ConnectionsDirection
		volume float64
	}
	var inVol, outVol float64
	if in.Activity != nil {
		inVol, outVol = in.Activity.InUSD, in.Activity.OutUSD
	}
	sides := []side{{"⬅️ Inbound (funds came from)", in.Inbound, inVol}, {"➡️ Outbound (funds went to)", in.Outbound, outVol}}

	var any bool
	for _, sd := range sides {
		if sd.d != nil && len(sd.d.Entries) > 0 {
			any = true
		}
	}
	if !any {
		return
	}

	b.WriteString("🏷 Identified connections\n")
	for _, sd := range sides {
		if sd.d == nil || len(sd.d.Entries) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n  %s\n", sd.title)
		for i, e := range sd.d.Entries {
			if i >= 5 {
				fmt.Fprintf(b, "    … and %d more\n", len(sd.d.Entries)-5)
				break
			}
			name := e.Entity
			if name == "" {
				name = displayCategory(e.Category)
			}
			amount := ""
			if sd.volume > 0 {
				amount = " ≈ " + usd(e.Pct/100*sd.volume)
			}
			share := fmt.Sprintf("%.1f%%", e.Pct)
			if e.Pct < minListedPct {
				share = "under 0.1%"
			}
			fmt.Fprintf(b, "    %d. %s (%s)\n       %s · %s%s · %s\n",
				i+1, name, shortAddress(e.Address), displayCategory(e.Category), share, amount, hops(e.MinHops))
		}
	}
	b.WriteString("\n")
}

// riskChecks are the categories a reader looks for first. Each is reported
// found or not found, never silently omitted.
var riskChecks = []string{
	"sanctions", "terrorist_financing", "darknet", "stolen_funds",
	"mixer", "scam", "high_risk_exchange", "gambling",
}

func writeChecks(b *strings.Builder, in ConnectionsInput, shares []ConnectionsCategory) {
	found := map[string]float64{}
	for _, s := range shares {
		found[s.Category] = s.Pct
	}
	// "Not found" in a low-coverage result is not a clean bill, so it gets a
	// neutral mark rather than a tick.
	clear := "✅"
	if in.LowConfidence {
		clear = "⚪"
	}

	b.WriteString("🛡 Risk checks\n\n")
	for _, c := range riskChecks {
		if pct, ok := found[c]; ok && pct > 0 {
			share := fmt.Sprintf("%.1f%%", pct)
			if pct < minListedPct {
				share = "under 0.1%"
			}
			fmt.Fprintf(b, "  🔴  %s - found, %s\n", displayCategory(c), share)
		} else {
			fmt.Fprintf(b, "  %s  %s - not found\n", clear, displayCategory(c))
		}
	}
	fmt.Fprintf(b, "\n  Checks cover the %.1f%% of traced value that could be attributed.\n\n", in.Coverage*100)
}

func combineReasons(dirs ...*ConnectionsDirection) []ConnectionsReason {
	var total float64
	for _, d := range dirs {
		if d != nil && d.TracedWeight > 0 {
			total += d.TracedWeight
		}
	}
	if total <= 0 {
		return nil
	}
	by := map[string]float64{}
	var order []string
	for _, d := range dirs {
		if d == nil || d.TracedWeight <= 0 {
			continue
		}
		w := d.TracedWeight / total
		for _, r := range d.Reasons {
			if _, ok := by[r.Reason]; !ok {
				order = append(order, r.Reason)
			}
			by[r.Reason] += r.Pct * w
		}
	}
	out := make([]ConnectionsReason, 0, len(order))
	for _, r := range order {
		out = append(out, ConnectionsReason{Reason: r, Pct: by[r]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Pct > out[j].Pct })
	return out
}

func reasonText(r string) string {
	switch r {
	case "dead_end":
		return "trail stops (no stored history beyond this point)"
	case "hop_limit":
		return "beyond the hop limit"
	case "fanout_cap":
		return "too many counterparties to follow"
	case "unlabelled_category":
		return "labelled, but without a category"
	}
	return strings.ReplaceAll(r, "_", " ")
}

func hops(n int) string {
	switch n {
	case 0, 1:
		return "direct"
	default:
		return fmt.Sprintf("%d hops away", n)
	}
}

func shortAddress(a string) string {
	if len(a) <= 12 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
}

func plural(n uint64, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if strings.HasSuffix(word, "ss") {
		return fmt.Sprintf("%d %ses", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// usd formats a dollar amount compactly.
func usd(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("$%.2fB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("$%.2fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("$%.1fk", v/1e3)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}

// writeDepth says whether this is a finished answer. A result scored while
// counterparties are still being fetched is shallower than the one that will
// follow, and must not read as final.
func writeDepth(b *strings.Builder, d *ConnectionsDepth) {
	if d == nil {
		return
	}
	if d.FetchError != "" {
		fmt.Fprintf(b, "⚠️ Could not refresh this address from the chain, so stored data was used: %s\n\n", d.FetchError)
	}
	if d.StillFetching {
		b.WriteString("🔄 This address's history is still being fetched. " +
			"The figures below are partial; screen again in a few minutes.\n\n")
	}
	if d.HistoryTruncated {
		b.WriteString("ℹ️ This address has more history than the per-address fetch limit " +
			"(10,000 transfers); activity figures cover the most recent part.\n\n")
	}
	if d.FrontierPending > 0 {
		queued := fmt.Sprintf("%d", d.FrontierQueued)
		if d.FrontierQueued < d.FrontierPending {
			queued = fmt.Sprintf("%d of %d", d.FrontierQueued, d.FrontierPending)
		}
		fmt.Fprintf(b, "🔭 Tracing further: %s addresses where the trail stops are queued. "+
			"Screen again later for a deeper result.\n", queued)
	}
	if d.Counterparties == 0 {
		if d.FrontierPending > 0 {
			b.WriteString("\n")
		}
		return
	}
	if d.Traced < d.Counterparties {
		fmt.Fprintf(b, "🔄 Tracing in progress: %d of %d counterparties traced. "+
			"Screen again later for a deeper result.\n", d.Traced, d.Counterparties)
	} else {
		fmt.Fprintf(b, "🔎 Counterparties traced: %d of %d.\n", d.Traced, d.Counterparties)
	}
	if d.TotalCounterparties > d.Counterparties {
		fmt.Fprintf(b, "   The %d most active of %d counterparties are traced.\n",
			d.Counterparties, d.TotalCounterparties)
	}
	b.WriteString("\n")
}
