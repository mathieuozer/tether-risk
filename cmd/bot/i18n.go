package main

import (
	"fmt"
	"strings"
)

// The bot's messages in English and Turkish (docs/DECISIONS.md D27). A user's
// language is their /language choice, else their Telegram client's.

const (
	langEN = "en"
	langTR = "tr"
)

// normLang maps a stored choice or a Telegram language_code to en or tr.
func normLang(chosen, client string) string {
	for _, l := range []string{chosen, client} {
		l = strings.ToLower(strings.TrimSpace(l))
		if strings.HasPrefix(l, "tr") {
			return langTR
		}
		if strings.HasPrefix(l, "en") {
			return langEN
		}
	}
	return langEN
}

// t renders a message. A key missing from Turkish falls back to English; a
// test keeps both complete.
func t(lang, key string, args ...any) string {
	m, ok := messages[key]
	if !ok {
		return key
	}
	format := m[0]
	if lang == langTR && m[1] != "" {
		format = m[1]
	}
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// messages: key -> {English, Turkish}.
var messages = map[string][2]string{
	// --- general ---
	"error_ours": {
		"Something went wrong on our side. Please try again in a minute.",
		"Bizim tarafımızda bir sorun oluştu. Lütfen bir dakika sonra tekrar deneyin.",
	},
	"unknown_command": {
		"Unknown command. Send an address, or /help.",
		"Bilinmeyen komut. Bir adres gönderin ya da /help yazın.",
	},
	"welcome": {
		"Hi %s. Send me a blockchain address and I will screen it: where its money came from, where it went, and which risk categories it touches.\n\n",
		"Merhaba %s. Bana bir blokzincir adresi gönderin, tarayayım: parası nereden geldi, nereye gitti ve hangi risk kategorilerine dokunuyor.\n\n",
	},
	"welcome_admin": {
		"You are an admin: every feature, no daily limit.\n",
		"Yöneticisiniz: tüm özellikler açık, günlük sınır yok.\n",
	},
	"welcome_footer": {
		"\n/app opens the full app · /plans to subscribe · /help for everything else",
		"\n/app tam uygulamayı açar · /plans abonelik · /help diğer her şey",
	},
	"there": {"there", "sevgili kullanıcı"},
	"trial_started": {
		"Your free %d-day trial has started: the %s plan, %d screens a day.",
		"%d günlük ücretsiz denemeniz başladı: %s planı, günde %d tarama.",
	},
	"help": {
		`Address risk screening.

Send an address and you get a summary of its connections: where its funds came from and went, and the risk categories they touch.

/app                 open the app
/details <address>   full breakdown with paths (Pro)
/pdf <address>       one-page PDF report (Pro)
/watch <address>     alert me when its risk changes
/watches  /unwatch <address>
/history             your recent screens
/plans  /status  /cancel
/apikey              API keys (Business)
/language            English / Türkçe
/terms  /support  /paysupport

Batch: send a .txt or .csv file with one address per line (Pro).

This is automated triage and pre-screening built on open data. It is not a regulated AML determination and must not be used as one.

Always read the coverage figure alongside the score. Low coverage means most traced value could not be attributed to a known entity: unknown, not clean.`,
		`Adres risk taraması.

Bir adres gönderin; bağlantılarının özetini alın: fonları nereden geldi, nereye gitti ve hangi risk kategorilerine dokunuyor.

/app                 uygulamayı aç
/details <adres>     yollarla birlikte tam döküm (Pro)
/pdf <adres>         tek sayfalık PDF rapor (Pro)
/watch <adres>       riski değişince beni uyar
/watches  /unwatch <adres>
/history             son taramalarınız
/plans  /status  /cancel
/apikey              API anahtarları (Business)
/language            English / Türkçe
/terms  /support  /paysupport

Toplu tarama: her satırda bir adres olan .txt veya .csv dosyası gönderin (Pro).

Bu, açık verilere dayalı otomatik bir ön taramadır. Düzenlenmiş bir AML kararı değildir ve öyle kullanılmamalıdır.

Puanı her zaman kapsam oranıyla birlikte okuyun. Düşük kapsam, izlenen değerin çoğunun bilinen bir kuruluşa bağlanamadığı anlamına gelir: bilinmiyor, temiz değil.`,
	},

	// --- screening ---
	"usage_cmd": {"Usage: %s <address>", "Kullanım: %s <adres>"},
	"bad_address": {
		"That does not look like a blockchain address. A TRON address starts with T and is 34 characters; an Ethereum or BSC address starts with 0x and is 42.",
		"Bu bir blokzincir adresine benzemiyor. TRON adresi T ile başlar ve 34 karakterdir; Ethereum veya BSC adresi 0x ile başlar ve 42 karakterdir.",
	},
	"chain_unavailable": {
		"%s is not available yet. TRON works today.",
		"%s henüz kullanılamıyor. Şu an TRON destekleniyor.",
	},
	"no_plan": {
		"You have no active plan. Your trial or subscription has ended.\n\nChoose a plan to continue:",
		"Aktif planınız yok. Deneme veya aboneliğiniz sona erdi.\n\nDevam etmek için bir plan seçin:",
	},
	"feature_locked": {
		"%s is part of a higher plan. Your plan is %s.",
		"%s daha üst bir planda var. Sizin planınız: %s.",
	},
	"busy": {
		"Your previous request is still running. I will answer it first.",
		"Önceki isteğiniz hâlâ çalışıyor. Önce onu yanıtlayacağım.",
	},
	"limit_reached": {
		"You have used all %d screens for today. The count resets at 00:00 UTC.\n\nNeed more? Upgrade:",
		"Bugünkü %d taramanın hepsini kullandınız. Sayaç 00:00 UTC'de sıfırlanır.\n\nDaha fazlası mı lazım? Planı yükseltin:",
	},
	"screening": {"Screening %s...", "%s taranıyor..."},
	"screen_failed": {
		"Could not screen that address. This did not count toward your daily limit.\n\n%s",
		"Bu adres taranamadı. Günlük sınırınızdan düşülmedi.\n\n%s",
	},
	"screens_left": {"%d of %d screens left today.", "Bugün %d / %d tarama hakkınız kaldı."},
	"pdf_caption":  {"Risk report for %s", "%s için risk raporu"},

	// --- plans and status ---
	"plans_title":   {"Plans, billed monthly:\n", "Aylık planlar:\n"},
	"plan_line":     {"\n%s: %d screens a day", "\n%s: günde %d tarama"},
	"feat_details":  {", /details breakdown", ", /details dökümü"},
	"feat_pdf":      {", PDF reports", ", PDF rapor"},
	"feat_watches":  {", watch %d addresses", ", %d adres izleme"},
	"feat_batch":    {", batch files of %d", ", %d adreslik toplu tarama"},
	"feat_api":      {", API access", ", API erişimi"},
	"price_stars":   {"\n  %d Stars a month, renews automatically", "\n  Aylık %d Stars, otomatik yenilenir"},
	"price_usdt":    {"\n  or %s USDT (TRC-20) for %d days", "\n  ya da %[2]d gün için %[1]s USDT (TRC-20)"},
	"your_plan":     {"\nYour plan: %s until %s.", "\nPlanınız: %s, %s tarihine kadar."},
	"renews":        {" It renews automatically.", " Otomatik yenilenir."},
	"no_days_lost":  {"\n\nPaying early never loses days: a new period starts when the current one ends.", "\n\nErken ödemek gün kaybettirmez: yeni dönem mevcut dönem bitince başlar."},
	"btn_active":    {"✅ %s active", "✅ %s aktif"},
	"btn_stars":     {"⭐ %s · %d Stars/mo", "⭐ %s · %d Stars/ay"},
	"btn_usdt":      {"💵 %s · %s USDT", "💵 %s · %s USDT"},
	"btn_open_app":  {"Open the app", "Uygulamayı aç"},
	"status_admin":  {"Admin: every feature, no daily limit.\nYour user id: %d", "Yönetici: tüm özellikler açık, günlük sınır yok.\nKullanıcı kimliğiniz: %d"},
	"status_none":   {"No active plan.\n\n/plans to subscribe.", "Aktif plan yok.\n\nAbone olmak için /plans."},
	"status_plan":   {"Plan: %s (%s)\nActive until: %s\n", "Plan: %s (%s)\nBitiş: %s\n"},
	"status_renews": {"Renews automatically. /cancel to stop.\n", "Otomatik yenilenir. Durdurmak için /cancel.\n"},
	"status_manual": {"Does not renew automatically. /plans to extend.\n", "Otomatik yenilenmez. Uzatmak için /plans.\n"},
	"status_today":  {"Today: %d of %d screens used (resets 00:00 UTC)\n", "Bugün: %d / %d tarama kullanıldı (00:00 UTC'de sıfırlanır)\n"},
	"status_watch":  {"Watching: %d of %d addresses\n", "İzlenen: %d / %d adres\n"},
	"status_id":     {"\nYour user id: %d", "\nKullanıcı kimliğiniz: %d"},
	"src_trial":     {"free trial", "ücretsiz deneme"},
	"src_stars":     {"Telegram Stars", "Telegram Stars"},
	"src_usdt":      {"USDT", "USDT"},
	"src_grant":     {"granted", "tanımlandı"},

	// --- cancel and payments ---
	"cancel_none": {
		"Nothing renews automatically, so there is nothing to cancel. USDT payments and trials never renew by themselves.",
		"Otomatik yenilenen bir şey yok, iptal edilecek bir şey de yok. USDT ödemeleri ve denemeler kendiliğinden yenilenmez.",
	},
	"cancel_failed": {
		"Telegram did not accept the cancellation. You can also cancel in Telegram: Settings > My Stars > Subscriptions. Or contact %s",
		"Telegram iptali kabul etmedi. Telegram'dan da iptal edebilirsiniz: Ayarlar > Yıldızlarım > Abonelikler. Ya da %s ile iletişime geçin",
	},
	"cancel_done": {
		"Automatic renewal is cancelled. You will not be charged again.",
		"Otomatik yenileme iptal edildi. Tekrar ücret alınmayacak.",
	},
	"cancel_until": {"\n\nYour %s plan stays active until %s.", "\n\n%s planınız %s tarihine kadar aktif kalır."},
	"support": {
		"Support: %s\n\nPlease include your user id (%d) and, for a payment, the date and amount.",
		"Destek: %s\n\nLütfen kullanıcı kimliğinizi (%d) ve ödemeyle ilgiliyse tarih ile tutarı yazın.",
	},
	"paysupport":      {"Payment help: %s\n\nPlease include your user id (%d)", "Ödeme desteği: %s\n\nLütfen kullanıcı kimliğinizi (%d) belirtin"},
	"paysupport_list": {" and the payment in question. Your payments:\n", " ve ilgili ödemeyi yazın. Ödemeleriniz:\n"},
	"paysupport_note": {
		"\n\nUSDT sent with the wrong amount or after an invoice expired is kept on record; support can apply it by hand.",
		"\n\nYanlış tutarla ya da fatura süresi dolduktan sonra gönderilen USDT kayıt altında tutulur; destek bunu elle tanımlayabilir.",
	},
	"refunded_tag":  {"  (refunded)", "  (iade edildi)"},
	"offer_gone":    {"This plan is no longer offered. Please open /plans again.", "Bu plan artık sunulmuyor. Lütfen /plans'ı yeniden açın."},
	"price_changed": {"The price of this plan has changed. Please open /plans again.", "Bu planın fiyatı değişti. Lütfen /plans'ı yeniden açın."},
	"paid_failed": {
		"Your payment went through, but I could not activate your plan. Support has been told and will fix it; you can also write to %s",
		"Ödemeniz alındı ama planınızı etkinleştiremedim. Destek ekibine haber verildi ve düzeltecek; %s adresine de yazabilirsiniz",
	},
	"paid_thanks": {
		"Thank you! %s is active until %s and renews automatically. /cancel stops renewal at any time.",
		"Teşekkürler! %s planı %s tarihine kadar aktif ve otomatik yenilenir. /cancel ile yenilemeyi istediğiniz an durdurabilirsiniz.",
	},
	"paid_renewed": {"Your %s subscription renewed. Active until %s.", "%s aboneliğiniz yenilendi. %s tarihine kadar aktif."},
	"refunded": {
		"Your payment was refunded, and the plan it paid for has ended.",
		"Ödemeniz iade edildi ve ödediği plan sona erdi.",
	},
	"option_gone":    {"This option is no longer available.", "Bu seçenek artık mevcut değil."},
	"invoice_failed": {"Could not create an invoice. Please try again in a few minutes.", "Fatura oluşturulamadı. Lütfen birkaç dakika sonra tekrar deneyin."},
	"usdt_invoice": {
		`%s for %d days, paid in USDT:

Send exactly %s USDT
Network: TRON (TRC-20) only
To the address in the next message

The exact amount identifies your payment. Send %s, not a rounded figure. If you withdraw from an exchange, make sure the amount that arrives is %s: the exchange's fee must not come out of it.

This invoice is valid for %d minutes. I will message you here as soon as the payment is confirmed, usually within 2 minutes.`,
		`%[1]s, %[2]d gün, USDT ile:

Tam olarak %[3]s USDT gönderin
Ağ: yalnızca TRON (TRC-20)
Adres bir sonraki mesajda

Ödemenizi tam tutar tanımlar. Yuvarlamadan tam %[4]s gönderin. Bir borsadan çekiyorsanız, ulaşan tutarın %[5]s olduğundan emin olun: borsanın komisyonu bu tutardan düşmemeli.

Bu fatura %[6]d dakika geçerli. Ödeme onaylanır onaylanmaz, genellikle 2 dakika içinde, size buradan yazacağım.`,
	},
	"usdt_received": {
		"Payment received: %s USDT. Your %s plan is active until %s. Thank you!\n\nUSDT periods do not renew by themselves; /plans extends at any time without losing days.",
		"Ödeme alındı: %s USDT. %s planınız %s tarihine kadar aktif. Teşekkürler!\n\nUSDT dönemleri kendiliğinden yenilenmez; /plans ile istediğiniz an gün kaybetmeden uzatabilirsiniz.",
	},
	"granted": {
		"You have been given the %s plan until %s. Send an address to start.",
		"Size %s planı %s tarihine kadar tanımlandı. Başlamak için bir adres gönderin.",
	},

	// --- invoice / product text shown in Telegram's payment sheet ---
	"inv_title":   {"Risk screening %s", "Risk taraması %s"},
	"inv_screens": {"%d address screens a day", "Günde %d adres taraması"},
	"inv_details": {", full breakdowns", ", tam dökümler"},
	"inv_pdf":     {", PDF reports", ", PDF raporlar"},
	"inv_renew":   {". Renews every 30 days; cancel any time with /cancel.", ". 30 günde bir yenilenir; /cancel ile istediğiniz an iptal edin."},

	// --- watches ---
	"watch_usage": {"Usage: /watch <address> [name]", "Kullanım: /watch <adres> [isim]"},
	"watch_added": {
		"Watching %s. I will check it every %s and message you here if its risk worsens: a higher band, a new high-risk category, or a direct listing. (%d of %d)",
		"%s izleniyor. Her %s bir kontrol edip riski kötüleşirse size buradan yazacağım: daha yüksek seviye, yeni bir yüksek riskli kategori ya da doğrudan listeleme. (%d / %d)",
	},
	"watch_limit": {
		"Your plan watches up to %d addresses, and they are all in use. /unwatch one, or upgrade:",
		"Planınız en fazla %d adres izler ve hepsi kullanımda. Birini /unwatch ile bırakın ya da planı yükseltin:",
	},
	"watch_removed":  {"Stopped watching %s.", "%s artık izlenmiyor."},
	"watch_notfound": {"You are not watching that address.", "Bu adresi izlemiyorsunuz."},
	"watches_none":   {"You are not watching any address. /watch <address> to start.", "Hiç adres izlemiyorsunuz. Başlamak için /watch <adres>."},
	"watches_title":  {"Watching %d of %d addresses:\n", "İzlenen adresler (%d / %d):\n"},
	"watch_pending":  {"first check pending", "ilk kontrol bekleniyor"},
	"alert_title":    {"⚠️ Risk changed for a watched address\n\n%s%s\n", "⚠️ İzlenen bir adresin riski değişti\n\n%s%s\n"},
	"alert_band":     {"• Band: %s → %s (score %.1f → %.1f)\n", "• Seviye: %s → %s (puan %.1f → %.1f)\n"},
	"alert_category": {"• New exposure: %s\n", "• Yeni maruziyet: %s\n"},
	"alert_listed":   {"• The address itself is now directly listed\n", "• Adresin kendisi artık doğrudan listede\n"},
	"alert_footer":   {"\nSend the address to see the full result.", "\nTam sonucu görmek için adresi gönderin."},

	// --- history ---
	"history_none":  {"No screens yet. Send an address to start.", "Henüz tarama yok. Başlamak için bir adres gönderin."},
	"history_title": {"Your recent screens:\n", "Son taramalarınız:\n"},

	// --- batch ---
	"batch_locked": {"Batch screening is part of the Pro plan.", "Toplu tarama Pro planında var."},
	"batch_bad_file": {
		"Send a .txt or .csv file with one address per line, up to 1 MB.",
		"Her satırda bir adres olan, en fazla 1 MB'lık bir .txt veya .csv dosyası gönderin.",
	},
	"batch_empty":    {"I found no addresses in that file.", "Dosyada hiç adres bulamadım."},
	"batch_too_many": {"That file has %d addresses; your plan screens up to %d per batch.", "Dosyada %d adres var; planınız toplu taramada en fazla %d adres tarar."},
	"batch_over_day": {"That is %d addresses, but only %d screens are left today.", "Dosyada %d adres var ama bugün yalnızca %d tarama hakkınız kaldı."},
	"batch_started":  {"Screening %d addresses (%d skipped as invalid). I will send the results here as a file.", "%d adres taranıyor (%d geçersiz adres atlandı). Sonuçları buraya dosya olarak göndereceğim."},
	"batch_done":     {"Batch done: %d screened, %d failed. High: %d · Medium: %d · Low: %d", "Toplu tarama bitti: %d tarandı, %d başarısız. Yüksek: %d · Orta: %d · Düşük: %d"},
	"batch_stopped":  {"The batch stopped early: %s", "Toplu tarama erken durdu: %s"},

	// --- API keys ---
	"apikey_locked": {"API access is part of the Business plan.", "API erişimi Business planında var."},
	"apikey_none":   {"No API keys yet. /apikey new <name> creates one.", "Henüz API anahtarı yok. /apikey new <isim> ile oluşturun."},
	"apikey_list":   {"Your API keys:\n", "API anahtarlarınız:\n"},
	"apikey_new": {
		"Your new API key (shown only now; store it safely):\n\n%s\n\nUse it as: Authorization: Bearer <key>\nPOST %s/api/v1/screen {\"address\": \"T...\"}",
		"Yeni API anahtarınız (yalnızca şimdi gösteriliyor, güvenle saklayın):\n\n%s\n\nKullanım: Authorization: Bearer <anahtar>\nPOST %s/api/v1/screen {\"address\": \"T...\"}",
	},
	"apikey_revoked":  {"Key %d revoked.", "%d numaralı anahtar iptal edildi."},
	"apikey_usage":    {"Usage: /apikey, /apikey new <name>, /apikey revoke <id>", "Kullanım: /apikey, /apikey new <isim>, /apikey revoke <id>"},
	"apikey_too_many": {"You already hold the maximum of 10 keys. Revoke one first.", "En fazla 10 anahtar tutabilirsiniz. Önce birini iptal edin."},

	// --- language and app ---
	"lang_pick": {"Choose your language:", "Dilinizi seçin:"},
	"lang_set":  {"Language set to English.", "Dil Türkçe olarak ayarlandı."},
	"app_open":  {"Open the app for charts, history, watches and your plan:", "Grafikler, geçmiş, izlemeler ve planınız için uygulamayı açın:"},
	"app_off":   {"The app is not available yet. Everything works here in the chat.", "Uygulama henüz kullanılamıyor. Her şey burada, sohbette çalışıyor."},

	// --- follow-up ---
	"fu_state":  {"%s %.1f/100, coverage %s", "%s %.1f/100, kapsam %s"},
	"fu_title":  {"✅ Final result for %s\n\n", "✅ %s için kesin sonuç\n\n"},
	"fu_change": {"First answer: %s\nNow: %s\n\n", "İlk sonuç: %s\nŞimdi: %s\n\n"},
	"fu_same": {
		"✅ Tracing finished for %s. The result did not change: %s.",
		"✅ %s için izleme tamamlandı. Sonuç değişmedi: %s.",
	},

	// --- details view headings ---
	"d_band":     {"Exposure: %s\nExposure score: %.1f / 100\n", "Maruziyet: %s\nMaruziyet skoru: %.1f / 100\n"},
	"d_coverage": {"Coverage: %.1f%%\n", "Kapsam: %%%.1f\n"},
}
