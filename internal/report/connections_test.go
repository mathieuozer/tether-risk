package report

import (
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
	if strings.Contains(out, "Scam - ") {
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
