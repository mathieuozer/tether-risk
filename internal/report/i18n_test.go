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
	for _, c := range []struct {
		name  string
		text  map[string]string
		cats  map[string]string
		nouns int
	}{{"Turkish", trText, trCategoryNames, len(trNouns)}, {"Russian", ruText, ruCategoryNames, len(ruNouns)}} {
		for k, en := range enText {
			other, ok := c.text[k]
			if !ok {
				t.Errorf("%q has no %s text", k, c.name)
				continue
			}
			if a, b := strings.Join(verbs(en), ""), strings.Join(verbs(other), ""); a != b {
				t.Errorf("%q: English verbs %q, %s %q", k, a, c.name, b)
			}
		}
		for k := range c.text {
			if _, ok := enText[k]; !ok {
				t.Errorf("%q has %s text but no English", k, c.name)
			}
		}
		for cat := range categoryNames {
			if _, ok := c.cats[cat]; !ok {
				t.Errorf("category %q has no %s name", cat, c.name)
			}
		}
		if c.nouns != 4 {
			t.Errorf("%s has %d nouns, want 4", c.name, c.nouns)
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
		Verdict: &ConnectionsVerdict{Level: "high_risk", Confidence: "high", ConfidencePct: 92, Reasons: []ConnectionsVerdictReason{
			{Code: "band_high"}, {Code: "exposure", Category: "frozen_funds", Pct: 54.05},
		}}}
	en := Connections(in)
	if !strings.Contains(en, "🔴 RISKY · confidence 92%\n   Risky because its overall risk score is high; 54.0% of its money is linked to Frozen by Tether.") {
		t.Errorf("English verdict:\n%s", en)
	}
	if strings.Index(en, "RISKY") > strings.Index(en, "Exposure score") {
		t.Error("the verdict must come before the details")
	}
	in.Lang = "tr"
	if tr := Connections(in); !strings.Contains(tr, "Riskli, çünkü genel risk puanı yüksek; parasının %54,0 kadarı “Tether tarafından dondurulmuş” kategorisine bağlanıyor") || !strings.Contains(tr, "🔴 RİSKLİ · güven %92\n") {
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

// A poisoning answer names the look-alike and says only that.
func TestPoisoningVerdictText(t *testing.T) {
	in := ConnectionsInput{Address: "TBkgVghdGoFYpP3EajvuNt8j8qX4xEEtN8", Chain: "tron", Lang: "tr",
		OwnLabel: &ConnectionsOwnLabel{Category: "scam", Imitates: "TBkgikRealxxxxxxxxxxxxxxxxxxxEtN8"},
		Verdict: &ConnectionsVerdict{Level: "high_risk", ConfidencePct: 99, Reasons: []ConnectionsVerdictReason{
			{Code: "poisoning", Category: "scam", Address: "TBkgikRealxxxxxxxxxxxxxxxxxxxEtN8"}, {Code: "low_coverage", Pct: 51}}}}
	tr := Connections(in)
	for _, want := range []string{"🔴 RİSKLİ · güven %99\n   Riskli: bu bir adres zehirleme adresi, TBkgik…EtN8 adresine benzetilmiş.",
		"Bu adres doğrudan listede: Adres zehirleme, TBkgik…EtN8 adresinin taklidi (Dolandırıcılık)"} {
		if !strings.Contains(tr, want) {
			t.Errorf("missing %q in:\n%s", want, tr)
		}
	}
	if strings.Contains(tr, "%51") {
		t.Errorf("a secondary reason leaked into a poisoning answer:\n%s", tr)
	}
}

// A poisoning target is warned, with an address it was made to confuse.
func TestPoisoningTargetNote(t *testing.T) {
	in := ConnectionsInput{Address: "TVictim", Chain: "tron", Lang: "tr",
		Flags: []ConnectionsFlag{{Code: "poisoning_target", Count: 3, Address: "TBkgikRealxxxxxxxxxxxxxxxxxxxEtN8"}}}
	want := "Adres zehirleme hedefi: bu cüzdana 3 taklit adresten değersiz transfer gelmiş, örneğin TBkgik…EtN8 adresini taklit eden biri."
	if tr := Connections(in); !strings.Contains(tr, want) {
		t.Errorf("missing %q in:\n%s", want, tr)
	}
}

func TestConnectionsInRussian(t *testing.T) {
	in := ConnectionsInput{Address: "TAddr", Chain: "tron", Band: "high", Score: 63.9, Coverage: 0.918, Lang: "ru",
		Activity: &ConnectionsActivity{InUSD: 19600, InTransfers: 22, InCounterparties: 31},
		Depth:    &ConnectionsDepth{Counterparties: 100, Traced: 100, TotalCounterparties: 140},
		Inbound: &ConnectionsDirection{TracedWeight: 1, Categories: []ConnectionsCategory{{Category: "frozen_funds", Pct: 54.05}},
			Entries: []ConnectionsEntry{{Address: "TFTqpcigcD64vsg9W8WsSYJZ5t8PqrTAYX", Entity: "Unidentified high-volume service",
				Category: "unnamed_service", Pct: 12, MinHops: 3,
				Profile: &ConnectionsProfile{VolumeUSD: 43977446, Transfers: 10000, Counterparties: 6402, Partial: true}}}},
		Verdict: &ConnectionsVerdict{Level: "high_risk", ConfidencePct: 92, Reasons: []ConnectionsVerdictReason{
			{Code: "band_high"}, {Code: "exposure", Category: "frozen_funds", Pct: 54.05},
		}}}
	out := Connections(in)
	for _, want := range []string{
		"🔴 ЕСТЬ РИСК · уверенность 92%\n   Есть риск, потому что общий балл риска высокий; 54,0% его средств связаны с категорией «Заморожено Tether».",
		"Связи адреса:",
		"Заморожено Tether - 54,0%",
		"оборот $43.98M · контрагенты: 6\u00a0402 адреса · 10\u00a0000 сохранённых переводов (неполная история)",
		"Получено: $19.6k · 22 перевода · источники: 31 адрес",
		"в 3 шагах",
		"Отслежены самые активные контрагенты: 100 из 140.",
		"Балл риск-экспозиции: Высокий (63.9 / 100)",
		"Покрытие: 91,8%",
		"Санкции - не найдено",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, en := range []string{"Connections of", "RISKY", "not found"} {
		if strings.Contains(out, en) {
			t.Errorf("English %q left in Russian output", en)
		}
	}

	// A not-risky answer under 10% confidence says so.
	in.Verdict = &ConnectionsVerdict{Level: "caution", ConfidencePct: 7, Insufficient: true,
		Reasons: []ConnectionsVerdictReason{{Code: "low_coverage", Pct: 4}}}
	if out := Connections(in); !strings.Contains(out, "🟡 НЕТ РИСКА · уверенность 7% · недостаточно данных\n") {
		t.Errorf("Russian insufficient-data verdict:\n%s", out)
	}
}

func TestRussianPlurals(t *testing.T) {
	l := newLoc("ru")
	for n, want := range map[uint64]string{1: "1 адрес", 2: "2 адреса", 5: "5 адресов", 11: "11 адресов", 12: "12 адресов",
		21: "21 адрес", 22: "22 адреса", 111: "111 адресов", 1004: "1\u00a0004 адреса"} {
		if got := l.n(n, "address"); got != want {
			t.Errorf("n(%d) = %q, want %q", n, got, want)
		}
	}
}

// A service's own wallet is explained by its name, without the derived
// label's English description (docs/DECISIONS.md D39).
func TestWhyOwnServiceUsesTheName(t *testing.T) {
	v := &ConnectionsVerdict{Level: "clear", ConfidencePct: 60, Reasons: []ConnectionsVerdictReason{
		{Code: "own_service", Category: "named_service", Entity: "HTX (sends to its reserves)"}}}
	for _, lang := range []string{"en", "tr", "ru"} {
		got := whyText(v, newLoc(lang))
		if !strings.Contains(got, "HTX") || strings.Contains(got, "sends to its reserves") {
			t.Errorf("%s: %q", lang, got)
		}
	}
}
