package main

import (
	"fmt"
	"slices"
	"strings"
)

// The bot's messages in English, Turkish and Russian (docs/DECISIONS.md D27,
// D33). A user's language is their /language choice, else their Telegram
// client's.

const (
	langEN = "en"
	langTR = "tr"
	langRU = "ru"
)

// ruClients are the Telegram language codes answered in Russian: Russian,
// and the CIS languages whose speakers commonly read it.
var ruClients = []string{"ru", "uk", "be", "kk", "uz", "ky", "tg"}

// normLang maps a stored choice or a Telegram language_code to en, tr or ru.
func normLang(chosen, client string) string {
	for _, l := range []string{chosen, client} {
		l = strings.ToLower(strings.TrimSpace(l))
		if i := strings.IndexAny(l, "-_"); i >= 0 {
			l = l[:i]
		}
		switch {
		case l == langTR:
			return langTR
		case l == langEN:
			return langEN
		case slices.Contains(ruClients, l):
			return langRU
		}
	}
	return langEN
}

// t renders a message. A key missing from a language falls back to English;
// a test keeps all three complete.
func t(lang, key string, args ...any) string {
	m, ok := messages[key]
	if !ok {
		return key
	}
	format := m[0]
	switch {
	case lang == langTR && m[1] != "":
		format = m[1]
	case lang == langRU && m[2] != "":
		format = m[2]
	}
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// messages: key -> {English, Turkish, Russian}.
var messages = map[string][3]string{
	// --- general ---
	"error_ours": {
		"Something went wrong on our side. Please try again in a minute.",
		"Bizim tarafımızda bir sorun oluştu. Lütfen bir dakika sonra tekrar deneyin.",
		"Что-то пошло не так на нашей стороне. Попробуйте ещё раз через минуту.",
	},
	"unknown_command": {
		"Unknown command. Send an address, or /help.",
		"Bilinmeyen komut. Bir adres gönderin ya da /help yazın.",
		"Неизвестная команда. Отправьте адрес или /help.",
	},
	"welcome": {
		"Hi %s. Send me a blockchain address and I will screen it: where its money came from, where it went, and which risk categories it touches.\n\n",
		"Merhaba %s. Bana bir blokzincir adresi gönderin, tarayayım: parası nereden geldi, nereye gitti ve hangi risk kategorilerine dokunuyor.\n\n",
		"Здравствуйте, %s. Отправьте мне адрес в блокчейне, и я его проверю: откуда пришли средства, куда ушли и с какими категориями риска они связаны.\n\n",
	},
	"welcome_admin": {
		"You are an admin: every feature, no daily limit.\n",
		"Yöneticisiniz: tüm özellikler açık, günlük sınır yok.\n",
		"Вы администратор: все функции, без дневного лимита.\n",
	},
	"welcome_footer": {
		"\n/app opens the full app · /plans to subscribe · /help for everything else",
		"\n/app tam uygulamayı açar · /plans abonelik · /help diğer her şey",
		"\n/app — полное приложение · /plans — подписка · /help — всё остальное",
	},
	"there": {"there", "sevgili kullanıcı", "коллега"},
	"trial_started": {
		"Your free %d-day trial has started: the %s plan, %d screens a day.",
		"%d günlük ücretsiz denemeniz başladı: %s planı, günde %d tarama.",
		"Бесплатный пробный период начался: %d дн., тариф %s, проверок в день: %d.",
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
/language            English / Türkçe / Русский
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
/language            English / Türkçe / Русский
/terms  /support  /paysupport

Toplu tarama: her satırda bir adres olan .txt veya .csv dosyası gönderin (Pro).

Bu, açık verilere dayalı otomatik bir ön taramadır. Düzenlenmiş bir AML kararı değildir ve öyle kullanılmamalıdır.

Puanı her zaman kapsam oranıyla birlikte okuyun. Düşük kapsam, izlenen değerin çoğunun bilinen bir kuruluşa bağlanamadığı anlamına gelir: bilinmiyor, temiz değil.`,
		`Проверка адресов на риск.

Отправьте адрес — и получите сводку по его связям: откуда пришли средства, куда ушли и с какими категориями риска они связаны.

/app                 открыть приложение
/details <адрес>     полная разбивка с путями (Pro)
/pdf <адрес>         PDF-отчёт на одну страницу (Pro)
/watch <адрес>       сообщить, если риск изменится
/watches  /unwatch <адрес>
/history             ваши последние проверки
/plans  /status  /cancel
/apikey              API-ключи (Business)
/language            English / Türkçe / Русский
/terms  /support  /paysupport

Пакетная проверка: отправьте файл .txt или .csv, по одному адресу в строке (Pro).

Это автоматическая предварительная проверка на основе открытых данных. Она не является регулируемым AML-заключением и не должна использоваться в этом качестве.

Всегда смотрите на балл вместе с показателем покрытия. Низкое покрытие означает, что большую часть отслеженных средств не удалось связать с известным субъектом: это «неизвестно», а не «чисто».`,
	},

	// --- screening ---
	"usage_cmd": {"Usage: %s <address>", "Kullanım: %s <adres>", "Использование: %s <адрес>"},
	"bad_address": {
		"That does not look like a blockchain address. A TRON address starts with T and is 34 characters; an Ethereum or BSC address starts with 0x and is 42.",
		"Bu bir blokzincir adresine benzemiyor. TRON adresi T ile başlar ve 34 karakterdir; Ethereum veya BSC adresi 0x ile başlar ve 42 karakterdir.",
		"Это не похоже на адрес в блокчейне. Адрес TRON начинается с T и состоит из 34 символов; адрес Ethereum или BSC начинается с 0x и состоит из 42.",
	},
	"chain_unavailable": {
		"%s is not available yet. TRON works today.",
		"%s henüz kullanılamıyor. Şu an TRON destekleniyor.",
		"Сеть %s пока не поддерживается. Сейчас работает TRON.",
	},
	"no_plan": {
		"You have no active plan. Your trial or subscription has ended.\n\nChoose a plan to continue:",
		"Aktif planınız yok. Deneme veya aboneliğiniz sona erdi.\n\nDevam etmek için bir plan seçin:",
		"У вас нет активного тарифа: пробный период или подписка закончились.\n\nВыберите тариф, чтобы продолжить:",
	},
	"feature_locked": {
		"%s is part of a higher plan. Your plan is %s.",
		"%s daha üst bir planda var. Sizin planınız: %s.",
		"%s входит в более высокий тариф. Ваш тариф: %s.",
	},
	"busy": {
		"Your previous request is still running. I will answer it first.",
		"Önceki isteğiniz hâlâ çalışıyor. Önce onu yanıtlayacağım.",
		"Ваш предыдущий запрос ещё выполняется. Сначала отвечу на него.",
	},
	"limit_reached": {
		"You have used all %d screens for today. The count resets at 00:00 UTC.\n\nNeed more? Upgrade:",
		"Bugünkü %d taramanın hepsini kullandınız. Sayaç 00:00 UTC'de sıfırlanır.\n\nDaha fazlası mı lazım? Planı yükseltin:",
		"Дневной лимит проверок исчерпан (%d). Счётчик обнуляется в 00:00 UTC.\n\nНужно больше? Повысьте тариф:",
	},
	"screening": {"Screening %s...", "%s taranıyor...", "Проверяю %s..."},
	"screen_failed": {
		"Could not screen that address. This did not count toward your daily limit.\n\n%s",
		"Bu adres taranamadı. Günlük sınırınızdan düşülmedi.\n\n%s",
		"Не удалось проверить этот адрес. Попытка не засчитана в дневной лимит.\n\n%s",
	},
	"screens_left": {"%d of %d screens left today.", "Bugün %d / %d tarama hakkınız kaldı.", "Осталось проверок на сегодня: %d из %d."},
	"pdf_caption":  {"Risk report for %s", "%s için risk raporu", "Отчёт о рисках для %s"},

	// --- plans and status ---
	"plans_title":   {"Plans, billed monthly:\n", "Aylık planlar:\n", "Тарифы, оплата помесячно:\n"},
	"plan_line":     {"\n%s: %d screens a day", "\n%s: günde %d tarama", "\n%s — проверок в день: %d"},
	"feat_details":  {", /details breakdown", ", /details dökümü", ", разбивка /details"},
	"feat_pdf":      {", PDF reports", ", PDF rapor", ", PDF-отчёты"},
	"feat_watches":  {", watch %d addresses", ", %d adres izleme", ", мониторинг адресов: %d"},
	"feat_batch":    {", batch files of %d", ", %d adreslik toplu tarama", ", адресов в пакете: до %d"},
	"feat_api":      {", API access", ", API erişimi", ", доступ к API"},
	"price_stars":   {"\n  %d Stars a month, renews automatically", "\n  Aylık %d Stars, otomatik yenilenir", "\n  %d Stars в месяц, продлевается автоматически"},
	"price_usdt":    {"\n  or %s USDT (TRC-20) for %d days", "\n  ya da %[2]d gün için %[1]s USDT (TRC-20)", "\n  или %s USDT (TRC-20) за %d дн."},
	"your_plan":     {"\nYour plan: %s until %s.", "\nPlanınız: %s, %s tarihine kadar.", "\nВаш тариф: %s до %s."},
	"renews":        {" It renews automatically.", " Otomatik yenilenir.", " Продлевается автоматически."},
	"no_days_lost":  {"\n\nPaying early never loses days: a new period starts when the current one ends.", "\n\nErken ödemek gün kaybettirmez: yeni dönem mevcut dönem bitince başlar.", "\n\nРанняя оплата не сокращает срок: новый период начнётся, когда закончится текущий."},
	"btn_active":    {"✅ %s active", "✅ %s aktif", "✅ %s активен"},
	"btn_stars":     {"⭐ %s · %d Stars/mo", "⭐ %s · %d Stars/ay", "⭐ %s · %d Stars/мес"},
	"btn_usdt":      {"💵 %s · %s USDT", "💵 %s · %s USDT", "💵 %s · %s USDT"},
	"btn_open_app":  {"Open the app", "Uygulamayı aç", "Открыть приложение"},
	"status_admin":  {"Admin: every feature, no daily limit.\nYour user id: %d", "Yönetici: tüm özellikler açık, günlük sınır yok.\nKullanıcı kimliğiniz: %d", "Администратор: все функции, без дневного лимита.\nВаш ID пользователя: %d"},
	"status_none":   {"No active plan.\n\n/plans to subscribe.", "Aktif plan yok.\n\nAbone olmak için /plans.", "Нет активного тарифа.\n\n/plans — оформить подписку."},
	"status_plan":   {"Plan: %s (%s)\nActive until: %s\n", "Plan: %s (%s)\nBitiş: %s\n", "Тариф: %s (%s)\nДействует до: %s\n"},
	"status_renews": {"Renews automatically. /cancel to stop.\n", "Otomatik yenilenir. Durdurmak için /cancel.\n", "Продлевается автоматически. /cancel — отключить.\n"},
	"status_manual": {"Does not renew automatically. /plans to extend.\n", "Otomatik yenilenmez. Uzatmak için /plans.\n", "Не продлевается автоматически. /plans — продлить.\n"},
	"status_today":  {"Today: %d of %d screens used (resets 00:00 UTC)\n", "Bugün: %d / %d tarama kullanıldı (00:00 UTC'de sıfırlanır)\n", "Сегодня: проверок использовано %d из %d (сброс в 00:00 UTC)\n"},
	"status_watch":  {"Watching: %d of %d addresses\n", "İzlenen: %d / %d adres\n", "Адресов на мониторинге: %d из %d\n"},
	"status_id":     {"\nYour user id: %d", "\nKullanıcı kimliğiniz: %d", "\nВаш ID пользователя: %d"},
	"src_trial":     {"free trial", "ücretsiz deneme", "пробный период"},
	"src_stars":     {"Telegram Stars", "Telegram Stars", "Telegram Stars"},
	"src_usdt":      {"USDT", "USDT", "USDT"},
	"src_grant":     {"granted", "tanımlandı", "назначен поддержкой"},

	// --- cancel and payments ---
	"cancel_none": {
		"Nothing renews automatically, so there is nothing to cancel. USDT payments and trials never renew by themselves.",
		"Otomatik yenilenen bir şey yok, iptal edilecek bir şey de yok. USDT ödemeleri ve denemeler kendiliğinden yenilenmez.",
		"Автопродление не подключено, отменять нечего. Оплата в USDT и пробный период сами не продлеваются.",
	},
	"cancel_failed": {
		"Telegram did not accept the cancellation. You can also cancel in Telegram: Settings > My Stars > Subscriptions. Or contact %s",
		"Telegram iptali kabul etmedi. Telegram'dan da iptal edebilirsiniz: Ayarlar > Yıldızlarım > Abonelikler. Ya da %s ile iletişime geçin",
		"Telegram не принял отмену. Отменить можно и в самом Telegram: Настройки > Мои звёзды > Подписки. Или напишите %s",
	},
	"cancel_done": {
		"Automatic renewal is cancelled. You will not be charged again.",
		"Otomatik yenileme iptal edildi. Tekrar ücret alınmayacak.",
		"Автопродление отключено. Повторных списаний не будет.",
	},
	"cancel_until": {"\n\nYour %s plan stays active until %s.", "\n\n%s planınız %s tarihine kadar aktif kalır.", "\n\nТариф %s действует до %s."},
	"support": {
		"Support: %s\n\nPlease include your user id (%d) and, for a payment, the date and amount.",
		"Destek: %s\n\nLütfen kullanıcı kimliğinizi (%d) ve ödemeyle ilgiliyse tarih ile tutarı yazın.",
		"Поддержка: %s\n\nУкажите, пожалуйста, ваш ID пользователя (%d), а если вопрос об оплате — её дату и сумму.",
	},
	"paysupport":      {"Payment help: %s\n\nPlease include your user id (%d)", "Ödeme desteği: %s\n\nLütfen kullanıcı kimliğinizi (%d) belirtin", "Помощь с оплатой: %s\n\nУкажите, пожалуйста, ваш ID пользователя (%d)"},
	"paysupport_list": {" and the payment in question. Your payments:\n", " ve ilgili ödemeyi yazın. Ödemeleriniz:\n", " и платёж, о котором идёт речь. Ваши платежи:\n"},
	"paysupport_note": {
		"\n\nUSDT sent with the wrong amount or after an invoice expired is kept on record; support can apply it by hand.",
		"\n\nYanlış tutarla ya da fatura süresi dolduktan sonra gönderilen USDT kayıt altında tutulur; destek bunu elle tanımlayabilir.",
		"\n\nUSDT, отправленные с неверной суммой или после истечения срока счёта, сохраняются в учёте; поддержка может зачислить их вручную.",
	},
	"refunded_tag":  {"  (refunded)", "  (iade edildi)", "  (возвращён)"},
	"offer_gone":    {"This plan is no longer offered. Please open /plans again.", "Bu plan artık sunulmuyor. Lütfen /plans'ı yeniden açın.", "Этот тариф больше не предлагается. Откройте /plans ещё раз."},
	"price_changed": {"The price of this plan has changed. Please open /plans again.", "Bu planın fiyatı değişti. Lütfen /plans'ı yeniden açın.", "Цена этого тарифа изменилась. Откройте /plans ещё раз."},
	"paid_failed": {
		"Your payment went through, but I could not activate your plan. Support has been told and will fix it; you can also write to %s",
		"Ödemeniz alındı ama planınızı etkinleştiremedim. Destek ekibine haber verildi ve düzeltecek; %s adresine de yazabilirsiniz",
		"Оплата прошла, но активировать тариф не удалось. Поддержка уже уведомлена и всё исправит; вы также можете написать %s",
	},
	"paid_thanks": {
		"Thank you! %s is active until %s and renews automatically. /cancel stops renewal at any time.",
		"Teşekkürler! %s planı %s tarihine kadar aktif ve otomatik yenilenir. /cancel ile yenilemeyi istediğiniz an durdurabilirsiniz.",
		"Спасибо! Тариф %s действует до %s и продлевается автоматически. Командой /cancel автопродление можно отключить в любой момент.",
	},
	"paid_renewed": {"Your %s subscription renewed. Active until %s.", "%s aboneliğiniz yenilendi. %s tarihine kadar aktif.", "Подписка %s продлена. Действует до %s."},
	"refunded": {
		"Your payment was refunded, and the plan it paid for has ended.",
		"Ödemeniz iade edildi ve ödediği plan sona erdi.",
		"Платёж возвращён, оплаченный им тариф завершён.",
	},
	"option_gone":    {"This option is no longer available.", "Bu seçenek artık mevcut değil.", "Этот вариант больше недоступен."},
	"invoice_failed": {"Could not create an invoice. Please try again in a few minutes.", "Fatura oluşturulamadı. Lütfen birkaç dakika sonra tekrar deneyin.", "Не удалось выставить счёт. Попробуйте ещё раз через несколько минут."},
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
		`Тариф %[1]s на %[2]d дн., оплата в USDT:

Отправьте ровно %[3]s USDT
Сеть: только TRON (TRC-20)
На адрес из следующего сообщения

Платёж распознаётся по точной сумме. Отправьте %[4]s, без округления. Если выводите с биржи, убедитесь, что поступит именно %[5]s: комиссия биржи не должна списываться из этой суммы.

Счёт действителен %[6]d мин. Как только платёж подтвердится (обычно в течение 2 минут), я напишу вам здесь.`,
	},
	"usdt_received": {
		"Payment received: %s USDT. Your %s plan is active until %s. Thank you!\n\nUSDT periods do not renew by themselves; /plans extends at any time without losing days.",
		"Ödeme alındı: %s USDT. %s planınız %s tarihine kadar aktif. Teşekkürler!\n\nUSDT dönemleri kendiliğinden yenilenmez; /plans ile istediğiniz an gün kaybetmeden uzatabilirsiniz.",
		"Платёж получен: %s USDT. Тариф %s действует до %s. Спасибо!\n\nПериоды, оплаченные в USDT, сами не продлеваются; через /plans можно продлить в любой момент без потери дней.",
	},
	"granted": {
		"You have been given the %s plan until %s. Send an address to start.",
		"Size %s planı %s tarihine kadar tanımlandı. Başlamak için bir adres gönderin.",
		"Вам назначен тариф %s до %s. Отправьте адрес, чтобы начать.",
	},

	// --- invoice / product text shown in Telegram's payment sheet ---
	"inv_title":   {"Risk screening %s", "Risk taraması %s", "Проверка адресов: %s"},
	"inv_screens": {"%d address screens a day", "Günde %d adres taraması", "Проверок адресов в день: %d"},
	"inv_details": {", full breakdowns", ", tam dökümler", ", полные разбивки"},
	"inv_pdf":     {", PDF reports", ", PDF raporlar", ", PDF-отчёты"},
	"inv_renew":   {". Renews every 30 days; cancel any time with /cancel.", ". 30 günde bir yenilenir; /cancel ile istediğiniz an iptal edin.", ". Продлевается каждые 30 дней; отменить можно в любой момент командой /cancel."},

	// --- watches ---
	"watch_usage": {"Usage: /watch <address> [name]", "Kullanım: /watch <adres> [isim]", "Использование: /watch <адрес> [название]"},
	"watch_added": {
		"Watching %s. I will check it every %s and message you here if its risk worsens: a higher band, a new high-risk category, or a direct listing. (%d of %d)",
		"%s izleniyor. Her %s bir kontrol edip riski kötüleşirse size buradan yazacağım: daha yüksek seviye, yeni bir yüksek riskli kategori ya da doğrudan listeleme. (%d / %d)",
		"%s на мониторинге. Я буду проверять его каждые %s и напишу здесь, если риск вырастет: более высокий уровень, новая категория высокого риска или прямое внесение в список. (%d из %d)",
	},
	"watch_limit": {
		"Your plan watches up to %d addresses, and they are all in use. /unwatch one, or upgrade:",
		"Planınız en fazla %d adres izler ve hepsi kullanımda. Birini /unwatch ile bırakın ya da planı yükseltin:",
		"Лимит мониторинга по вашему тарифу (%d) исчерпан. Уберите адрес через /unwatch или повысьте тариф:",
	},
	"watch_removed":  {"Stopped watching %s.", "%s artık izlenmiyor.", "Мониторинг %s остановлен."},
	"watch_notfound": {"You are not watching that address.", "Bu adresi izlemiyorsunuz.", "Этот адрес не на мониторинге."},
	"watches_none":   {"You are not watching any address. /watch <address> to start.", "Hiç adres izlemiyorsunuz. Başlamak için /watch <adres>.", "Вы не отслеживаете ни одного адреса. /watch <адрес> — начать."},
	"watches_title":  {"Watching %d of %d addresses:\n", "İzlenen adresler (%d / %d):\n", "Адреса на мониторинге (%d из %d):\n"},
	"watch_pending":  {"first check pending", "ilk kontrol bekleniyor", "ожидает первой проверки"},
	"alert_title":    {"⚠️ Risk changed for a watched address\n\n%s%s\n", "⚠️ İzlenen bir adresin riski değişti\n\n%s%s\n", "⚠️ Изменился риск отслеживаемого адреса\n\n%s%s\n"},
	"alert_band":     {"• Band: %s → %s (score %.1f → %.1f)\n", "• Seviye: %s → %s (puan %.1f → %.1f)\n", "• Уровень: %s → %s (балл %.1f → %.1f)\n"},
	"alert_category": {"• New exposure: %s\n", "• Yeni maruziyet: %s\n", "• Новая связь с категорией: %s\n"},
	"alert_listed":   {"• The address itself is now directly listed\n", "• Adresin kendisi artık doğrudan listede\n", "• Сам адрес теперь напрямую внесён в список\n"},
	"alert_footer":   {"\nSend the address to see the full result.", "\nTam sonucu görmek için adresi gönderin.", "\nОтправьте адрес, чтобы увидеть полный результат."},

	// --- history ---
	"history_none":  {"No screens yet. Send an address to start.", "Henüz tarama yok. Başlamak için bir adres gönderin.", "Проверок пока нет. Отправьте адрес, чтобы начать."},
	"history_title": {"Your recent screens:\n", "Son taramalarınız:\n", "Ваши последние проверки:\n"},

	// --- batch ---
	"batch_locked": {"Batch screening is part of the Pro plan.", "Toplu tarama Pro planında var.", "Пакетная проверка входит в тариф Pro."},
	"batch_bad_file": {
		"Send a .txt or .csv file with one address per line, up to 1 MB.",
		"Her satırda bir adres olan, en fazla 1 MB'lık bir .txt veya .csv dosyası gönderin.",
		"Отправьте файл .txt или .csv размером до 1 МБ, по одному адресу в строке.",
	},
	"batch_empty":    {"I found no addresses in that file.", "Dosyada hiç adres bulamadım.", "В файле не найдено ни одного адреса."},
	"batch_too_many": {"That file has %d addresses; your plan screens up to %d per batch.", "Dosyada %d adres var; planınız toplu taramada en fazla %d adres tarar.", "Адресов в файле: %d; ваш тариф допускает до %d за один пакет."},
	"batch_over_day": {"That is %d addresses, but only %d screens are left today.", "Dosyada %d adres var ama bugün yalnızca %d tarama hakkınız kaldı.", "Адресов: %d, а проверок на сегодня осталось только %d."},
	"batch_started":  {"Screening %d addresses (%d skipped as invalid). I will send the results here as a file.", "%d adres taranıyor (%d geçersiz adres atlandı). Sonuçları buraya dosya olarak göndereceğim.", "Проверяю адреса: %d (пропущено некорректных: %d). Результаты пришлю сюда файлом."},
	"batch_done":     {"Batch done: %d screened, %d failed. High: %d · Medium: %d · Low: %d", "Toplu tarama bitti: %d tarandı, %d başarısız. Yüksek: %d · Orta: %d · Düşük: %d", "Пакет готов: проверено %d, с ошибкой %d. Высокий: %d · Средний: %d · Низкий: %d"},
	"batch_stopped":  {"The batch stopped early: %s", "Toplu tarama erken durdu: %s", "Пакетная проверка остановлена досрочно: %s"},

	// --- API keys ---
	"apikey_locked": {"API access is part of the Business plan.", "API erişimi Business planında var.", "Доступ к API входит в тариф Business."},
	"apikey_none":   {"No API keys yet. /apikey new <name> creates one.", "Henüz API anahtarı yok. /apikey new <isim> ile oluşturun.", "API-ключей пока нет. /apikey new <название> — создать."},
	"apikey_list":   {"Your API keys:\n", "API anahtarlarınız:\n", "Ваши API-ключи:\n"},
	"apikey_new": {
		"Your new API key (shown only now; store it safely):\n\n%s\n\nUse it as: Authorization: Bearer <key>\nPOST %s/api/v1/screen {\"address\": \"T...\"}",
		"Yeni API anahtarınız (yalnızca şimdi gösteriliyor, güvenle saklayın):\n\n%s\n\nKullanım: Authorization: Bearer <anahtar>\nPOST %s/api/v1/screen {\"address\": \"T...\"}",
		"Ваш новый API-ключ (показывается только сейчас, сохраните его в надёжном месте):\n\n%s\n\nИспользование: Authorization: Bearer <ключ>\nPOST %s/api/v1/screen {\"address\": \"T...\"}",
	},
	"apikey_revoked":  {"Key %d revoked.", "%d numaralı anahtar iptal edildi.", "Ключ %d отозван."},
	"apikey_usage":    {"Usage: /apikey, /apikey new <name>, /apikey revoke <id>", "Kullanım: /apikey, /apikey new <isim>, /apikey revoke <id>", "Использование: /apikey, /apikey new <название>, /apikey revoke <id>"},
	"apikey_too_many": {"You already hold the maximum of 10 keys. Revoke one first.", "En fazla 10 anahtar tutabilirsiniz. Önce birini iptal edin.", "У вас уже максимальное число ключей — 10. Сначала отзовите один из них."},

	// --- language and app ---
	"lang_pick": {"Choose your language:", "Dilinizi seçin:", "Выберите язык:"},
	"lang_set":  {"Language set to English.", "Dil Türkçe olarak ayarlandı.", "Язык интерфейса: русский."},
	"app_open":  {"Open the app for charts, history, watches and your plan:", "Grafikler, geçmiş, izlemeler ve planınız için uygulamayı açın:", "Откройте приложение: графики, история, мониторинг и ваш тариф:"},
	"app_off":   {"The app is not available yet. Everything works here in the chat.", "Uygulama henüz kullanılamıyor. Her şey burada, sohbette çalışıyor.", "Приложение пока недоступно. Всё работает здесь, в чате."},

	// --- follow-up ---
	"fu_state":  {"%s %.1f/100, coverage %s", "%s %.1f/100, kapsam %s", "%s %.1f/100, покрытие %s"},
	"fu_title":  {"✅ Final result for %s\n\n", "✅ %s için kesin sonuç\n\n", "✅ Итоговый результат для %s\n\n"},
	"fu_change": {"First answer: %s\nNow: %s\n\n", "İlk sonuç: %s\nŞimdi: %s\n\n", "Первый ответ: %s\nСейчас: %s\n\n"},
	"fu_same": {
		"✅ Tracing finished for %s. The result did not change: %s.",
		"✅ %s için izleme tamamlandı. Sonuç değişmedi: %s.",
		"✅ Отслеживание для %s завершено. Результат не изменился: %s.",
	},

	// --- details view headings ---
	"d_band":     {"Exposure: %s\nExposure score: %.1f / 100\n", "Maruziyet: %s\nMaruziyet skoru: %.1f / 100\n", "Риск-экспозиция: %s\nБалл риск-экспозиции: %.1f / 100\n"},
	"d_coverage": {"Coverage: %.1f%%\n", "Kapsam: %%%.1f\n", "Покрытие: %.1f%%\n"},
}
