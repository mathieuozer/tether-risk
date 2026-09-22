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

	// Lang is "en", "tr" or "ru"; anything else renders English.
	Lang string

	// Flags are behaviour notes; shown, never scored.
	Flags []ConnectionsFlag

	// Verdict is the three-state answer (D29); nil when not computed.
	Verdict *ConnectionsVerdict
}

// ConnectionsVerdict is the answer to "is this wallet clean?".
type ConnectionsVerdict struct {
	Level         string // clear, caution, high_risk
	Confidence    string // high, medium, low
	ConfidencePct int    // 1-99
	Insufficient  bool   // not risky, but too little data to lean on
	Reasons       []ConnectionsVerdictReason
}

type ConnectionsVerdictReason struct {
	Code, Category, Flag, Address string
	Pct                           float64
}

// ConnectionsFlag is one behaviour note.
type ConnectionsFlag struct {
	Code                                string
	InUSD, OutUSD, VolumeUSD, AmountUSD float64
	Days, AgeDays, Count, Minutes       int
	Address                             string // poisoning_target: an imitated address; frozen_contact: the frozen wallet
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
	FrontierPending int // dead ends not yet fetched
	FrontierQueued  int // of those, queued by this screen
	// FollowUp means the caller keeps tracing in the background and will
	// send the final result, so the reader is not told to screen again.
	FollowUp            bool
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

	// Profile is the counterparty's own stored activity, set for unnamed
	// services; nil otherwise.
	Profile *ConnectionsProfile
}

// ConnectionsProfile is what an unnamed service did, which is how it is
// judged when there is no name to go on.
type ConnectionsProfile struct {
	VolumeUSD           float64
	Transfers           uint64
	Counterparties      uint64
	FirstSeen, LastSeen string // YYYY-MM-DD
	Assets              []string
	Partial             bool // older history beyond the fetch limit is missing
}

// ConnectionsReason is part of the unattributed share with one cause.
type ConnectionsReason struct {
	Reason string
	Pct    float64 // 0-100 of its direction's traced value
}

