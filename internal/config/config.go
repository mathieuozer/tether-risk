// Package config loads and validates the versioned YAML that drives scoring.
//
// SPEC.md §2 requires that category weights, hop limits, decay factors and
// thresholds live in configuration and never inline in Go, and that identical
// input plus an identical label snapshot produces an identical score. The
// Version returned here is the SHA-256 of the configuration bytes and is
// stamped on every result so a score can always be tied back to the exact
// settings that produced it.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the fully validated configuration set.
type Config struct {
	Weights Weights
	Sources Sources

	// Version is the SHA-256 over both files' bytes. Stamped on every result.
	Version string
}

// ---------------------------------------------------------------------------
// weights.yaml
// ---------------------------------------------------------------------------

type Weights struct {
	Version    int                 `yaml:"version"`
	Categories map[string]Category `yaml:"categories"`
	Bands      []Band              `yaml:"bands"`

	SanctionsOverrideBand string `yaml:"sanctions_override_band"`

	Traversal        Traversal        `yaml:"traversal"`
	Dust             Dust             `yaml:"dust"`
	Coverage         Coverage         `yaml:"coverage"`
	Labels           LabelRules       `yaml:"labels"`
	DerivedDeposit   DerivedDeposit   `yaml:"derived_deposit"`
	DerivedHotWallet DerivedHotWallet `yaml:"derived_hotwallet"`
	Behaviour        Behaviour        `yaml:"behaviour"`
	Verdict          Verdict          `yaml:"verdict"`
	DerivedService   DerivedService   `yaml:"derived_service"`
	Pricing          Pricing          `yaml:"pricing"`
}

// DerivedService configures behavioural service detection. The evidence
// proves an address is a service, not which one, so matches are labelled
// unnamed_service rather than exchange.
type DerivedService struct {
	Enabled             bool    `yaml:"enabled"`
	SamplePages         int     `yaml:"sample_pages"`
	PageSize            int     `yaml:"page_size"`
	MinTransfersSampled int     `yaml:"min_transfers_sampled"`
	MinCounterparties   int     `yaml:"min_counterparties"`
	RequireMorePages    bool    `yaml:"require_more_pages"`
	Confidence          float64 `yaml:"confidence"`
	MaxCandidates       int     `yaml:"max_candidates"`
	Stored              struct {
		MinTransfers         int     `yaml:"min_transfers"`
		MinCounterparties    int     `yaml:"min_counterparties"`
		MinCounterpartyRatio float64 `yaml:"min_counterparty_ratio"`
		MinActiveDays        int     `yaml:"min_active_days"`
	} `yaml:"stored"`
}

// Pricing controls how raw amounts become USD values. docs/PLAN.md F3.
type Pricing struct {
	// Pinned holds assets held at a fixed USD value, i.e. stablecoins.
	Pinned map[string]float64 `yaml:"pinned"`
	// DailyClose lists assets valued from the daily close series.
	DailyClose []string `yaml:"daily_close"`
}

type Category struct {
	Weight      float64 `yaml:"weight"`
	Description string  `yaml:"description"`
}

type Band struct {
	Name string  `yaml:"name"`
	Min  float64 `yaml:"min"`
	Max  float64 `yaml:"max"`
}

type Traversal struct {
	MaxHops              int     `yaml:"max_hops"`
	Decay                float64 `yaml:"decay"`
	MaxNeighboursPerNode int     `yaml:"max_neighbours_per_node"`
	TerminateAtLabelled  bool    `yaml:"terminate_at_labelled"`
	MinContribution      float64 `yaml:"min_contribution"`
}

type Dust struct {
	USDThreshold float64 `yaml:"usd_threshold"`
	InboundOnly  bool    `yaml:"inbound_only"`
}

type Coverage struct {
	LowConfidenceThreshold float64 `yaml:"low_confidence_threshold"`
}

