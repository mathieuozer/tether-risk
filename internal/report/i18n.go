package report

import (
	"fmt"
	"strings"
)

// The connections summary in English and Turkish (docs/DECISIONS.md D27).
// Each message has a key and a format string per language; a test keeps the
// two catalogues in step, including the number and kind of their verbs.

// loc renders messages in one language.
type loc struct{ tr bool }

func newLoc(lang string) loc { return loc{tr: strings.HasPrefix(strings.ToLower(lang), "tr")} }

func (l loc) f(key string, args ...any) string {
	m := enText
	if l.tr {
		m = trText
	}
	format, ok := m[key]
	if !ok {
		format = enText[key]
	}
	return fmt.Sprintf(format, args...)
}

// n renders a count with its noun: "3 transfers", or in Turkish, which does
// not pluralise after a number, "3 transfer".
func (l loc) n(count uint64, noun string) string {
	if l.tr {
		return l.num(count) + " " + trNouns[noun]
	}
	if count == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "ss") {
		return l.num(count) + " " + noun + "es"
	}
	return l.num(count) + " " + noun + "s"
}

// num groups thousands: 10,000 in English, 10.000 in Turkish.
func (l loc) num(n uint64) string {
	sep := ","
	if l.tr {
		sep = "."
	}
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + sep + s[i:]
	}
	return s
}

// pct renders a share: 61.0% in English, %61,0 in Turkish.
func (l loc) pct(v float64) string {
	if l.tr {
		return "%" + strings.Replace(fmt.Sprintf("%.1f", v), ".", ",", 1)
	}
	return fmt.Sprintf("%.1f%%", v)
}

func (l loc) category(c string) string {
	if l.tr {
		if n, ok := trCategoryNames[c]; ok {
			return n
		}
	}
	if n, ok := categoryNames[c]; ok {
		return n
	}
	return titleCase(strings.ReplaceAll(c, "_", " "))
}

func (l loc) band(b string) string {
	if l.tr {
		if n, ok := map[string]string{"low": "Düşük", "medium": "Orta", "high": "Yüksek"}[strings.ToLower(b)]; ok {
			return n
		}
	}
	return titleCase(b)
}

var trNouns = map[string]string{
	"transfer":        "transfer",
	"address":         "adres",
	"token":           "token",
	"stored transfer": "kayıtlı transfer",
}

var trCategoryNames = map[string]string{
	"sanctions":           "Yaptırımlar",
	"terrorist_financing": "Terörün Finansmanı",
	"darknet":             "Darknet Pazarı",
	"stolen_funds":        "Çalıntı Fonlar",
	"frozen_funds":        "Tether tarafından dondurulmuş",
	"mixer":               "Karıştırıcı (Mixer)",
	"scam":                "Dolandırıcılık",
	"high_risk_exchange":  "Yüksek Riskli Borsa",
	"gambling":            "Kumar",
	"unnamed_service":     "İsimsiz hizmet",
	"dust":                "Toz işlemler (dust)",
	"dex":                 "DEX",
	"exchange":            "Borsa",
}

