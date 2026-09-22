package report

import (
	"path/filepath"

	"github.com/mozer/tether-risk/internal/config"
	"strings"
	"testing"
)

// Directions are combined by traced weight, not averaged: a direction with
// almost nothing traced must not carry half the weight.
func TestConnectionsWeightsDirectionsByValue(t *testing.T) {
	out := Connections(ConnectionsInput{
		Address: "TAddr", Chain: "tron", Band: "low", Score: 5, Coverage: 0.5,
		Inbound: &ConnectionsDirection{
			TracedWeight: 900,
			Categories:   []ConnectionsCategory{{"exchange", 100}},
		},
		Outbound: &ConnectionsDirection{
			TracedWeight:    100,
			UnattributedPct: 100,
		},
	})
	for _, want := range []string{
		"Exchange - 90.0%",
		"Unattributed (unknown, not clean) - 10.0%",
		"⛓ Blockchain: Tron (TRX)",
		"📈 Risk level: Low (5.0 / 100)",
		"🎯 Coverage: 50.0%",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestConnectionsSortsAndSplitsMinorCategories(t *testing.T) {
	out := Connections(ConnectionsInput{
		Address: "TAddr", Chain: "tron", Band: "medium", Score: 30, Coverage: 1,
		Inbound: &ConnectionsDirection{
			TracedWeight: 1000,
			Categories: []ConnectionsCategory{
				{"scam", 0.05},
				{"exchange", 60},
				{"unnamed_service", 39.95},
			},
		},
	})
	ex := strings.Index(out, "Exchange - 60.0%")
	svc := strings.Index(out, "Unnamed service - 40.0%")
	minor := strings.Index(out, "Less than 0.1%:")
	scam := strings.Index(out, "•   Scam\n")
	if ex < 0 || svc < 0 || minor < 0 || scam < 0 {
		t.Fatalf("missing expected lines:\n%s", out)
	}
	if !(ex < svc && svc < minor && minor < scam) {
		t.Errorf("wrong order:\n%s", out)
	}
	// Found, but too small to round: it must not read as 0.1%.
	if !strings.Contains(out, "Scam - found, under 0.1%") {
		t.Errorf("a found category under 0.1%% should say so:\n%s", out)
	}
	if strings.Contains(out, "•   Scam - ") {
		t.Errorf("a category under 0.1%% should not get a percentage:\n%s", out)
	}
}

// SPEC.md §7: the compact form must still carry every mandatory warning.
func TestConnectionsKeepsMandatoryWarnings(t *testing.T) {
	out := Connections(ConnectionsInput{
		Address: "TAddr", Chain: "tron", Band: "high", Score: 100, Coverage: 0.2,
		LowConfidence: true, SanctionsOverride: true,
		OwnLabel: &ConnectionsOwnLabel{Entity: "Some SDN entry", Category: "sanctions"},
		Inbound: &ConnectionsDirection{
			TracedWeight: 10, UnattributedPct: 80, FanoutCapped: true,
			Categories: []ConnectionsCategory{{"sanctions", 20}},
		},
		Disclaimer: "Not a regulated AML determination.",
	})
	for _, want := range []string{
		"directly listed: Some SDN entry (Sanctions)",
		"Direct sanctions match",
		"Low confidence: only 20.0%",
		"Traversal was truncated",
		"Unattributed (unknown, not clean) - 80.0%",
		"Not a regulated AML determination.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestConnectionsWithNoTracedValue(t *testing.T) {
	out := Connections(ConnectionsInput{Address: "TAddr", Chain: "tron", Band: "low"})
	if !strings.Contains(out, "No traced value") {
		t.Errorf("expected an explicit no-traced-value line:\n%s", out)
	}
}

func TestConnectionsDetailSections(t *testing.T) {
	out := Connections(ConnectionsInput{
		Address: "TAddr", Chain: "tron", Band: "low", Score: 5, Coverage: 0.15, LowConfidence: true,
		Activity: &ConnectionsActivity{
			InUSD: 4_812_946, OutUSD: 4_812_946,
			InTransfers: 158, OutTransfers: 35, InCounterparties: 125, OutCounterparties: 17,
			FirstSeen: "2026-08-02", LastSeen: "2026-09-19",
			Assets:            []ConnectionsAsset{{"USDT", 9_625_892}, {"TRX", 853}},
			UnpricedTransfers: 12, UnpricedTokens: 5,
		},
		Inbound: &ConnectionsDirection{
			TracedWeight: 1, UnattributedPct: 70,
			Categories: []ConnectionsCategory{{"unnamed_service", 30}},
			Entries: []ConnectionsEntry{
				{Address: "TQrY8tryqsYVCYS3MFbtffiPp2ccyn4STm", Entity: "HTX (proof-of-reserves wallet)",
					Category: "unnamed_service", Pct: 10, MinHops: 1},
				{Address: "TNXoiAJ3dct8Fjg4M9fkLFh9S2v9TXc32G", Entity: "Unidentified high-volume service",
					Category: "unnamed_service", Pct: 5, MinHops: 2},
			},
			Reasons: []ConnectionsReason{{"dead_end", 50}, {"hop_limit", 20}},
		},
	})
	for _, want := range []string{
		"Received: $4.81M in 158 transfers from 125 addresses",
		"Sent: $4.81M in 35 transfers to 17 addresses",
		"Active: 2026-08-02 → 2026-09-19",
		"Assets: USDT $9.63M · TRX $853",
		"Unrecognised tokens: 12 transfers of 5 tokens",
		"◦ trail stops (no stored history beyond this point) - 50.0%",
		"◦ beyond the hop limit - 20.0%",
		"1. HTX (proof-of-reserves wallet) (TQrY8t…4STm)",
		"10.0% ≈ $481.3k · direct",
		"2 hops away",
		"⚪  Sanctions - not found",
		"Checks cover the 15.0% of traced value",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A low-coverage result must not tick anything as clear.
	if strings.Contains(out, "✅") {
		t.Errorf("low confidence output shows a clear tick:\n%s", out)
	}
}

func TestConnectionsReportsTracingProgress(t *testing.T) {
	out := Connections(ConnectionsInput{
		Address: "TAddr", Chain: "tron", Band: "low",
		Depth: &ConnectionsDepth{Counterparties: 100, Traced: 22, TotalCounterparties: 140},
	})
	for _, want := range []string{
		"Tracing in progress: 22 of 100 counterparties traced",
		"The 100 most active of 140 counterparties are traced",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	done := Connections(ConnectionsInput{
		Address: "TAddr", Chain: "tron", Band: "low",
		Depth: &ConnectionsDepth{Counterparties: 31, Traced: 31, TotalCounterparties: 31, FetchError: "rate limited"},
	})
	for _, want := range []string{"Counterparties traced: 31 of 31", "stored data was used: rate limited"} {
		if !strings.Contains(done, want) {
			t.Errorf("missing %q in:\n%s", want, done)
		}
	}
}

func TestConnectionsReportsPartialHistory(t *testing.T) {
	out := Connections(ConnectionsInput{
		Address: "TAddr", Chain: "tron", Band: "low",
		Depth: &ConnectionsDepth{StillFetching: true, HistoryTruncated: true},
	})
	for _, want := range []string{"still being fetched", "more history than the per-address fetch limit"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestConnectionsReportsFrontier(t *testing.T) {
	out := Connections(ConnectionsInput{
		Address: "TAddr", Chain: "tron", Band: "low",
		Depth: &ConnectionsDepth{FrontierPending: 122, FrontierQueued: 100},
	})
	if !strings.Contains(out, "Tracing further: 100 of 122 addresses where the trail stops are queued") {
		t.Errorf("frontier progress missing:\n%s", out)
	}
}

func TestCategoryOrderMatchesWeights(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, c := range categoryOrder {
		listed[c] = true
		if _, ok := cfg.Weights.Categories[c]; !ok {
			t.Errorf("categoryOrder has %q, which weights.yaml does not define", c)
		}
		if _, ok := categoryNames[c]; !ok {
			t.Errorf("category %q has no display name", c)
		}
	}
	for c := range cfg.Weights.Categories {
		if !listed[c] {
			t.Errorf("weights.yaml defines %q, missing from categoryOrder", c)
		}
	}
}

func TestConnectionsListsEveryCategory(t *testing.T) {
	out := Connections(ConnectionsInput{
		Address: "TAddr", Chain: "tron", Band: "low", Coverage: 1,
		Inbound: &ConnectionsDirection{
			TracedWeight: 1,
			Categories: []ConnectionsCategory{
				{Category: "unnamed_service", Pct: 99.95},
				{Category: "dust", Pct: 0.05},
			},
		},
	})
	for _, want := range []string{
		"Unnamed service - 100.0%",
		// Trace amounts first, then every category not found, in one list.
		"Less than 0.1%:\n\n  •   Dust\n  •   Sanctions\n  •   Terrorist Financing\n",
		"  •   DEX\n  •   Exchange\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	small := out[strings.Index(out, "Less than 0.1%"):]
	small = small[:strings.Index(small, "🛡")]
	if strings.Contains(small, "Unnamed service") {
		t.Errorf("a listed category is repeated under less than 0.1%%:\n%s", small)
	}
	if strings.Count(small, "Dust") != 1 {
		t.Errorf("dust should appear once:\n%s", small)
	}
}

func TestConnectionsDescribesUnnamedService(t *testing.T) {
	out := Connections(ConnectionsInput{
		Address: "TAddr", Chain: "tron", Band: "low", Coverage: 1,
		Activity: &ConnectionsActivity{InUSD: 19600, InTransfers: 39, InCounterparties: 31},
		Inbound: &ConnectionsDirection{
			TracedWeight: 1,
			Categories:   []ConnectionsCategory{{Category: "unnamed_service", Pct: 100}},
			Entries: []ConnectionsEntry{{
				Address: "TFTqpcigcD64vsg9W8WsSYJZ5t8PqrTAYX", Entity: "Unidentified high-volume service",
				Category: "unnamed_service", Pct: 67.1, MinHops: 1,
				Profile: &ConnectionsProfile{
					VolumeUSD: 43977446, Transfers: 10000, Counterparties: 6402,
					FirstSeen: "2026-09-04", LastSeen: "2026-09-21", Assets: []string{"USDT"}, Partial: true,
				},
			}},
		},
	})
	for _, want := range []string{
		"1. High-volume service (TFTqpc…TAYX)",
		"$43.98M moved with 6,402 addresses across 10,000 stored transfers (partial history) · 2026-09-04 → 2026-09-21 · USDT",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Unidentified") {
		t.Errorf("entry still reads as unidentified:\n%s", out)
	}
}