type LabelRules struct {
	SourcePriority []string     `yaml:"source_priority"`
	AlwaysWins     []string     `yaml:"always_wins"`
	AbuseReports   AbuseReports `yaml:"abuse_reports"`
}

type AbuseReports struct {
	Sources               []string `yaml:"sources"`
	MinIndependentReports int      `yaml:"min_independent_reports"`
	CappedBand            string   `yaml:"capped_band"`
}

type DerivedDeposit struct {
	Enabled          bool    `yaml:"enabled"`
	MinTransfers     int     `yaml:"min_transfers"`
	MinOutboundShare float64 `yaml:"min_outbound_share"`
	MaxOtherShare    float64 `yaml:"max_other_share"`
	Confidence       float64 `yaml:"confidence"`

	// AnchorSources are label sources whose addresses anchor the heuristic
	// whatever their category: an exchange's own reserve list proves who
	// controls an address without saying anything about its KYC tier.
	AnchorSources []string `yaml:"anchor_sources"`

	// FetchCandidates caps how many unfetched candidates one run queues for
	// fetching. A candidate is judged on its whole outbound history, which
	// is unknown until it has been fetched.
	FetchCandidates int `yaml:"fetch_candidates"`
}

// Verdict configures the three-state answer (docs/DECISIONS.md D29).
type Verdict struct {
	RedExposure       map[string]float64 `yaml:"red_exposure"`
	ClearMinCoverage  float64            `yaml:"clear_min_coverage"`
	MaxPendingPct     float64            `yaml:"max_pending_pct"`
	MaxUnnamedPct     float64            `yaml:"max_unnamed_pct"`
	ConfidenceCredit  map[string]float64 `yaml:"confidence_credit"`
	BehaviourPenalty  float64            `yaml:"behaviour_penalty"`
	InsufficientBelow int                `yaml:"insufficient_below"`
	ConfidenceHigh    float64            `yaml:"confidence_high"`
	ConfidenceMedium  float64            `yaml:"confidence_medium"`
}

// Behaviour configures the unscored behaviour notes (docs/DECISIONS.md D28).
type Behaviour struct {
	PassThrough struct {
		MinVolumeUSD     float64 `yaml:"min_volume_usd"`
		MaxRetainedShare float64 `yaml:"max_retained_share"`
		MaxDays          int     `yaml:"max_days"`
	} `yaml:"pass_through"`
	NewAddress struct {
		MaxAgeDays    int     `yaml:"max_age_days"`
		HighVolumeUSD float64 `yaml:"high_volume_usd"`
	} `yaml:"new_address"`
	RoundSplit struct {
		MinAmountUSD  float64 `yaml:"min_amount_usd"`
		RoundToUSD    float64 `yaml:"round_to_usd"`
		MinRecipients int     `yaml:"min_recipients"`
		MaxMinutes    int     `yaml:"max_minutes"`
	} `yaml:"round_split"`
	Parked struct {
		MinAmountUSD float64 `yaml:"min_amount_usd"`
		MinWallets   int     `yaml:"min_wallets"`
		MinTotalUSD  float64 `yaml:"min_total_usd"`
	} `yaml:"parked"`
}

// DerivedHotWallet configures exchange hot-wallet detection from two-way
// flows with the exchange's own reserve wallets (docs/DECISIONS.md D28).
type DerivedHotWallet struct {
	Enabled             bool     `yaml:"enabled"`
	ReserveSources      []string `yaml:"reserve_sources"`
	MinTransfersEachWay int      `yaml:"min_transfers_each_way"`
	MinInflowUSD        float64  `yaml:"min_inflow_usd"`
	MinCounterparties   int      `yaml:"min_counterparties"`
	MinExclusiveShare   float64  `yaml:"min_exclusive_share"`
	Confidence          float64  `yaml:"confidence"`
}

// ---------------------------------------------------------------------------
// sources.yaml
// ---------------------------------------------------------------------------