var enText = map[string]string{
	"address":                  "🔵 Address: %s\n\n",
	"chain":                    "⛓ Blockchain: %s\n\n",
	"listed":                   "🚫 This address is directly listed: %s (%s)\n\n",
	"sanctioned":               "🚫 Direct sanctions match. Risk is High regardless of score.\n\n",
	"activity":                 "📊 Activity\n\n",
	"received":                 "  •   Received: %s in %s from %s\n",
	"sent":                     "  •   Sent: %s in %s to %s\n",
	"active":                   "  •   Active: %s → %s\n",
	"assets":                   "  •   Assets: %s\n",
	"unpriced":                 "  •   Unrecognised tokens: %s of %s, not valued (typical of spam and airdrops)\n",
	"connections":              "Connections of the address:\n\n",
	"no_value":                 "  •   No traced value\n\n",
	"unattributed":             "  •   Unattributed (unknown, not clean) - %s\n",
	"less_than":                "\nLess than 0.1%%:\n\n",
	"risk_level":               "📈 Risk level: %s (%.1f / 100)\n",
	"coverage":                 "🎯 Coverage: %s\n",
	"low_confidence":           "\n⚠️ Low confidence: only %s of traced value reached a known entity. The rest is unknown, not clean.\n",
	"band_capped":              "\n⚠️ Band capped: the only evidence is unverified abuse reports.\n",
	"truncated":                "\nℹ️ Traversal was truncated; value beyond the limit is unknown.\n",
	"inbound":                  "⬅️ Inbound (funds came from)",
	"outbound":                 "➡️ Outbound (funds went to)",
	"identified":               "🏷 Identified connections\n",
	"and_more":                 "    … and %d more\n",
	"under":                    "under 0.1%%",
	"checks":                   "🛡 Risk checks\n\n",
	"found":                    "  🔴  %s - found, %s\n",
	"not_found":                "  %s  %s - not found\n",
	"checks_cover":             "\n  Checks cover the %s of traced value that could be attributed.\n\n",
	"r_dead_end":               "trail stops (no stored history beyond this point)",
	"r_hop_limit":              "beyond the hop limit",
	"r_fanout_cap":             "too many counterparties to follow",
	"r_unlabelled":             "labelled, but without a category",
	"profile":                  "%s moved with %s across %s",
	"partial":                  " (partial history)",
	"direct":                   "direct",
	"hops_away":                "%d hops away",
	"service":                  "High-volume service",
	"fetch_error":              "⚠️ Could not refresh this address from the chain, so stored data was used: %s\n\n",
	"still_fetching":           "🔄 This address's history is still being fetched. The figures below are partial; screen again in a few minutes.\n\n",
	"history_limit":            "ℹ️ This address has more history than the per-address fetch limit (10,000 transfers); activity figures cover the most recent part.\n\n",
	"queued_of":                "%d of %d",
	"frontier":                 "🔭 Tracing further: %s addresses where the trail stops are queued. Screen again later for a deeper result.\n",
	"tracing":                  "🔄 Tracing in progress: %d of %d counterparties traced. Screen again later for a deeper result.\n",
	"traced":                   "🔎 Counterparties traced: %d of %d.\n",
	"most_active":              "   The %d most active of %d counterparties are traced.\n",
	"disclaimer":               "This is an automated triage and pre-screening result built on open data. It is not a regulated AML determination and must not be used as one.",
	"still_fetching_followup":  "🔄 This address's history is still being fetched. The figures below are partial; I will send the final result here when tracing finishes.\n\n",
	"frontier_followup":        "🔭 Tracing further: %s addresses where the trail stops are queued. I will send the final result here when tracing finishes.\n",
	"tracing_followup":         "🔄 Tracing in progress: %d of %d counterparties traced. I will send the final result here when tracing finishes.\n",
	"flags":                    "⚑ Behaviour notes (not part of the score)\n\n",
	"flag_pass_through":        "  •   Pass-through: %s in and %s out within %d days; almost nothing stays. Typical of intermediary and layering wallets, also of OTC desks and exchanges' internal wallets.\n",
	"flag_high_volume_new":     "  •   New address with high volume: %s moved within %d days of its first activity.\n",
	"flag_new_address":         "  •   New address: first activity %d days ago.\n",
	"direct_high":              "🚫 Direct listing (%s). Risk is High regardless of score.\n\n",
	"v_clear":                  "🟢 LOOKS CLEAN · confidence: %s\n",
	"v_caution":                "🟡 CAUTION · confidence: %s\n",
	"v_high_risk":              "🔴 HIGH RISK · confidence: %s\n",
	"conf_high":                "high",
	"conf_medium":              "medium",
	"conf_low":                 "low",
	"vr_own_listed":            "  •   The address itself is listed: %s\n",
	"vr_band_high":             "  •   Risk band is High\n",
	"vr_band_medium":           "  •   Risk band is Medium\n",
	"vr_exposure":              "  •   %s of traced value reaches %s\n",
	"vr_exposure_minor":        "  •   %s of traced value reaches %s (below the high-risk line)\n",
	"vr_low_coverage":          "  •   Only %s of traced value could be attributed; the rest is unknown, not clean\n",
	"vr_tracing_incomplete":    "  •   Tracing has not finished yet\n",
	"vr_behaviour":             "  •   Behaviour: %s\n",
	"vr_clean":                 "  •   No risk found across %s of traced value, and tracing finished\n",
	"flagname_pass_through":    "pass-through wallet",
	"flagname_high_volume_new": "new address with high volume",
}

