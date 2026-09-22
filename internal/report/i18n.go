package report

import (
	"fmt"
	"strings"
)

// The connections summary in English, Turkish and Russian (docs/DECISIONS.md
// D27). Each message has a key and a format string per language; a test keeps
// the catalogues in step, including the number and kind of their verbs.

// loc renders messages in one language: "en", "tr" or "ru".
type loc struct{ lang string }

func newLoc(lang string) loc {
	l := strings.ToLower(lang)
	switch {
	case strings.HasPrefix(l, "tr"):
		return loc{lang: "tr"}
	case strings.HasPrefix(l, "ru"):
		return loc{lang: "ru"}
	}
	return loc{lang: "en"}
}

func (l loc) f(key string, args ...any) string {
	m := enText
	switch l.lang {
	case "tr":
		m = trText
	case "ru":
		m = ruText
	}
	format, ok := m[key]
	if !ok {
		format = enText[key]
	}
	return fmt.Sprintf(format, args...)
}

// n renders a count with its noun: "3 transfers"; in Turkish, which does not
// pluralise after a number, "3 transfer"; in Russian, with the form the
// number takes: "1 перевод", "3 перевода", "5 переводов".
func (l loc) n(count uint64, noun string) string {
	switch l.lang {
	case "tr":
		return l.num(count) + " " + trNouns[noun]
	case "ru":
		forms := ruNouns[noun]
		i := 2
		switch n10, n100 := count%10, count%100; {
		case n10 == 1 && n100 != 11:
			i = 0
		case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
			i = 1
		}
		return l.num(count) + " " + forms[i]
	}
	if count == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "ss") {
		return l.num(count) + " " + noun + "es"
	}
	return l.num(count) + " " + noun + "s"
}

// num groups thousands: 10,000 in English, 10.000 in Turkish, 10 000 (with a
// no-break space) in Russian.
func (l loc) num(n uint64) string {
	sep := ","
	switch l.lang {
	case "tr":
		sep = "."
	case "ru":
		sep = "\u00a0"
	}
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + sep + s[i:]
	}
	return s
}

// pct renders a share: 61.0% in English, %61,0 in Turkish, 61,0% in Russian.
func (l loc) pct(v float64) string {
	switch l.lang {
	case "tr":
		return "%" + strings.Replace(fmt.Sprintf("%.1f", v), ".", ",", 1)
	case "ru":
		return strings.Replace(fmt.Sprintf("%.1f", v), ".", ",", 1) + "%"
	}
	return fmt.Sprintf("%.1f%%", v)
}

func (l loc) category(c string) string {
	names := map[string]map[string]string{"tr": trCategoryNames, "ru": ruCategoryNames}[l.lang]
	if n, ok := names[c]; ok {
		return n
	}
	if n, ok := categoryNames[c]; ok {
		return n
	}
	return titleCase(strings.ReplaceAll(c, "_", " "))
}