type Sources struct {
	Version int           `yaml:"version"`
	Sources []LabelSource `yaml:"sources"`
	Chains  []ChainSource `yaml:"chains"`
}

// SourceStatus controls whether an ingester may run for a source.
type SourceStatus string

const (
	// StatusAllowed means the terms permit ingestion and redistribution.
	StatusAllowed SourceStatus = "allowed"
	// StatusBlocked means the terms prohibit it. SPEC.md §6.2 requires that
	// we stop rather than work around this; no ingester may run.
	StatusBlocked SourceStatus = "blocked"
	// StatusNeedsReview means a human has not yet read the terms.
	StatusNeedsReview SourceStatus = "needs_review"
	// StatusUnavailable means the terms are fine but we lack access.
	StatusUnavailable SourceStatus = "unavailable"
)

type LabelSource struct {
	ID             string       `yaml:"id"`
	Name           string       `yaml:"name"`
	Status         SourceStatus `yaml:"status"`
	Confidence     float64      `yaml:"confidence"`
	Schedule       string       `yaml:"schedule"`
	URL            string       `yaml:"url"`
	Path           string       `yaml:"path"`
	Format         string       `yaml:"format"`
	Licence        string       `yaml:"licence"`
	Redistribution string       `yaml:"redistribution"`
	Checked        string       `yaml:"checked"`
	BlockedReason  string       `yaml:"blocked_reason"`

	// UnavailableReason explains a source that is permitted but cannot
	// actually be used — no credentials, or the data is not what it appeared
	// to be. Distinct from BlockedReason, which means the terms forbid it.
	// The two failure modes call for different follow-up, so they are not
	// collapsed into one field.
	UnavailableReason string   `yaml:"unavailable_reason"`
	Substitutes       []string `yaml:"substitutes"`
	Notes             string   `yaml:"notes"`
}

// Ingestible reports whether an ingester is permitted to run for this source.
func (s LabelSource) Ingestible() bool { return s.Status == StatusAllowed }

type ChainSource struct {
	ID              string       `yaml:"id"`
	Status          SourceStatus `yaml:"status"`
	Adapter         string       `yaml:"adapter"`
	URL             string       `yaml:"url"`
	Auth            string       `yaml:"auth"`
	RateLimitPerSec int          `yaml:"rate_limit_per_sec"`
	// RateLimitPerSecWithKey applies when the chain's API key is set. Zero
	// means the key buys nothing and RateLimitPerSec applies either way.
	RateLimitPerSecWithKey int    `yaml:"rate_limit_per_sec_with_key"`
	Checked                string `yaml:"checked"`
	UnavailableReason      string `yaml:"unavailable_reason"`
	Notes                  string `yaml:"notes"`
}

// RequestRate is the request budget to pace at, given whether an API key is
// configured.
func (c ChainSource) RequestRate(hasKey bool) float64 {
	if hasKey && c.RateLimitPerSecWithKey > 0 {
		return float64(c.RateLimitPerSecWithKey)
	}
	return float64(c.RateLimitPerSec)
}