type ConnectionsOwnLabel struct {
	Entity   string
	Category string
	Imitates string // an address-poisoning sender's look-alike target
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
	l := newLoc(in.Lang)

	b.WriteString(l.f("address", in.Address))
	b.WriteString(l.f("chain", chainName(in.Chain)))
	writeVerdict(&b, in.Verdict, l)

	if in.OwnLabel != nil {
		name := in.OwnLabel.Entity
		if in.OwnLabel.Imitates != "" {
			name = l.f("poisoning_entity", shortAddress(in.OwnLabel.Imitates))
		}
		if name == "" {
			name = l.category(in.OwnLabel.Category)
		}
		b.WriteString(l.f("listed", name, l.category(in.OwnLabel.Category)))
	}
	if in.SanctionsOverride {
		// The override fires for any always-wins category; only a sanctions
		// listing is described as one.
		if in.OwnLabel != nil && in.OwnLabel.Category != "sanctions" {
			b.WriteString(l.f("direct_high", l.category(in.OwnLabel.Category)))
		} else {
			b.WriteString(l.f("sanctioned"))
		}
	}

	if a := in.Activity; a != nil && (a.InTransfers+a.OutTransfers+a.UnpricedTransfers) > 0 {
		b.WriteString(l.f("activity"))
		b.WriteString(l.f("received", usd(a.InUSD), l.n(a.InTransfers, "transfer"), l.n(a.InCounterparties, "address")))
		b.WriteString(l.f("sent", usd(a.OutUSD), l.n(a.OutTransfers, "transfer"), l.n(a.OutCounterparties, "address")))
		if a.FirstSeen != "" {
			b.WriteString(l.f("active", a.FirstSeen, a.LastSeen))
		}
		if len(a.Assets) > 0 {
			parts := make([]string, 0, len(a.Assets))
			for _, as := range a.Assets {
				parts = append(parts, as.Asset+" "+usd(as.USD))
			}
			b.WriteString(l.f("assets", strings.Join(parts, " · ")))
		}
		if a.UnpricedTransfers > 0 {
			b.WriteString(l.f("unpriced", l.n(a.UnpricedTransfers, "transfer"), l.n(a.UnpricedTokens, "token")))
		}
		b.WriteString("\n")
	}

	writeDepth(&b, in.Depth, l)

	shares, unattributed, total := combine(in.Inbound, in.Outbound)

	if total <= 0 {
		b.WriteString(l.f("connections") + l.f("no_value"))
	} else {
		var listed, minor []ConnectionsCategory
		for _, s := range shares {
			if s.Pct >= minListedPct {
				listed = append(listed, s)
			} else if s.Pct > 0 {
				minor = append(minor, s)
			}
		}

		b.WriteString(l.f("connections"))
		for _, s := range listed {
			fmt.Fprintf(&b, "  •   %s - %s\n", l.category(s.Category), l.pct(s.Pct))
		}
		if unattributed > 0 {
			b.WriteString(l.f("unattributed", l.pct(unattributed)))
			for _, r := range combineReasons(in.Inbound, in.Outbound) {
				if r.Pct >= minListedPct {
					fmt.Fprintf(&b, "        ◦ %s - %s\n", reasonText(l, r.Reason), l.pct(r.Pct))
				}
			}
		}
		// Every category is accounted for, so a missing line cannot be read
		// as "not checked". Those found in trace amounts come first, then
		// those not found at all, in one list as other screeners lay it out.
		small := make([]string, 0, len(minor)+len(categoryOrder))
		for _, s := range minor {
			small = append(small, s.Category)
		}
		small = append(small, notFound(shares)...)
		if len(small) > 0 {
			b.WriteString(l.f("less_than"))
			for _, c := range small {
				fmt.Fprintf(&b, "  •   %s\n", l.category(c))
			}
		}
		b.WriteString("\n")

		writeEntries(&b, in, l)
		writeChecks(&b, in, shares, l)
	}
	writeFlags(&b, in.Flags, l)

	key := "risk_level"
	if in.Verdict != nil {
		key = "risk_level_verdict"
	}
	b.WriteString(l.f(key, l.band(in.Band), in.Score))
	b.WriteString(l.f("coverage", l.pct(in.Coverage*100)))

	if in.LowConfidence {
		b.WriteString(l.f("low_confidence", l.pct(in.Coverage*100)))
	}
	if in.BandCappedByAbuse {
		b.WriteString(l.f("band_capped"))
	}
	if truncated(in.Inbound) || truncated(in.Outbound) {
		b.WriteString(l.f("truncated"))
	}
	if in.Disclaimer != "" {
		// The API's disclaimer is English; its meaning, not its wording, is
		// what must reach the reader.
		disclaimer := in.Disclaimer
		if l.lang != "en" {
			disclaimer = l.f("disclaimer")
		}
		fmt.Fprintf(&b, "\n%s\n", disclaimer)
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

// categoryOrder is every category in config/weights.yaml, highest weight
// first. A test keeps it in step with the config.
var categoryOrder = []string{
	"sanctions", "terrorist_financing", "darknet", "stolen_funds", "frozen_funds", "mixer", "scam",
	"high_risk_exchange", "gambling", "named_service", "unnamed_service", "dust", "dex", "exchange",
}

// notFound is every category with no traced value, in categoryOrder.
func notFound(shares []ConnectionsCategory) []string {
	seen := map[string]bool{}
	for _, s := range shares {
		if s.Pct > 0 {
			seen[s.Category] = true
		}
	}
	var out []string
	for _, c := range categoryOrder {
		if !seen[c] {
			out = append(out, c)
		}
	}
	return out
}

var categoryNames = map[string]string{
	"sanctions":           "Sanctions",
	"terrorist_financing": "Terrorist Financing",
	"darknet":             "Darknet Market",
	"stolen_funds":        "Stolen Funds",
	"frozen_funds":        "Frozen by Tether",
	"mixer":               "Mixer",
	"scam":                "Scam",
	"high_risk_exchange":  "High-Risk Exchange",
	"gambling":            "Gambling",
	"named_service":       "Named service",
	"unnamed_service":     "Unnamed service",
	"dust":                "Dust",
	"dex":                 "DEX",
	"exchange":            "Exchange",
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
func writeEntries(b *strings.Builder, in ConnectionsInput, l loc) {
	type side struct {
		title  string
		d      *ConnectionsDirection
		volume float64
	}
	var inVol, outVol float64
	if in.Activity != nil {
		inVol, outVol = in.Activity.InUSD, in.Activity.OutUSD
	}
	sides := []side{{l.f("inbound"), in.Inbound, inVol}, {l.f("outbound"), in.Outbound, outVol}}

	var any bool
	for _, sd := range sides {
		if sd.d != nil && len(sd.d.Entries) > 0 {
			any = true
		}
	}
	if !any {
		return
	}

	b.WriteString(l.f("identified"))
	for _, sd := range sides {
		if sd.d == nil || len(sd.d.Entries) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n  %s\n", sd.title)
		for i, e := range riskFirst(sd.d.Entries) {
			if i >= 5 {
				b.WriteString(l.f("and_more", len(sd.d.Entries)-5))
				break
			}
			name := entryName(l, e)
			amount := ""
			if sd.volume > 0 {
				amount = " ≈ " + usd(e.Pct/100*sd.volume)
			}
			share := l.pct(e.Pct)
			if e.Pct < minListedPct {
				share = l.f("under")
			}
			fmt.Fprintf(b, "    %d. %s (%s)\n       %s · %s%s · %s\n",
				i+1, name, shortAddress(e.Address), l.category(e.Category), share, amount, hops(l, e.MinHops))
			if p := e.Profile; p != nil {
				fmt.Fprintf(b, "       %s\n", profileText(l, p))
			}
		}
	}
	b.WriteString("\n")
}

// riskFirst orders entries so every risk connection comes before the rest,
// each group largest first. Only five are shown, and a 3% frozen connection
// used to sit behind five large services, out of sight, while the verdict
// named it (docs/DECISIONS.md D30).
func riskFirst(entries []ConnectionsEntry) []ConnectionsEntry {
	isRisk := map[string]bool{}
	for _, c := range riskChecks {
		isRisk[c] = true
	}
	out := make([]ConnectionsEntry, 0, len(entries))
	for _, e := range entries {
		if isRisk[e.Category] {
			out = append(out, e)
		}
	}
	for _, e := range entries {
		if !isRisk[e.Category] {
			out = append(out, e)
		}
	}
	return out
}

// riskChecks are the categories a reader looks for first. Each is reported
// found or not found, never silently omitted.
var riskChecks = []string{
	"sanctions", "terrorist_financing", "darknet", "stolen_funds", "frozen_funds",
	"mixer", "scam", "high_risk_exchange", "gambling",
}

func writeChecks(b *strings.Builder, in ConnectionsInput, shares []ConnectionsCategory, l loc) {
	found := map[string]float64{}
	for _, s := range shares {
		found[s.Category] = s.Pct
	}
	// "Not found" in a low-coverage result is not a clean bill, so it gets a
	// neutral mark rather than a tick.
	clear := "✅"
	unseen := unidentifiedPct(in.Verdict)
	if in.LowConfidence || unseen > 0 {
		clear = "⚪"
	}

	b.WriteString(l.f("checks"))
	for _, c := range riskChecks {
		if pct, ok := found[c]; ok && pct > 0 {
			share := l.pct(pct)
			if pct < minListedPct {
				share = l.f("under")
			}
			b.WriteString(l.f("found", l.category(c), share))
		} else {
			b.WriteString(l.f("not_found", clear, l.category(c)))
		}
	}
	cover := l.f("checks_cover", l.pct(in.Coverage*100))
	if unseen > 0 {
		cover = strings.TrimSuffix(cover, "\n") + l.f("checks_unseen", l.pct(unseen))
	}
	b.WriteString(cover)
}

// unidentifiedPct is the share of traced value ending at unnamed services
// when the verdict says it is too much to vouch for, else 0.
func unidentifiedPct(v *ConnectionsVerdict) float64 {
	if v == nil {
		return 0
	}
	for _, r := range v.Reasons {
		if r.Code == "unidentified" {
			return r.Pct
		}
	}
	return 0
}

// writeVerdict puts the answer first: risky or not risky, the confidence as
// a percentage, and a line or two on why, so a reader who stops there still
// has it. The owner asked for exactly this shape.
func writeVerdict(b *strings.Builder, v *ConnectionsVerdict, l loc) {
	if v == nil {
		return
	}
	head := l.f("v_"+v.Level, v.ConfidencePct)
	if v.Insufficient {
		head = strings.TrimSuffix(head, "\n") + l.f("v_insufficient")
	}
	b.WriteString(head)
	b.WriteString(whyText(v, l))
	b.WriteString("\n")
}

// whyText is the verdict in plain words, one short paragraph, for a reader
// who is not an analyst: what decided it, and what that means for them.
func whyText(v *ConnectionsVerdict, l loc) string {
	// A listing or a poisoning is the whole answer; anything after it
	// would read as if it mattered as much.
	if len(v.Reasons) > 0 && v.Level == "high_risk" {
		switch r := v.Reasons[0]; r.Code {
		case "poisoning":
			return l.f("why_poisoning_verdict", shortAddress(r.Address))
		case "own_listed":
			return l.f("why_high_risk", l.f("why_own_listed", l.category(r.Category)))
		}
	}
	var parts []string
	seen := map[string]bool{}
	for _, r := range v.Reasons {
		var p string
		switch r.Code {
		case "own_listed":
			p = l.f("why_own_listed", l.category(r.Category))
		case "exposure", "exposure_minor":
			p = l.f("why_exposure", l.pct(r.Pct), l.category(r.Category))
		case "low_coverage", "unidentified":
			p = l.f("why_"+r.Code, l.pct(r.Pct))
		case "behaviour":
			p = l.f("why_" + r.Flag)
		case "band_high", "band_medium", "tracing_incomplete":
			p = l.f("why_" + r.Code)
		}
		if p != "" && !seen[p] {
			seen[p] = true
			parts = append(parts, p)
		}
		if len(parts) == 2 {
			break
		}
	}
	switch v.Level {
	case "clear":
		return l.f("why_clear")
	case "high_risk":
		return l.f("why_high_risk", strings.Join(parts, "; "))
	default:
		return l.f("why_caution", strings.Join(parts, "; "))
	}
}

// writeFlags lists behaviour notes. They describe what the address did and
// say plainly that they are not part of the score.
func writeFlags(b *strings.Builder, flags []ConnectionsFlag, l loc) {
	if len(flags) == 0 {
		return
	}
	b.WriteString(l.f("flags"))
	for _, f := range flags {
		switch f.Code {
		case "pass_through":
			b.WriteString(l.f("flag_pass_through", usd(f.InUSD), usd(f.OutUSD), f.Days))
		case "high_volume_new":
			if f.AgeDays < 1 {
				b.WriteString(l.f("flag_high_volume_new_today", usd(f.VolumeUSD)))
			} else {
				b.WriteString(l.f("flag_high_volume_new", usd(f.VolumeUSD), f.AgeDays))
			}
		case "new_address":
			b.WriteString(l.f("flag_new_address", f.AgeDays))
		case "round_split":
			b.WriteString(l.f("flag_round_split", usd(f.AmountUSD), f.Count, f.Minutes))
		case "parked_funds":
			b.WriteString(l.f("flag_parked_funds", f.Count, usd(f.AmountUSD)))
		case "poisoning_target":
			b.WriteString(l.f("flag_poisoning_target", f.Count, shortAddress(f.Address)))
		case "frozen_contact":
			b.WriteString(l.f("flag_frozen_contact", shortAddress(f.Address), usd(f.AmountUSD), f.Count))
		}
	}
	b.WriteString("\n")
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

func reasonText(l loc, r string) string {
	switch r {
	case "dead_end":
		return l.f("r_dead_end")
	case "hop_limit":
		return l.f("r_hop_limit")
	case "fanout_cap":
		return l.f("r_fanout_cap")
	case "unlabelled_category":
		return l.f("r_unlabelled")
	}
	return strings.ReplaceAll(r, "_", " ")
}

// entryName is how a counterparty is named in the list. The service
// detector's labels read "Unidentified high-volume service"; the category
// line already says the operator is unnamed, and the profile says what the
// service is, so the name says only what kind of wallet it is.
func entryName(l loc, e ConnectionsEntry) string {
	if e.Entity == "" {
		return l.category(e.Category)
	}
	if e.Entity == "Unidentified high-volume service" {
		return l.f("service")
	}
	if rest, ok := strings.CutPrefix(e.Entity, "Unidentified "); ok && rest != "" {
		return strings.ToUpper(rest[:1]) + rest[1:]
	}
	return e.Entity
}

// profileText describes an unnamed service by what it did: how much it moved,
// with how many addresses, when, and in what.
func profileText(l loc, p *ConnectionsProfile) string {
	out := l.f("profile", usd(p.VolumeUSD), l.n(p.Counterparties, "address"), l.n(p.Transfers, "stored transfer"))
	if p.Partial {
		out += l.f("partial")
	}
	if p.FirstSeen != "" {
		out += fmt.Sprintf(" · %s → %s", p.FirstSeen, p.LastSeen)
	}
	if len(p.Assets) > 0 {
		out += " · " + strings.Join(p.Assets, ", ")
	}
	return out
}

func hops(l loc, n int) string {
	switch n {
	case 0, 1:
		return l.f("direct")
	default:
		return l.f("hops_away", n)
	}
}

func shortAddress(a string) string {
	if len(a) <= 12 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
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
func writeDepth(b *strings.Builder, d *ConnectionsDepth, l loc) {
	if d == nil {
		return
	}
	if d.FetchError != "" {
		b.WriteString(l.f("fetch_error", d.FetchError))
	}
	later := ""
	if d.FollowUp {
		later = "_followup"
	}
	if d.StillFetching {
		b.WriteString(l.f("still_fetching" + later))
	}
	if d.HistoryTruncated {
		b.WriteString(l.f("history_limit"))
	}
	if d.FrontierPending > 0 {
		queued := fmt.Sprintf("%d", d.FrontierQueued)
		if d.FrontierQueued < d.FrontierPending {
			queued = l.f("queued_of", d.FrontierQueued, d.FrontierPending)
		}
		b.WriteString(l.f("frontier"+later, queued))
	}
	if d.Counterparties == 0 {
		if d.FrontierPending > 0 {
			b.WriteString("\n")
		}
		return
	}
	if d.Traced < d.Counterparties {
		b.WriteString(l.f("tracing"+later, d.Traced, d.Counterparties))
	} else {
		b.WriteString(l.f("traced", d.Traced, d.Counterparties))
	}
	if d.TotalCounterparties > d.Counterparties {
		b.WriteString(l.f("most_active", d.Counterparties, d.TotalCounterparties))
	}
	b.WriteString("\n")
}

// VerdictBlock is the verdict headline and its why lines alone, for places
// with room for nothing else, such as a message sent inline into a group.
func VerdictBlock(v *ConnectionsVerdict, lang string) string {
	var b strings.Builder
	writeVerdict(&b, v, newLoc(lang))
	return b.String()
}