func (l loc) band(b string) string {
	names := map[string]map[string]string{
		"tr": {"low": "Düşük", "medium": "Orta", "high": "Yüksek"},
		"ru": {"low": "Низкий", "medium": "Средний", "high": "Высокий"},
	}[l.lang]
	if n, ok := names[strings.ToLower(b)]; ok {
		return n
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
	"named_service":       "Tanımlı hizmet",
	"unnamed_service":     "İsimsiz hizmet",
	"dust":                "Toz işlemler (dust)",
	"dex":                 "DEX",
	"exchange":            "Borsa",
}

// ruNouns holds each noun's forms after 1, after 2-4 and after 5 or more.
var ruNouns = map[string][3]string{
	"transfer":        {"перевод", "перевода", "переводов"},
	"address":         {"адрес", "адреса", "адресов"},
	"token":           {"токен", "токена", "токенов"},
	"stored transfer": {"сохранённый перевод", "сохранённых перевода", "сохранённых переводов"},
}

var ruCategoryNames = map[string]string{
	"sanctions":           "Санкции",
	"terrorist_financing": "Финансирование терроризма",
	"darknet":             "Даркнет-рынок",
	"stolen_funds":        "Похищенные средства",
	"frozen_funds":        "Заморожено Tether",
	"mixer":               "Миксер",
	"scam":                "Мошенничество",
	"high_risk_exchange":  "Высокорисковая биржа",
	"gambling":            "Азартные игры",
	"named_service":       "Известный сервис",
	"unnamed_service":     "Неназванный сервис",
	"dust":                "Пылевые переводы (dust)",
	"dex":                 "DEX",
	"exchange":            "Биржа",
}

var enText = map[string]string{
	"address":                    "🔵 Address: %s\n\n",
	"chain":                      "⛓ Blockchain: %s\n\n",
	"listed":                     "🚫 This address is directly listed: %s (%s)\n\n",
	"sanctioned":                 "🚫 Direct sanctions match. Risk is High regardless of score.\n\n",
	"activity":                   "📊 Activity\n\n",
	"received":                   "  •   Received: %s in %s from %s\n",
	"sent":                       "  •   Sent: %s in %s to %s\n",
	"active":                     "  •   Active: %s → %s\n",
	"assets":                     "  •   Assets: %s\n",
	"unpriced":                   "  •   Unrecognised tokens: %s of %s, not valued (typical of spam and airdrops)\n",
	"connections":                "Connections of the address:\n\n",
	"no_value":                   "  •   No traced value\n\n",
	"unattributed":               "  •   Unattributed (unknown, not clean) - %s\n",
	"less_than":                  "\nLess than 0.1%%:\n\n",
	"risk_level":                 "📈 Exposure score: %s (%.1f / 100)\n",
	"risk_level_verdict":         "📈 Exposure score: %s (%.1f / 100). It measures only value linked to risk lists; the answer is the verdict at the top.\n",
	"checks_unseen":              "  ⚪ means not found in what could be seen: %s of traced value ends at services nobody has identified, and what is behind them is not part of these checks.\n\n",
	"coverage":                   "🎯 Coverage: %s\n",
	"low_confidence":             "\n⚠️ Low confidence: only %s of traced value reached a known entity. The rest is unknown, not clean.\n",
	"band_capped":                "\n⚠️ Band capped: the only evidence is unverified abuse reports.\n",
	"truncated":                  "\nℹ️ Traversal was truncated; value beyond the limit is unknown.\n",
	"inbound":                    "⬅️ Inbound (funds came from)",
	"outbound":                   "➡️ Outbound (funds went to)",
	"identified":                 "🏷 Identified connections\n",
	"and_more":                   "    … and %d more\n",
	"under":                      "under 0.1%%",
	"checks":                     "🛡 Risk checks\n\n",
	"found":                      "  🔴  %s - found, %s\n",
	"not_found":                  "  %s  %s - not found\n",
	"checks_cover":               "\n  Checks cover the %s of traced value that could be attributed.\n\n",
	"r_dead_end":                 "trail stops (no stored history beyond this point)",
	"r_hop_limit":                "beyond the hop limit",
	"r_fanout_cap":               "too many counterparties to follow",
	"r_unlabelled":               "labelled, but without a category",
	"profile":                    "%s moved with %s across %s",
	"partial":                    " (partial history)",
	"direct":                     "direct",
	"hops_away":                  "%d hops away",
	"service":                    "High-volume service",
	"fetch_error":                "⚠️ Could not refresh this address from the chain, so stored data was used: %s\n\n",
	"still_fetching":             "🔄 This address's history is still being fetched. The figures below are partial; screen again in a few minutes.\n\n",
	"history_limit":              "ℹ️ This address has more history than the per-address fetch limit (10,000 transfers); activity figures cover the most recent part.\n\n",
	"queued_of":                  "%d of %d",
	"frontier":                   "🔭 Tracing further: %s addresses where the trail stops are queued. Screen again later for a deeper result.\n",
	"tracing":                    "🔄 Tracing in progress: %d of %d counterparties traced. Screen again later for a deeper result.\n",
	"traced":                     "🔎 Counterparties traced: %d of %d.\n",
	"most_active":                "   The %d most active of %d counterparties are traced.\n",
	"disclaimer":                 "This is an automated triage and pre-screening result built on open data. It is not a regulated AML determination and must not be used as one.",
	"still_fetching_followup":    "🔄 This address's history is still being fetched. The figures below are partial; I will send the final result here when tracing finishes.\n\n",
	"frontier_followup":          "🔭 Tracing further: %s addresses where the trail stops are queued. I will send the final result here when tracing finishes.\n",
	"tracing_followup":           "🔄 Tracing in progress: %d of %d counterparties traced. I will send the final result here when tracing finishes.\n",
	"flags":                      "⚑ Behaviour notes (not part of the score)\n\n",
	"flag_pass_through":          "  •   Pass-through: %s in and %s out within %d days; almost nothing stays. Typical of intermediary and layering wallets, also of OTC desks and exchanges' internal wallets.\n",
	"flag_high_volume_new":       "  •   New address with high volume: %s moved within %d days of its first activity.\n",
	"flag_new_address":           "  •   New address: first activity %d days ago.\n",
	"direct_high":                "🚫 Direct listing (%s). Risk is High regardless of score.\n\n",
	"v_clear":                    "🟢 NOT RISKY · confidence %d%%\n",
	"v_caution":                  "🟡 NOT RISKY · confidence %d%%\n",
	"v_high_risk":                "🔴 RISKY · confidence %d%%\n",
	"v_insufficient":             " · not enough data\n",
	"conf_high":                  "high",
	"conf_medium":                "medium",
	"conf_low":                   "low",
	"vr_own_listed":              "  •   The address itself is listed: %s\n",
	"vr_band_high":               "  •   Exposure score is High\n",
	"vr_band_medium":             "  •   Exposure score is Medium\n",
	"vr_exposure":                "  •   %s of traced value reaches %s\n",
	"vr_exposure_minor":          "  •   %s of traced value reaches %s (below the high-risk line)\n",
	"vr_low_coverage":            "  •   Only %s of traced value could be attributed; the rest is unknown, not clean\n",
	"vr_tracing_incomplete":      "  •   Tracing has not finished yet\n",
	"vr_unidentified":            "  •   %s of traced value ends at services nobody has identified; what is behind them cannot be seen\n",
	"vr_behaviour":               "  •   Behaviour: %s\n",
	"vr_clean":                   "  •   No risk found across %s of traced value, and tracing finished\n",
	"flagname_pass_through":      "pass-through wallet",
	"flagname_high_volume_new":   "new address with high volume",
	"flag_high_volume_new_today": "  •   New address with high volume: %s moved in less than a day since its first activity.\n",
	"flag_round_split":           "  •   Round split: %s sent, in single transfers, to %d different wallets within %d minutes. Typical of splitting funds to break the trail.\n",
	"flag_parked_funds":          "  •   Parked funds: %d wallets that received from this address have sent nothing since; %s is waiting in them.\n",
	"flag_poisoning_target":      "  •   Address poisoning target: %d look-alike addresses sent this wallet worthless transfers, for example one imitating %s. They sit in its history: never copy an address from there.\n",
	"flag_frozen_contact":        "  •   Paid by a wallet Tether has just frozen: %s sent this wallet %s in the week before Tether froze it, within the last three days (frozen wallets of this kind: %d). Tether freezes in clusters: 3.1%% of wallets in this position were frozen too, nearly all within three days, against 0.12%% for ordinary wallets. Accepting its USDT in the next few days carries that risk.\n",
	"flagname_round_split":       "round-amount split",
	"flagname_parked_funds":      "parked funds",
	"why_clear":                  "   No risk list connection was found and almost all of the money could be followed to named services.\n",
	"why_caution":                "   Nothing links it to risk strongly enough to call it risky. What lowers confidence: %s.\n",
	"why_high_risk":              "   Risky because %s. Do not send or accept funds without further checks.\n",
	"why_own_listed":             "the address itself is on the %s list",
	"why_poisoning_verdict":      "   Risky: this is an address-poisoning address, made to look like %s. It sends worthless transfers so that it is copied from transaction history by mistake. Do not send to it; take the real address from the recipient, never from history.\n",
	"poisoning_entity":           "Address poisoning, look-alike of %s",
	"why_exposure":               "%s of its money is linked to %s",
	"why_low_coverage":           "only %s of its money could be traced to a known place",
	"why_band_high":              "its overall risk score is high",
	"why_band_medium":            "its overall risk score is medium",
	"why_tracing_incomplete":     "tracing has not finished yet",
	"why_unidentified":           "%s of its money moves through services whose operator is unknown, so what sits behind them cannot be checked",
	"why_pass_through":           "money passes straight through it",
	"why_high_volume_new":        "it is new and has moved a large amount",
	"why_round_split":            "it split money into identical round amounts across several wallets",
	"why_parked_funds":           "money it sent is sitting untouched in fresh wallets",
}

var trText = map[string]string{
	"address":                    "🔵 Adres: %s\n\n",
	"chain":                      "⛓ Blokzincir: %s\n\n",
	"listed":                     "🚫 Bu adres doğrudan listede: %s (%s)\n\n",
	"sanctioned":                 "🚫 Doğrudan yaptırım eşleşmesi. Puandan bağımsız olarak risk Yüksek.\n\n",
	"activity":                   "📊 Hareketler\n\n",
	"received":                   "  •   Gelen: %s · %s · kaynak: %s\n",
	"sent":                       "  •   Giden: %s · %s · alıcı: %s\n",
	"active":                     "  •   Etkin: %s → %s\n",
	"assets":                     "  •   Varlıklar: %s\n",
	"unpriced":                   "  •   Tanınmayan tokenlar: %s, %s; değerlenmedi (spam ve airdrop'larda tipik)\n",
	"connections":                "Adresin bağlantıları:\n\n",
	"no_value":                   "  •   İzlenen değer yok\n\n",
	"unattributed":               "  •   Atanamayan (bilinmiyor, temiz değil) - %s\n",
	"less_than":                  "\n%%0,1'den az:\n\n",
	"risk_level":                 "📈 Maruziyet skoru: %s (%.1f / 100)\n",
	"risk_level_verdict":         "📈 Maruziyet skoru: %s (%.1f / 100). Yalnızca risk listelerine bağlanan değeri ölçer; asıl cevap en üstteki karardır.\n",
	"checks_unseen":              "  ⚪ görülebilen kısımda bulunmadı demektir: izlenen değerin %s kadarı kimliği belirlenemeyen hizmetlerde bitiyor ve onların arkası bu kontrollere dahil değil.\n\n",
	"coverage":                   "🎯 Kapsam: %s\n",
	"low_confidence":             "\n⚠️ Düşük güven: izlenen değerin yalnızca %s kadarı bilinen bir kuruluşa ulaştı. Geri kalanı bilinmiyor, temiz değil.\n",
	"band_capped":                "\n⚠️ Seviye sınırlandı: tek kanıt doğrulanmamış kötüye kullanım bildirimleri.\n",
	"truncated":                  "\nℹ️ İzleme sınırda kesildi; sınırın ötesindeki değer bilinmiyor.\n",
	"inbound":                    "⬅️ Gelen (fonların kaynağı)",
	"outbound":                   "➡️ Giden (fonların gittiği yer)",
	"identified":                 "🏷 Tanımlanan bağlantılar\n",
	"and_more":                   "    … ve %d tane daha\n",
	"under":                      "%%0,1'den az",
	"checks":                     "🛡 Risk kontrolleri\n\n",
	"found":                      "  🔴  %s - bulundu, %s\n",
	"not_found":                  "  %s  %s - bulunmadı\n",
	"checks_cover":               "\n  Kontroller, izlenen değerin atanabilen %s kısmını kapsar.\n\n",
	"r_dead_end":                 "iz bitiyor (bu noktanın ötesinde kayıtlı geçmiş yok)",
	"r_hop_limit":                "adım sınırının ötesinde",
	"r_fanout_cap":               "izlenemeyecek kadar çok karşı taraf",
	"r_unlabelled":               "etiketli ama kategorisiz",
	"profile":                    "%s hacim · karşı taraf: %s · %s",
	"partial":                    " (kısmi geçmiş)",
	"direct":                     "doğrudan",
	"hops_away":                  "%d adım uzakta",
	"service":                    "Yüksek hacimli hizmet",
	"fetch_error":                "⚠️ Adres zincirden güncellenemedi, kayıtlı veri kullanıldı: %s\n\n",
	"still_fetching":             "🔄 Bu adresin geçmişi hâlâ çekiliyor. Aşağıdaki rakamlar kısmi; birkaç dakika sonra tekrar tarayın.\n\n",
	"history_limit":              "ℹ️ Bu adresin geçmişi adres başına çekme sınırını (10.000 transfer) aşıyor; hareketler en yeni kısmı kapsar.\n\n",
	"queued_of":                  "%d / %d",
	"frontier":                   "🔭 Daha derine iniliyor: izin bittiği %s adres sıraya alındı. Daha derin sonuç için daha sonra tekrar tarayın.\n",
	"tracing":                    "🔄 İzleme sürüyor: %d / %d karşı taraf izlendi. Daha derin sonuç için daha sonra tekrar tarayın.\n",
	"traced":                     "🔎 İzlenen karşı taraflar: %d / %d.\n",
	"most_active":                "   %[2]d karşı taraftan en etkin %[1]d tanesi izleniyor.\n",
	"disclaimer":                 "Bu, açık verilere dayalı otomatik bir ön tarama sonucudur. Düzenlenmiş bir AML kararı değildir ve öyle kullanılmamalıdır.",
	"still_fetching_followup":    "🔄 Bu adresin geçmişi hâlâ çekiliyor. Aşağıdaki rakamlar kısmi; izleme bitince kesin sonucu buraya göndereceğim.\n\n",
	"frontier_followup":          "🔭 Daha derine iniliyor: izin bittiği %s adres sıraya alındı. İzleme bitince kesin sonucu buraya göndereceğim.\n",
	"tracing_followup":           "🔄 İzleme sürüyor: %d / %d karşı taraf izlendi. İzleme bitince kesin sonucu buraya göndereceğim.\n",
	"flags":                      "⚑ Davranış notları (puana dahil değil)\n\n",
	"flag_pass_through":          "  •   Gir-çık cüzdanı: %s girdi, %s çıktı, %d gün içinde; neredeyse hiçbir şey kalmıyor. Aracı ve katmanlama cüzdanlarında tipik; OTC masalarında ve borsaların iç cüzdanlarında da görülür.\n",
	"flag_high_volume_new":       "  •   Yüksek hacimli yeni adres: ilk hareketinden bu yana %[2]d günde %[1]s hareket etti.\n",
	"flag_new_address":           "  •   Yeni adres: ilk hareket %d gün önce.\n",
	"direct_high":                "🚫 Doğrudan listede (%s). Puandan bağımsız olarak risk Yüksek.\n\n",
	"v_clear":                    "🟢 RİSKLİ DEĞİL · güven %%%d\n",
	"v_caution":                  "🟡 RİSKLİ DEĞİL · güven %%%d\n",
	"v_high_risk":                "🔴 RİSKLİ · güven %%%d\n",
	"v_insufficient":             " · yeterli veri yok\n",
	"conf_high":                  "yüksek",
	"conf_medium":                "orta",
	"conf_low":                   "düşük",
	"vr_own_listed":              "  •   Adresin kendisi listede: %s\n",
	"vr_band_high":               "  •   Maruziyet skoru Yüksek\n",
	"vr_band_medium":             "  •   Maruziyet skoru Orta\n",
	"vr_exposure":                "  •   %[2]s: izlenen değerin %[1]s kadarı\n",
	"vr_exposure_minor":          "  •   %[2]s: izlenen değerin %[1]s kadarı (yüksek risk eşiğinin altında)\n",
	"vr_low_coverage":            "  •   İzlenen değerin yalnızca %s kadarı atanabildi; geri kalanı bilinmiyor, temiz değil\n",
	"vr_tracing_incomplete":      "  •   İzleme henüz bitmedi\n",
	"vr_unidentified":            "  •   İzlenen değerin %s kadarı kimliği belirlenemeyen hizmetlerde bitiyor; bunların arkası görünmüyor\n",
	"vr_behaviour":               "  •   Davranış: %s\n",
	"vr_clean":                   "  •   İzlenen değerin %s kadarında risk bulunmadı ve izleme tamamlandı\n",
	"flagname_pass_through":      "gir-çık cüzdanı",
	"flagname_high_volume_new":   "yüksek hacimli yeni adres",
	"flag_high_volume_new_today": "  •   Yüksek hacimli yeni adres: ilk hareketinden bu yana 1 günden kısa sürede %s hareket etti.\n",
	"flag_round_split":           "  •   Yuvarlak bölme: %[3]d dakika içinde %[2]d farklı cüzdana, her birine tek transferle %[1]s gönderildi. İzi kırmak için parayı bölmekte tipik.\n",
	"flag_parked_funds":          "  •   Park edilmiş para: bu adresten para alan %d cüzdan o günden beri hiçbir şey göndermedi; içlerinde %s bekliyor.\n",
	"flag_poisoning_target":      "  •   Adres zehirleme hedefi: bu cüzdana %d taklit adresten değersiz transfer gelmiş, örneğin %s adresini taklit eden biri. Bunlar işlem geçmişinde duruyor: adresi asla oradan kopyalamayın.\n",
	"flag_frozen_contact":        "  •   Tether'in az önce dondurduğu bir cüzdandan para gelmiş: %s, Tether onu son üç gün içinde dondurmadan önceki hafta bu cüzdana %s gönderdi (bu türden dondurulmuş cüzdan: %d). Tether kümeler hâlinde dondurur: bu durumdaki cüzdanların %%3,1'i de donduruldu, neredeyse hepsi üç gün içinde; sıradan cüzdanlarda bu oran %%0,12. Önümüzdeki birkaç gün bu cüzdandan USDT kabul etmek bu riski taşır.\n",
	"flagname_round_split":       "yuvarlak bölme",
	"flagname_parked_funds":      "park edilmiş para",
	"why_clear":                  "   Risk listeleriyle bağlantı bulunmadı ve paranın neredeyse tamamı kimliği bilinen hizmetlere kadar izlendi.\n",
	"why_caution":                "   Riskli sayılacak bir bağlantı bulunmadı. Güveni düşüren: %s.\n",
	"why_high_risk":              "   Riskli, çünkü %s. Ek kontrol yapmadan para göndermeyin ve kabul etmeyin.\n",
	"why_own_listed":             "adresin kendisi %s listesinde",
	"why_poisoning_verdict":      "   Riskli: bu bir adres zehirleme adresi, %s adresine benzetilmiş. Değersiz transferler gönderip işlem geçmişinden yanlışlıkla kopyalanmayı bekliyor. Bu adrese para göndermeyin; gerçek adresi işlem geçmişinden değil, doğrudan alıcının kendisinden alın.\n",
	"poisoning_entity":           "Adres zehirleme, %s adresinin taklidi",
	"why_exposure":               "parasının %s kadarı “%s” kategorisine bağlanıyor",
	"why_low_coverage":           "parasının yalnızca %s kadarı bilinen bir yere kadar izlenebildi",
	"why_band_high":              "genel risk puanı yüksek",
	"why_band_medium":            "genel risk puanı orta",
	"why_tracing_incomplete":     "izleme henüz bitmedi",
	"why_unidentified":           "parasının %s kadarı kimin işlettiği bilinmeyen hizmetlerden geçiyor, bu yüzden arkalarında ne olduğu kontrol edilemiyor",
	"why_pass_through":           "para adresten durmadan geçip gidiyor",
	"why_high_volume_new":        "adres yeni ve büyük tutarlar hareket ettirmiş",
	"why_round_split":            "parayı birkaç cüzdana aynı yuvarlak tutarlarla bölmüş",
	"why_parked_funds":           "gönderdiği para yeni cüzdanlarda hiç kıpırdamadan bekliyor",
}

var ruText = map[string]string{
	"address":                    "🔵 Адрес: %s\n\n",
	"chain":                      "⛓ Блокчейн: %s\n\n",
	"listed":                     "🚫 Этот адрес напрямую внесён в список: %s (%s)\n\n",
	"sanctioned":                 "🚫 Прямое совпадение с санкционным списком. Риск высокий независимо от балла.\n\n",
	"activity":                   "📊 Активность\n\n",
	"received":                   "  •   Получено: %s · %s · источники: %s\n",
	"sent":                       "  •   Отправлено: %s · %s · получатели: %s\n",
	"active":                     "  •   Период активности: %s → %s\n",
	"assets":                     "  •   Активы: %s\n",
	"unpriced":                   "  •   Нераспознанные токены: %s, %s; не оценены (типично для спама и эйрдропов)\n",
	"connections":                "Связи адреса:\n\n",
	"no_value":                   "  •   Отслеженных средств нет\n\n",
	"unattributed":               "  •   Не атрибутировано (неизвестно, а не «чисто») - %s\n",
	"less_than":                  "\nМенее 0,1%%:\n\n",
	"risk_level":                 "📈 Балл риск-экспозиции: %s (%.1f / 100)\n",
	"risk_level_verdict":         "📈 Балл риск-экспозиции: %s (%.1f / 100). Он учитывает только средства, связанные со списками риска; ответ даёт вердикт в начале.\n",
	"checks_unseen":              "  ⚪ означает «не найдено в видимой части»: %s отслеженных средств уходит в сервисы, которые никто не идентифицировал, а то, что за ними, в эти проверки не входит.\n\n",
	"coverage":                   "🎯 Покрытие: %s\n",
	"low_confidence":             "\n⚠️ Низкая уверенность: лишь %s отслеженных средств дошло до известного субъекта. Остальное неизвестно, а не «чисто».\n",
	"band_capped":                "\n⚠️ Уровень ограничен: единственное основание — непроверенные жалобы на злоупотребления.\n",
	"truncated":                  "\nℹ️ Отслеживание остановилось на лимите; средства за его пределами неизвестны.\n",
	"inbound":                    "⬅️ Входящие (откуда пришли средства)",
	"outbound":                   "➡️ Исходящие (куда ушли средства)",
	"identified":                 "🏷 Идентифицированные связи\n",
	"and_more":                   "    … и ещё %d\n",
	"under":                      "менее 0,1%%",
	"checks":                     "🛡 Проверки на риск\n\n",
	"found":                      "  🔴  %s - найдено, %s\n",
	"not_found":                  "  %s  %s - не найдено\n",
	"checks_cover":               "\n  Проверки охватывают %s отслеженных средств, которые удалось атрибутировать.\n\n",
	"r_dead_end":                 "след обрывается (дальше этой точки сохранённой истории нет)",
	"r_hop_limit":                "за пределом допустимого числа шагов",
	"r_fanout_cap":               "слишком много контрагентов, чтобы отследить",
	"r_unlabelled":               "есть метка, но без категории",
	"profile":                    "оборот %s · контрагенты: %s · %s",
	"partial":                    " (неполная история)",
	"direct":                     "напрямую",
	"hops_away":                  "в %d шагах",
	"service":                    "Сервис с большим оборотом",
	"fetch_error":                "⚠️ Не удалось обновить данные адреса из блокчейна, поэтому использованы сохранённые данные: %s\n\n",
	"still_fetching":             "🔄 История этого адреса ещё загружается. Цифры ниже неполные; повторите проверку через несколько минут.\n\n",
	"history_limit":              "ℹ️ История этого адреса больше лимита загрузки на один адрес (10 000 переводов); показатели активности отражают самую свежую часть.\n\n",
	"queued_of":                  "%d из %d",
	"frontier":                   "🔭 Отслеживаем дальше: адреса, на которых обрывается след, поставлены в очередь (%s). Повторите проверку позже, чтобы получить более глубокий результат.\n",
	"tracing":                    "🔄 Идёт отслеживание: отслежено контрагентов — %d из %d. Повторите проверку позже, чтобы получить более глубокий результат.\n",
	"traced":                     "🔎 Отслежено контрагентов: %d из %d.\n",
	"most_active":                "   Отслежены самые активные контрагенты: %d из %d.\n",
	"disclaimer":                 "Это результат автоматической предварительной проверки на основе открытых данных. Он не является регулируемым AML-заключением и не должен использоваться в этом качестве.",
	"still_fetching_followup":    "🔄 История этого адреса ещё загружается. Цифры ниже неполные; итоговый результат я пришлю сюда, когда отслеживание завершится.\n\n",
	"frontier_followup":          "🔭 Отслеживаем дальше: адреса, на которых обрывается след, поставлены в очередь (%s). Итоговый результат я пришлю сюда, когда отслеживание завершится.\n",
	"tracing_followup":           "🔄 Идёт отслеживание: отслежено контрагентов — %d из %d. Итоговый результат я пришлю сюда, когда отслеживание завершится.\n",
	"flags":                      "⚑ Поведенческие признаки (не входят в балл)\n\n",
	"flag_pass_through":          "  •   Транзитный кошелёк: за %[3]d дн. поступило %[1]s и ушло %[2]s; почти ничего не остаётся. Типично для промежуточных кошельков и расслоения средств, а также для OTC-площадок и внутренних кошельков бирж.\n",
	"flag_high_volume_new":       "  •   Новый адрес с большим оборотом: %[1]s за %[2]d дн. с первой активности.\n",
	"flag_new_address":           "  •   Новый адрес: первая активность %d дн. назад.\n",
	"direct_high":                "🚫 Прямое внесение в список (%s). Риск высокий независимо от балла.\n\n",
	"v_clear":                    "🟢 НЕТ РИСКА · уверенность %d%%\n",
	"v_caution":                  "🟡 НЕТ РИСКА · уверенность %d%%\n",
	"v_high_risk":                "🔴 ЕСТЬ РИСК · уверенность %d%%\n",
	"v_insufficient":             " · недостаточно данных\n",
	"conf_high":                  "высокая",
	"conf_medium":                "средняя",
	"conf_low":                   "низкая",
	"vr_own_listed":              "  •   Сам адрес внесён в список: %s\n",
	"vr_band_high":               "  •   Балл риск-экспозиции высокий\n",
	"vr_band_medium":             "  •   Балл риск-экспозиции средний\n",
	"vr_exposure":                "  •   %[2]s: %[1]s отслеженных средств\n",
	"vr_exposure_minor":          "  •   %[2]s: %[1]s отслеженных средств (ниже порога высокого риска)\n",
	"vr_low_coverage":            "  •   Атрибутировать удалось лишь %s отслеженных средств; остальное неизвестно, а не «чисто»\n",
	"vr_tracing_incomplete":      "  •   Отслеживание ещё не завершено\n",
	"vr_unidentified":            "  •   %s отслеженных средств уходит в сервисы, которые никто не идентифицировал; что за ними, не видно\n",
	"vr_behaviour":               "  •   Поведение: %s\n",
	"vr_clean":                   "  •   В %s отслеженных средств риск не найден, отслеживание завершено\n",
	"flagname_pass_through":      "транзитный кошелёк",
	"flagname_high_volume_new":   "новый адрес с большим оборотом",
	"flag_high_volume_new_today": "  •   Новый адрес с большим оборотом: %s менее чем за сутки с первой активности.\n",
	"flag_round_split":           "  •   Дробление круглыми суммами: %[1]s отправлено отдельными переводами за %[3]d мин.; разных кошельков-получателей: %[2]d. Типичный приём, чтобы оборвать след средств.\n",
	"flag_parked_funds":          "  •   Замершие средства: кошельки, получившие средства с этого адреса (%d), с тех пор ничего не отправляли; в них лежит %s.\n",
	"flag_poisoning_target":      "  •   Цель отравления адреса: этому кошельку приходили пустые переводы с адресов-двойников (%d), например с адреса, имитирующего %s. Они остаются в истории операций: никогда не копируйте адрес оттуда.\n",
	"flag_frozen_contact":        "  •   Средства от кошелька, только что замороженного Tether: %s отправил этому кошельку %s за неделю до заморозки, которая произошла в последние три дня (таких замороженных кошельков: %d). Tether замораживает кластерами: 3,1%% кошельков в таком положении тоже были заморожены, почти все в течение трёх дней, против 0,12%% у обычных кошельков. Принимать USDT с этого кошелька в ближайшие дни — значит брать на себя этот риск.\n",
	"flagname_round_split":       "дробление круглыми суммами",
	"flagname_parked_funds":      "замершие средства",
	"why_clear":                  "   Связей со списками риска не найдено, и почти все средства удалось проследить до известных сервисов.\n",
	"why_caution":                "   Ничто не связывает адрес с риском настолько, чтобы считать его рискованным. Уверенность снижает: %s.\n",
	"why_high_risk":              "   Есть риск, потому что %s. Не отправляйте и не принимайте средства без дополнительной проверки.\n",
	"why_own_listed":             "сам адрес находится в списке «%s»",
	"why_poisoning_verdict":      "   Есть риск: это адрес-двойник для отравления адреса, он сделан похожим на %s. Он рассылает пустые переводы, чтобы его по ошибке скопировали из истории операций. Не отправляйте на него средства; берите настоящий адрес у самого получателя, а не из истории.\n",
	"poisoning_entity":           "Отравление адреса, двойник %s",
	"why_exposure":               "%s его средств связаны с категорией «%s»",
	"why_low_coverage":           "лишь %s его средств удалось проследить до известного субъекта",
	"why_band_high":              "общий балл риска высокий",
	"why_band_medium":            "общий балл риска средний",
	"why_tracing_incomplete":     "отслеживание ещё не завершено",
	"why_unidentified":           "%s его средств проходит через сервисы с неизвестным владельцем, поэтому проверить, что за ними, нельзя",
	"why_pass_through":           "средства проходят через адрес транзитом",
	"why_high_volume_new":        "адрес новый и уже провёл крупные суммы",
	"why_round_split":            "средства раздроблены на одинаковые круглые суммы по нескольким кошелькам",
	"why_parked_funds":           "отправленные им средства лежат нетронутыми в новых кошельках",
}