// Available reports whether the chain has a live data path.
func (c ChainSource) Available() bool { return c.Status == StatusAllowed }

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// Load reads weights.yaml and sources.yaml from dir, validates them, and
// computes the version stamp. It fails loudly: a configuration that cannot be
// trusted must not silently produce scores.
func Load(dir string) (*Config, error) {
	weightsRaw, err := os.ReadFile(filepath.Join(dir, "weights.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read weights.yaml: %w", err)
	}
	sourcesRaw, err := os.ReadFile(filepath.Join(dir, "sources.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read sources.yaml: %w", err)
	}

	var c Config

	dec := yaml.NewDecoder(strings.NewReader(string(weightsRaw)))
	dec.KnownFields(true) // a typo'd key must fail, not be silently ignored
	if err := dec.Decode(&c.Weights); err != nil {
		return nil, fmt.Errorf("parse weights.yaml: %w", err)
	}

	dec = yaml.NewDecoder(strings.NewReader(string(sourcesRaw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c.Sources); err != nil {
		return nil, fmt.Errorf("parse sources.yaml: %w", err)
	}

	// Version is order-independent over the two files.
	h := sha256.New()
	h.Write(weightsRaw)
	h.Write(sourcesRaw)
	c.Version = hex.EncodeToString(h.Sum(nil))[:16]

	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate enforces every invariant the scoring engine relies on.
func (c *Config) Validate() error {
	var errs []string
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	// --- categories ---
	if len(c.Weights.Categories) == 0 {
		bad("categories: must not be empty")
	}
	for name, cat := range c.Weights.Categories {
		if cat.Weight < 0 || cat.Weight > 100 {
			bad("categories.%s.weight: %v out of range 0-100", name, cat.Weight)
		}
		if cat.Description == "" {
			bad("categories.%s: description is required; an unexplained weight is not auditable", name)
		}
	}

	// --- bands must tile 0-100 with no gap or overlap ---
	if len(c.Weights.Bands) == 0 {
		bad("bands: must not be empty")
	} else {
		bands := append([]Band(nil), c.Weights.Bands...)
		sort.Slice(bands, func(i, j int) bool { return bands[i].Min < bands[j].Min })
		if bands[0].Min != 0 {
			bad("bands: must start at 0, starts at %v", bands[0].Min)
		}
		if last := bands[len(bands)-1]; last.Max != 100 {
			bad("bands: must end at 100, ends at %v", last.Max)
		}
		for i, b := range bands {
			if b.Name == "" {
				bad("bands[%d]: name is required", i)
			}
			if b.Max <= b.Min {
				bad("bands.%s: max %v must exceed min %v", b.Name, b.Max, b.Min)
			}
			if i > 0 && b.Min != bands[i-1].Max {
				bad("bands: gap or overlap between %s (max %v) and %s (min %v)",
					bands[i-1].Name, bands[i-1].Max, b.Name, b.Min)
			}
		}
	}

	if c.Weights.SanctionsOverrideBand == "" {
		bad("sanctions_override_band: required")
	} else if !c.hasBand(c.Weights.SanctionsOverrideBand) {
		bad("sanctions_override_band: %q is not a defined band", c.Weights.SanctionsOverrideBand)
	}

	// --- traversal ---
	t := c.Weights.Traversal
	if t.MaxHops < 1 {
		bad("traversal.max_hops: must be at least 1, got %d", t.MaxHops)
	}
	if t.Decay <= 0 || t.Decay > 1 {
		bad("traversal.decay: must be in (0,1], got %v", t.Decay)
	}
	if t.MaxNeighboursPerNode < 1 {
		bad("traversal.max_neighbours_per_node: must be at least 1, got %d", t.MaxNeighboursPerNode)
	}
	if t.MinContribution < 0 || t.MinContribution >= 1 {
		bad("traversal.min_contribution: must be in [0,1), got %v", t.MinContribution)
	}
	if !t.TerminateAtLabelled {
		// SPEC.md §7 is explicit that without this rule every score becomes
		// noise. Allowed only because tests need to disable it.
		bad("traversal.terminate_at_labelled: disabling this makes every score noise (SPEC.md §7); only tests may override it in memory")
	}

	// --- dust ---
	if c.Weights.Dust.USDThreshold < 0 {
		bad("dust.usd_threshold: must not be negative, got %v", c.Weights.Dust.USDThreshold)
	}

	// --- coverage ---
	if v := c.Weights.Coverage.LowConfidenceThreshold; v < 0 || v > 1 {
		bad("coverage.low_confidence_threshold: must be in [0,1], got %v", v)
	}

	// --- derived deposit ---
	d := c.Weights.DerivedDeposit
	if d.Enabled {
		if d.MinTransfers < 1 {
			bad("derived_deposit.min_transfers: must be at least 1, got %d", d.MinTransfers)
		}
		if d.MinOutboundShare <= 0 || d.MinOutboundShare > 1 {
			bad("derived_deposit.min_outbound_share: must be in (0,1], got %v", d.MinOutboundShare)
		}
		if d.MaxOtherShare < 0 || d.MaxOtherShare >= 1 {
			bad("derived_deposit.max_other_share: must be in [0,1), got %v", d.MaxOtherShare)
		}
		if d.Confidence <= 0 || d.Confidence > 1 {
			bad("derived_deposit.confidence: must be in (0,1], got %v", d.Confidence)
		}
		if d.FetchCandidates < 0 {
			bad("derived_deposit.fetch_candidates: must not be negative, got %d", d.FetchCandidates)
		}
	}

	// --- label rules reference real categories and real sources ---
	for _, cat := range c.Weights.Labels.AlwaysWins {
		if _, ok := c.Weights.Categories[cat]; !ok {
			bad("labels.always_wins: %q is not a defined category", cat)
		}
	}
	if b := c.Weights.Labels.AbuseReports.CappedBand; b != "" && !c.hasBand(b) {
		bad("labels.abuse_reports.capped_band: %q is not a defined band", b)
	}

	known := make(map[string]bool, len(c.Sources.Sources))
	for _, s := range c.Sources.Sources {
		if s.ID == "" {
			bad("sources: every entry needs an id")
			continue
		}
		if known[s.ID] {
			bad("sources: duplicate id %q", s.ID)
		}
		known[s.ID] = true

		switch s.Status {
		case StatusAllowed, StatusBlocked, StatusNeedsReview, StatusUnavailable:
		default:
			bad("sources.%s.status: %q is not a recognised status", s.ID, s.Status)
		}
		if s.Checked == "" {
			bad("sources.%s: checked date is required; SPEC.md §6.2 requires terms be checked before ingesting", s.ID)
		}
		if s.Status == StatusBlocked && s.BlockedReason == "" {
			bad("sources.%s: blocked sources must record why, so the decision is visible", s.ID)
		}
		if s.Status == StatusUnavailable && s.UnavailableReason == "" {
			bad("sources.%s: unavailable sources must record why; otherwise a dead end "+
				"is indistinguishable from work outstanding", s.ID)
		}
		if s.Ingestible() && (s.Confidence <= 0 || s.Confidence > 1) {
			bad("sources.%s.confidence: must be in (0,1], got %v", s.ID, s.Confidence)
		}
	}

	// Every source named in the priority order must exist, and every
	// ingestible source must be ranked — otherwise tie-breaking is undefined
	// and scores stop being deterministic.
	ranked := make(map[string]bool, len(c.Weights.Labels.SourcePriority))
	for _, id := range c.Weights.Labels.SourcePriority {
		if !known[id] {
			bad("labels.source_priority: %q is not a source in sources.yaml", id)
		}
		if ranked[id] {
			bad("labels.source_priority: duplicate entry %q", id)
		}
		ranked[id] = true
	}
	for _, s := range c.Sources.Sources {
		if s.Ingestible() && !ranked[s.ID] {
			bad("labels.source_priority: ingestible source %q is unranked, leaving tie-breaks undefined", s.ID)
		}
	}
	for _, id := range c.Weights.Labels.AbuseReports.Sources {
		if !known[id] {
			bad("labels.abuse_reports.sources: %q is not a source in sources.yaml", id)
		}
	}
	for _, id := range c.Weights.DerivedDeposit.AnchorSources {
		if !known[id] {
			bad("derived_deposit.anchor_sources: %q is not a source in sources.yaml", id)
		}
	}
	for cat := range c.Weights.Verdict.RedExposure {
		if _, ok := c.Weights.Categories[cat]; !ok {
			bad("verdict.red_exposure: %q is not a defined category", cat)
		}
	}
	if v := c.Weights.Verdict; v.ClearMinCoverage <= 0 || v.ConfidenceMedium > v.ConfidenceHigh {
		bad("verdict: clear_min_coverage must be positive and confidence_medium at most confidence_high")
	}
	if h := c.Weights.DerivedHotWallet; h.Enabled {
		for _, id := range h.ReserveSources {
			if !known[id] {
				bad("derived_hotwallet.reserve_sources: %q is not a source in sources.yaml", id)
			}
		}
		if h.MinTransfersEachWay < 1 || h.MinCounterparties < 1 {
			bad("derived_hotwallet: min_transfers_each_way and min_counterparties must be at least 1")
		}
		if h.MinExclusiveShare <= 0.5 || h.MinExclusiveShare > 1 {
			bad("derived_hotwallet.min_exclusive_share: must be in (0.5,1], got %v", h.MinExclusiveShare)
		}
		if h.Confidence <= 0 || h.Confidence > 1 {
			bad("derived_hotwallet.confidence: must be in (0,1], got %v", h.Confidence)
		}
	}

	// --- chains ---
	seenChain := map[string]bool{}
	for _, ch := range c.Sources.Chains {
		if ch.ID == "" {
			bad("chains: every entry needs an id")
			continue
		}
		if seenChain[ch.ID] {
			bad("chains: duplicate id %q", ch.ID)
		}
		seenChain[ch.ID] = true
		if ch.Status == StatusUnavailable && ch.UnavailableReason == "" {
			bad("chains.%s: unavailable chains must record why", ch.ID)
		}
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func (c *Config) hasBand(name string) bool {
	for _, b := range c.Weights.Bands {
		if b.Name == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Lookups used by the scoring and label packages
// ---------------------------------------------------------------------------

// CategoryWeight returns the weight for a category. Unknown categories carry
// zero weight and are reported as an error by the caller rather than silently
// contributing; scoring must never invent a weight.
func (c *Config) CategoryWeight(category string) (float64, bool) {
	cat, ok := c.Weights.Categories[category]
	if !ok {
		return 0, false
	}
	return cat.Weight, true
}

// BandFor maps a 0-100 score to its band name. The top band includes 100.
func (c *Config) BandFor(score float64) string {
	bands := append([]Band(nil), c.Weights.Bands...)
	sort.Slice(bands, func(i, j int) bool { return bands[i].Min < bands[j].Min })
	for i, b := range bands {
		last := i == len(bands)-1
		if score >= b.Min && (score < b.Max || (last && score <= b.Max)) {
			return b.Name
		}
	}
	if score < 0 {
		return bands[0].Name
	}
	return bands[len(bands)-1].Name
}

// SourcePriority returns the rank of a source; lower is stronger. Unranked
// sources sort last, deterministically.
func (c *Config) SourcePriority(id string) int {
	for i, s := range c.Weights.Labels.SourcePriority {
		if s == id {
			return i
		}
	}
	return len(c.Weights.Labels.SourcePriority)
}

// AlwaysWins reports whether a category overrides confidence-based
// resolution. SPEC.md §6: sanctions labels always win.
func (c *Config) AlwaysWins(category string) bool {
	for _, s := range c.Weights.Labels.AlwaysWins {
		if s == category {
			return true
		}
	}
	return false
}

// Chain returns the chain source entry for id.
func (c *Config) Chain(id string) (ChainSource, bool) {
	for _, ch := range c.Sources.Chains {
		if ch.ID == id {
			return ch, true
		}
	}
	return ChainSource{}, false
}

// Source returns the label source entry for id.
func (c *Config) Source(id string) (LabelSource, bool) {
	for _, s := range c.Sources.Sources {
		if s.ID == id {
			return s, true
		}
	}
	return LabelSource{}, false
}

// Categories returns category names in a stable sorted order. Scoring output
// must never depend on Go map iteration order (docs/PLAN.md D6).
func (c *Config) Categories() []string {
	out := make([]string, 0, len(c.Weights.Categories))
	for name := range c.Weights.Categories {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
