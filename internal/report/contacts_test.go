package report

import (
	"strings"
	"testing"
)

// The counterparty contacts of TVnZo4…gRFR as measured on 2026-09-28 (D48).
func contactsFixture() []ConnectionsContact {
	return []ConnectionsContact{
		{Address: "TB1R8kS56xShvgtUc1Lp5KqUXWyggfLiF3", Category: "unnamed_service", Inbound: true,
			FlowUSD: 29_550, TotalUSD: 391_160_505, RiskyUSD: 5_548_721, SharePct: 1.42,
			Risks: []ConnectionsContactRisk{{Category: "frozen_funds", USD: 5_548_721, Addresses: 1,
				Entity: "Frozen by Tether (USDT blacklist)", Address: "TRJqS9aYftcZ8KcySM75eEUi7Srv6HWNqh"}}},
		{Address: "TNYhZCtCu56otX3WC5unPnQ21GsBJAGRF1", Category: "unnamed_service", Inbound: true,
			FlowUSD: 26_000, TotalUSD: 90_334_968, RiskyUSD: 4_114_000, SharePct: 4.6,
			Risks: []ConnectionsContactRisk{{Category: "terrorist_financing", USD: 4_114_000, Addresses: 1,
				Entity: "Arash Estaki ALIVAND", Address: "TQ5H49Wz3K57zNHmuXVp6uLzFwitxviABs"}}},
		{Address: "TQ1shnH16kGsTP9fHRbS1YHAYbhkLhQHu6", Category: "unnamed_service", Inbound: true,
			FlowUSD: 29_700, TotalUSD: 684_394_046, RiskyUSD: 1_302_000, SharePct: 0.19,
			Risks: []ConnectionsContactRisk{
				{Category: "terrorist_financing", USD: 1_300_000, Addresses: 1, Entity: "ANSARALLAH", Address: "TQ5mpZPbQMSq2s1vSM3RJgiB1AsTsiTpFZ"},
				{Category: "frozen_funds", USD: 2_000, Addresses: 2, Entity: "Frozen by Tether (USDT blacklist)", Address: "TRJqS9aYftcZ8KcySM75eEUi7Srv6HWNqh"}}},
		{Address: "TCCS8KY5S7kUSVfQxLyNq2p3DB1oW7sTUT", Inbound: true, FlowUSD: 109_890, RiskyUSD: 1_689_436, Partial: true,
			Risks: []ConnectionsContactRisk{{Category: "frozen_funds", USD: 1_689_436, Addresses: 1,
				Entity: "Frozen by Tether (USDT blacklist)", Address: "TL34x6JY55TTksamArx1mVrBWtrrvnuQRu"}}},
		{Address: "TBA6CypYJizwA9XdC7Ubgc5F1bxrQ7SqPt", Entity: "Gate.io (withdrawal hot wallet)", Category: "named_service",
			FlowUSD: 13_572_279, TotalUSD: 1_200_000_000, RiskyUSD: 6_893_760, SharePct: 0.57,
			Risks: []ConnectionsContactRisk{{Category: "terrorist_financing", USD: 6_893_760, Addresses: 1,
				Entity: "Arash Estaki ALIVAND", Address: "TQ5H49Wz3K57zNHmuXVp6uLzFwitxviABs"}}},
	}
}

func TestContactsSection(t *testing.T) {
	in := ConnectionsInput{Address: "TAddr", Chain: "tron", Band: "low", Contacts: contactsFixture(),
		Verdict: &ConnectionsVerdict{Level: "caution", ConfidencePct: 46,
			Reasons: []ConnectionsVerdictReason{{Code: "contact", Category: "terrorist_financing", Pct: 4.6, Address: "TNYhZCtCu56otX3WC5unPnQ21GsBJAGRF1"}}}}
	out := Connections(in)
	for _, want := range []string{
		"🟡 NEEDS REVIEW · confidence 46%",
		"4.6% of a counterparty's own money comes from the Terrorist Financing list",
		"What its counterparties did with listed addresses (not part of the score)",
		"TB1R8k…LiF3 (Unnamed service) paid this address $29.6k. It received $5.55M from 1 address on the Frozen by Tether list, the largest TRJqS9…WNqh, 1.4% of everything it received.",
		"TNYhZC…GRF1 (Unnamed service) paid this address $26.0k. It received $4.11M from 1 address on the Terrorist Financing list, the largest Arash Estaki ALIVAND, 4.6% of everything it received.",
		"Also $2.0k with 2 addresses on the Frozen by Tether list.",
		"TCCS8K…sTUT (no label) paid this address $109.9k. It received $1.69M from 1 address on the Frozen by Tether list, the largest TL34x6…uQRu (its own history is not stored, so this is a partial view).",
		"TBA6Cy…SqPt (Gate.io) received $13.57M from this address. It sent $6.89M to 1 address on the Terrorist Financing list, the largest Arash Estaki ALIVAND, 0.6% of everything it sent.",
		"Money that meets in these wallets cannot be followed through them",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The section comes after the checks and before the behaviour notes.
	in.Flags = []ConnectionsFlag{{Code: "new_address", AgeDays: 3}}
	in.Inbound = &ConnectionsDirection{TracedWeight: 1, Categories: []ConnectionsCategory{{"unnamed_service", 100}}}
	out = Connections(in)
	checks, contacts, flags := strings.Index(out, "🛡"), strings.Index(out, "🧭"), strings.Index(out, "⚑")
	if !(checks < contacts && contacts < flags) {
		t.Errorf("order: checks %d, contacts %d, flags %d\n%s", checks, contacts, flags, out)
	}

	in.Lang = "tr"
	tr := Connections(in)
	for _, want := range []string{
		"🟡 İNCELEME GEREKLİ · güven %46",
		"bir karşı tarafının kendi parasının %4,6 kadarı Terörün Finansmanı listesinden geliyor",
		"Karşı taraflarının listedeki adreslerle teması (puana dahil değil)",
		"TNYhZC…GRF1 (İsimsiz hizmet) bu adrese $26.0k gönderdi. Kendisi $4.11M almış; kaynağı 1 adres, Terörün Finansmanı listesinden; en büyüğü Arash Estaki ALIVAND, aldığı her şeyin %4,6 kadarı.",
		"TBA6Cy…SqPt (Gate.io) bu adresten $13.57M aldı. Kendisi $6.89M göndermiş; alıcısı 1 adres, Terörün Finansmanı listesinden; en büyüğü Arash Estaki ALIVAND, gönderdiği her şeyin %0,6 kadarı.",
	} {
		if !strings.Contains(tr, want) {
			t.Errorf("missing %q in:\n%s", want, tr)
		}
	}
	in.Lang = "ru"
	ru := Connections(in)
	for _, want := range []string{
		"🟡 ТРЕБУЕТ ПРОВЕРКИ · уверенность 46%",
		"Контакты его контрагентов с адресами из списков (не входят в балл)",
		"TNYhZC…GRF1 (Неназванный сервис) отправил этому адресу $26.0k. Сам он получил $4.11M; отправители — 1 адрес из списка «Финансирование терроризма», крупнейший — Arash Estaki ALIVAND, это 4,6% всего полученного им.",
	} {
		if !strings.Contains(ru, want) {
			t.Errorf("missing %q in:\n%s", want, ru)
		}
	}
}