var trText = map[string]string{
	"address":                  "🔵 Adres: %s\n\n",
	"chain":                    "⛓ Blokzincir: %s\n\n",
	"listed":                   "🚫 Bu adres doğrudan listede: %s (%s)\n\n",
	"sanctioned":               "🚫 Doğrudan yaptırım eşleşmesi. Puandan bağımsız olarak risk Yüksek.\n\n",
	"activity":                 "📊 Hareketler\n\n",
	"received":                 "  •   Gelen: %s · %s · kaynak: %s\n",
	"sent":                     "  •   Giden: %s · %s · alıcı: %s\n",
	"active":                   "  •   Etkin: %s → %s\n",
	"assets":                   "  •   Varlıklar: %s\n",
	"unpriced":                 "  •   Tanınmayan tokenlar: %s, %s; değerlenmedi (spam ve airdrop'larda tipik)\n",
	"connections":              "Adresin bağlantıları:\n\n",
	"no_value":                 "  •   İzlenen değer yok\n\n",
	"unattributed":             "  •   Atanamayan (bilinmiyor, temiz değil) - %s\n",
	"less_than":                "\n%%0,1'den az:\n\n",
	"risk_level":               "📈 Risk seviyesi: %s (%.1f / 100)\n",
	"coverage":                 "🎯 Kapsam: %s\n",
	"low_confidence":           "\n⚠️ Düşük güven: izlenen değerin yalnızca %s kadarı bilinen bir kuruluşa ulaştı. Geri kalanı bilinmiyor, temiz değil.\n",
	"band_capped":              "\n⚠️ Seviye sınırlandı: tek kanıt doğrulanmamış kötüye kullanım bildirimleri.\n",
	"truncated":                "\nℹ️ İzleme sınırda kesildi; sınırın ötesindeki değer bilinmiyor.\n",
	"inbound":                  "⬅️ Gelen (fonların kaynağı)",
	"outbound":                 "➡️ Giden (fonların gittiği yer)",
	"identified":               "🏷 Tanımlanan bağlantılar\n",
	"and_more":                 "    … ve %d tane daha\n",
	"under":                    "%%0,1'den az",
	"checks":                   "🛡 Risk kontrolleri\n\n",
	"found":                    "  🔴  %s - bulundu, %s\n",
	"not_found":                "  %s  %s - bulunmadı\n",
	"checks_cover":             "\n  Kontroller, izlenen değerin atanabilen %s kısmını kapsar.\n\n",
	"r_dead_end":               "iz bitiyor (bu noktanın ötesinde kayıtlı geçmiş yok)",
	"r_hop_limit":              "adım sınırının ötesinde",
	"r_fanout_cap":             "izlenemeyecek kadar çok karşı taraf",
	"r_unlabelled":             "etiketli ama kategorisiz",
	"profile":                  "%s hacim · karşı taraf: %s · %s",
	"partial":                  " (kısmi geçmiş)",
	"direct":                   "doğrudan",
	"hops_away":                "%d adım uzakta",
	"service":                  "Yüksek hacimli hizmet",
	"fetch_error":              "⚠️ Adres zincirden güncellenemedi, kayıtlı veri kullanıldı: %s\n\n",
	"still_fetching":           "🔄 Bu adresin geçmişi hâlâ çekiliyor. Aşağıdaki rakamlar kısmi; birkaç dakika sonra tekrar tarayın.\n\n",
	"history_limit":            "ℹ️ Bu adresin geçmişi adres başına çekme sınırını (10.000 transfer) aşıyor; hareketler en yeni kısmı kapsar.\n\n",
	"queued_of":                "%d / %d",
	"frontier":                 "🔭 Daha derine iniliyor: izin bittiği %s adres sıraya alındı. Daha derin sonuç için daha sonra tekrar tarayın.\n",
	"tracing":                  "🔄 İzleme sürüyor: %d / %d karşı taraf izlendi. Daha derin sonuç için daha sonra tekrar tarayın.\n",
	"traced":                   "🔎 İzlenen karşı taraflar: %d / %d.\n",
	"most_active":              "   %[2]d karşı taraftan en etkin %[1]d tanesi izleniyor.\n",
	"disclaimer":               "Bu, açık verilere dayalı otomatik bir ön tarama sonucudur. Düzenlenmiş bir AML kararı değildir ve öyle kullanılmamalıdır.",
	"still_fetching_followup":  "🔄 Bu adresin geçmişi hâlâ çekiliyor. Aşağıdaki rakamlar kısmi; izleme bitince kesin sonucu buraya göndereceğim.\n\n",
	"frontier_followup":        "🔭 Daha derine iniliyor: izin bittiği %s adres sıraya alındı. İzleme bitince kesin sonucu buraya göndereceğim.\n",
	"tracing_followup":         "🔄 İzleme sürüyor: %d / %d karşı taraf izlendi. İzleme bitince kesin sonucu buraya göndereceğim.\n",
	"flags":                    "⚑ Davranış notları (puana dahil değil)\n\n",
	"flag_pass_through":        "  •   Gir-çık cüzdanı: %s girdi, %s çıktı, %d gün içinde; neredeyse hiçbir şey kalmıyor. Aracı ve katmanlama cüzdanlarında tipik; OTC masalarında ve borsaların iç cüzdanlarında da görülür.\n",
	"flag_high_volume_new":     "  •   Yüksek hacimli yeni adres: ilk hareketinden bu yana %[2]d günde %[1]s hareket etti.\n",
	"flag_new_address":         "  •   Yeni adres: ilk hareket %d gün önce.\n",
	"direct_high":              "🚫 Doğrudan listede (%s). Puandan bağımsız olarak risk Yüksek.\n\n",
	"v_clear":                  "🟢 TEMİZ GÖRÜNÜYOR · güven: %s\n",
	"v_caution":                "🟡 DİKKAT · güven: %s\n",
	"v_high_risk":              "🔴 RİSKLİ · güven: %s\n",
	"conf_high":                "yüksek",
	"conf_medium":              "orta",
	"conf_low":                 "düşük",
	"vr_own_listed":            "  •   Adresin kendisi listede: %s\n",
	"vr_band_high":             "  •   Risk seviyesi Yüksek\n",
	"vr_band_medium":           "  •   Risk seviyesi Orta\n",
	"vr_exposure":              "  •   %[2]s: izlenen değerin %[1]s kadarı\n",
	"vr_exposure_minor":        "  •   %[2]s: izlenen değerin %[1]s kadarı (yüksek risk eşiğinin altında)\n",
	"vr_low_coverage":          "  •   İzlenen değerin yalnızca %s kadarı atanabildi; geri kalanı bilinmiyor, temiz değil\n",
	"vr_tracing_incomplete":    "  •   İzleme henüz bitmedi\n",
	"vr_behaviour":             "  •   Davranış: %s\n",
	"vr_clean":                 "  •   İzlenen değerin %s kadarında risk bulunmadı ve izleme tamamlandı\n",
	"flagname_pass_through":    "gir-çık cüzdanı",
	"flagname_high_volume_new": "yüksek hacimli yeni adres",
}
