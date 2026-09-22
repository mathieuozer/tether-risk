package report

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// verbs lists a format string's verbs, ignoring %% and argument indexes, so
// "%[2]d … %[1]d" and "%d … %d" compare equal.
var verbRE = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?[a-zA-Z%]`)

func verbs(format string) []string {
	var out []string
	for _, m := range verbRE.FindAllString(format, -1) {
		if m == "%%" {
			continue
		}
		out = append(out, m[len(m)-1:])
	}
	sort.Strings(out)
	return out
}

func TestCataloguesMatch(t *testing.T) {
	for k, en := range enText {
		tr, ok := trText[k]
		if !ok {
			t.Errorf("%q has no Turkish text", k)
			continue
		}
		if a, b := strings.Join(verbs(en), ""), strings.Join(verbs(tr), ""); a != b {
			t.Errorf("%q: English verbs %q, Turkish %q", k, a, b)
		}
	}
	for k := range trText {
		if _, ok := enText[k]; !ok {
			t.Errorf("%q has Turkish text but no English", k)
		}
	}
	for c := range categoryNames {
		if _, ok := trCategoryNames[c]; !ok {
			t.Errorf("category %q has no Turkish name", c)
		}
	}
}

func TestConnectionsInTurkish(t *testing.T) {
	out := Connections(ConnectionsInput{
		Address: "TAddr", Chain: "tron", Band: "low", Score: 15, Coverage: 0.999, Lang: "tr",
		Activity: &ConnectionsActivity{InUSD: 19600, InTransfers: 39, InCounterparties: 31},
		Depth:    &ConnectionsDepth{Counterparties: 100, Traced: 100, TotalCounterparties: 140},
		Inbound: &ConnectionsDirection{
			TracedWeight: 1,
			Categories:   []ConnectionsCategory{{Category: "unnamed_service", Pct: 99.9}},
			Entries: []ConnectionsEntry{{Address: "TFTqpcigcD64vsg9W8WsSYJZ5t8PqrTAYX",
				Entity: "Unidentified high-volume service", Category: "unnamed_service", Pct: 67.2, MinHops: 1,
				Profile: &ConnectionsProfile{VolumeUSD: 43977446, Transfers: 10000, Counterparties: 6402, Partial: true}}},
		},
	})
	for _, want := range []string{
		"Adresin bağlantıları:",
		"İsimsiz hizmet - %99,9",
		"Yüksek hacimli hizmet (TFTqpc…TAYX)",
		"$43.98M hacim · karşı taraf: 6.402 adres · 10.000 kayıtlı transfer (kısmi geçmiş)",
		"Gelen: $19.6k · 39 transfer · kaynak: 31 adres",
		"140 karşı taraftan en etkin 100 tanesi izleniyor",
		"Maruziyet skoru: Düşük (15.0 / 100)",
		"Kapsam: %99,9",
		"Yaptırımlar - bulunmadı",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, en := range []string{"Connections of", "Unnamed service", "not found"} {
		if strings.Contains(out, en) {
			t.Errorf("English %q left in Turkish output", en)
		}
	}
}

func TestBehaviourNotes(t *testing.T) {
	in := ConnectionsInput{Address: "TAddr", Chain: "tron", Band: "low", Flags: []ConnectionsFlag{
		{Code: "pass_through", InUSD: 7_110_000, OutUSD: 7_105_000, Days: 11},
		{Code: "high_volume_new", VolumeUSD: 14_215_000, AgeDays: 12},
	}}
	en := Connections(in)
	for _, want := range []string{"Behaviour notes (not part of the score)", "Pass-through: $7.11M in and $7.11M out within 11 days",
		"New address with high volume: $14.21M moved within 12 days"} {
		if !strings.Contains(en, want) {
			t.Errorf("missing %q in:\n%s", want, en)
		}
	}
	in.Lang = "tr"
	tr := Connections(in)
	for _, want := range []string{"Davranış notları (puana dahil değil)", "Gir-çık cüzdanı: $7.11M girdi, $7.11M çıktı, 11 gün içinde",
		"ilk hareketinden bu yana 12 günde $14.21M hareket etti"} {
		if !strings.Contains(tr, want) {
			t.Errorf("missing %q in:\n%s", want, tr)
		}
	}
}

func TestVerdictComesFirst(t *testing.T) {
	in := ConnectionsInput{Address: "TAddr", Chain: "tron", Band: "high", Score: 63.9, Coverage: 0.918,
		Verdict: &ConnectionsVerdict{Level: "high_risk", Confidence: "high", Reasons: []ConnectionsVerdictReason{
			{Code: "band_high"}, {Code: "exposure", Category: "frozen_funds", Pct: 54.05},
		}}}
	en := Connections(in)
	if !strings.Contains(en, "🔴 HIGH RISK · confidence: high\n   High risk because its overall risk score is high; 54.0% of its money is linked to Frozen by Tether.") ||
		!strings.Contains(en, "  •   Exposure score is High\n  •   54.0% of traced value reaches Frozen by Tether") {
		t.Errorf("English verdict:\n%s", en)
	}
	if strings.Index(en, "HIGH RISK") > strings.Index(en, "Exposure score") {
		t.Error("the verdict must come before the details")
	}
	in.Lang = "tr"
	if tr := Connections(in); !strings.Contains(tr, "Riskli, çünkü genel risk puanı yüksek; parasının %54,0 kadarı “Tether tarafından dondurulmuş” kategorisine bağlanıyor") || !strings.Contains(tr, "🔴 RİSKLİ · güven: yüksek") || !strings.Contains(tr, "Tether tarafından dondurulmuş: izlenen değerin %54,0 kadarı") {
		t.Errorf("Turkish verdict:\n%s", tr)
	}
}

// A risk connection is listed even when five larger services outrank it.
func TestRiskConnectionsListedFirst(t *testing.T) {
	var entries []ConnectionsEntry
	for i := 0; i < 6; i++ {
		entries = append(entries, ConnectionsEntry{Address: fmt.Sprintf("TService%026d", i), Category: "unnamed_service", Pct: float64(20 - i)})
	}
	entries = append(entries, ConnectionsEntry{Address: "TFrozen00000000000000000000000000", Entity: "Frozen by Tether (USDT blacklist)", Category: "frozen_funds", Pct: 3})
	out := Connections(ConnectionsInput{Address: "TAddr", Chain: "tron", Band: "low",
		Inbound: &ConnectionsDirection{TracedWeight: 1, Categories: []ConnectionsCategory{{Category: "unnamed_service", Pct: 90}, {Category: "frozen_funds", Pct: 3}}, Entries: entries}})
	if !strings.Contains(out, "1. Frozen by Tether (USDT blacklist)") {
		t.Errorf("frozen connection not listed first:\n%s", out)
	}
}
