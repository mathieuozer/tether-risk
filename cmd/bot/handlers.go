package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/mozer/tether-risk/internal/billing"
)

// publicCommands is the menu Telegram shows next to the input field, per
// language.
var publicCommands = map[string][]botCommand{
	langEN: {
		{"app", "Open the app"},
		{"plans", "Plans and prices, subscribe"},
		{"status", "Your plan, renewal and today's usage"},
		{"watches", "Addresses you watch for risk changes"},
		{"history", "Your recent screens"},
		{"details", "Full breakdown: /details <address> (Pro)"},
		{"pdf", "One-page PDF report: /pdf <address> (Pro)"},
		{"cancel", "Stop automatic renewal"},
		{"language", "English / Türkçe / Русский"},
		{"help", "How to use this bot"},
		{"terms", "Terms of service"},
		{"support", "Contact support"},
		{"paysupport", "Help with a payment"},
	},
	langTR: {
		{"app", "Uygulamayı aç"},
		{"plans", "Planlar ve fiyatlar, abonelik"},
		{"status", "Planınız, yenileme ve bugünkü kullanım"},
		{"watches", "Risk değişimi için izlediğiniz adresler"},
		{"history", "Son taramalarınız"},
		{"details", "Tam döküm: /details <adres> (Pro)"},
		{"pdf", "Tek sayfa PDF rapor: /pdf <adres> (Pro)"},
		{"cancel", "Otomatik yenilemeyi durdur"},
		{"language", "English / Türkçe / Русский"},
		{"help", "Bot nasıl kullanılır"},
		{"terms", "Kullanım koşulları"},
		{"support", "Destek"},
		{"paysupport", "Ödeme desteği"},
	},
	langRU: {
		{"app", "Открыть приложение"},
		{"plans", "Тарифы и цены, подписка"},
		{"status", "Ваш тариф, продление и лимит на сегодня"},
		{"watches", "Адреса на мониторинге изменений риска"},
		{"history", "Ваши последние проверки"},
		{"details", "Полная разбивка: /details <адрес> (Pro)"},
		{"pdf", "PDF-отчёт на одну страницу: /pdf <адрес> (Pro)"},
		{"cancel", "Отключить автопродление"},
		{"language", "English / Türkçe / Русский"},
		{"help", "Как пользоваться ботом"},
		{"terms", "Условия использования"},
		{"support", "Связаться с поддержкой"},
		{"paysupport", "Помощь с оплатой"},
	},
}

func (b *bot) dispatch(ctx context.Context, u update) {
	switch {
	case u.PreCheckoutQuery != nil:
		b.preCheckout(ctx, u.PreCheckoutQuery)
	case u.CallbackQuery != nil:
		b.callback(ctx, u.CallbackQuery)
	case u.Message != nil:
		b.message(ctx, u.Message)
	}
}

func (b *bot) isAdmin(id int64) bool { return b.admins[id] }

func (b *bot) say(ctx context.Context, chatID int64, text string) {
	if err := b.tg.sendMessage(ctx, chatID, text, nil); err != nil {
		b.log.Warn("send failed", "chat", chatID, "error", err)
	}
}

func (b *bot) sayWith(ctx context.Context, chatID int64, text string, kb *keyboard) {
	if err := b.tg.sendMessage(ctx, chatID, text, kb); err != nil {
		b.log.Warn("send failed", "chat", chatID, "error", err)
	}
}

// chatCtx is one incoming message's context: who, where, in which language.
type chatCtx struct {
	chat int64
	user billing.User
	lang string
}

func (b *bot) message(ctx context.Context, m *message) {
	if m.From == nil || m.From.IsBot {
		return
	}
	// Payments and screening are personal: groups would share one person's
	// allowance with everyone in them.
	if m.Chat.Type != "private" {
		return
	}
	user, err := b.store.Touch(ctx, billing.User{ID: m.From.ID, Username: m.From.Username,
		FirstName: m.From.FirstName, ClientLang: m.From.LanguageCode})
	if err != nil {
		b.log.Error("touch user", "error", err)
		b.say(ctx, m.Chat.ID, t(normLang("", m.From.LanguageCode), "error_ours"))
		return
	}
	c := chatCtx{chat: m.Chat.ID, user: user, lang: normLang(user.Lang, m.From.LanguageCode)}

	switch {
	case m.SuccessfulPayment != nil:
		b.paid(ctx, c, m)
		return
	case m.RefundedPayment != nil:
		b.refunded(ctx, c, m)
		return
	}

	// A first contact of any kind starts the trial, so /start, a pasted
	// address and a batch file all work as a first message.
	trialStarted := false
	if billing.TrialDue(b.billing, user.TrialStartedAt) && !b.isAdmin(user.ID) {
		ok, err := b.store.StartTrial(ctx, user.ID, b.now())
		if err != nil {
			b.log.Error("start trial", "user", user.ID, "error", err)
		}
		trialStarted = ok
	}

	if m.Document != nil {
		if trialStarted {
			b.say(ctx, c.chat, b.trialNotice(c.lang))
		}
		b.batchFile(ctx, c, m.Document)
		return
	}

	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}
	cmd, arg := splitCommand(text)

	switch cmd {
	case "/start":
		b.welcome(ctx, c, m.From, trialStarted)
	case "/help":
		b.say(ctx, c.chat, t(c.lang, "help"))
	case "/app":
		b.openApp(ctx, c)
	case "/language", "/lang", "/dil":
		b.sayWith(ctx, c.chat, t(c.lang, "lang_pick"), &keyboard{InlineKeyboard: [][]button{{
			{Text: "English", CallbackData: "lang:en"}, {Text: "Türkçe", CallbackData: "lang:tr"}, {Text: "Русский", CallbackData: "lang:ru"},
		}}})
	case "/plans", "/subscribe", "/upgrade":
		b.plans(ctx, c)
	case "/status", "/account":
		b.status(ctx, c)
	case "/cancel":
		b.cancel(ctx, c)
	case "/terms":
		b.say(ctx, c.chat, b.terms[c.lang])
	case "/support":
		b.say(ctx, c.chat, t(c.lang, "support", b.support, user.ID))
	case "/paysupport":
		b.paySupport(ctx, c)
	case "/details":
		b.screenCommand(ctx, c, arg, kindDetails, "/details")
	case "/pdf":
		b.screenCommand(ctx, c, arg, kindPDF, "/pdf")
	case "/watch":
		b.watchCommand(ctx, c, arg)
	case "/unwatch":
		b.unwatchCommand(ctx, c, arg)
	case "/watches":
		b.watchesCommand(ctx, c)
	case "/history":
		b.historyCommand(ctx, c)
	case "/apikey", "/apikeys":
		b.apiKeyCommand(ctx, c, arg)
	case "/grant", "/revoke", "/stats", "/refund", "/user":
		if !b.isAdmin(user.ID) {
			b.say(ctx, c.chat, t(c.lang, "unknown_command"))
			return
		}
		b.admin(ctx, c.chat, cmd, arg)
	case "":
		if trialStarted {
			b.say(ctx, c.chat, b.trialNotice(c.lang))
		}
		b.screenCommand(ctx, c, text, kindSummary, "")
	default:
		b.say(ctx, c.chat, t(c.lang, "unknown_command"))
	}
}

// splitCommand returns "/cmd" (lower case, without any @botname) and the
// rest, or "" and the text for a plain message.
func splitCommand(text string) (string, string) {
	if !strings.HasPrefix(text, "/") {
		return "", text
	}
	cmd, arg, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@")
	return strings.ToLower(cmd), strings.TrimSpace(arg)
}

func (b *bot) welcome(ctx context.Context, c chatCtx, from *tgUser, trialStarted bool) {
	var sb strings.Builder
	name := from.FirstName
	if name == "" {
		name = t(c.lang, "there")
	}
	sb.WriteString(t(c.lang, "welcome", name))
	switch {
	case b.isAdmin(from.ID):
		sb.WriteString(t(c.lang, "welcome_admin"))
	case trialStarted:
		sb.WriteString(b.trialNotice(c.lang) + "\n")
	}
	sb.WriteString(t(c.lang, "welcome_footer"))
	b.sayWith(ctx, c.chat, sb.String(), b.appKeyboard(c.lang))
}

func (b *bot) trialNotice(lang string) string {
	p, _ := b.billing.Plan(b.billing.Trial.Plan)
	return t(lang, "trial_started", b.billing.Trial.Days, p.Name, p.DailyScreens)
}

// appKeyboard is a one-button keyboard opening the Mini App, or nil when no
// public URL is configured.
func (b *bot) appKeyboard(lang string) *keyboard {
	if b.appURL == "" {
		return nil
	}
	return &keyboard{InlineKeyboard: [][]button{{{Text: t(lang, "btn_open_app"), WebApp: &webApp{URL: b.appURL}}}}}
}

func (b *bot) openApp(ctx context.Context, c chatCtx) {
	if b.appURL == "" {
		b.say(ctx, c.chat, t(c.lang, "app_off"))
		return
	}
	b.sayWith(ctx, c.chat, t(c.lang, "app_open"), b.appKeyboard(c.lang))
}

// --- screening -------------------------------------------------------------

func (b *bot) screenCommand(ctx context.Context, c chatCtx, arg string, kind screenKind, cmd string) {
	if strings.TrimSpace(arg) == "" {
		b.say(ctx, c.chat, t(c.lang, "usage_cmd", cmd))
		return
	}
	// Say something as soon as the screen starts: it can take a minute.
	out, err := b.gate(ctx, screenRequest{UserID: c.user.ID, Text: arg, Kind: kind, Channel: chanBot,
		Started: func(_, address string) { b.say(ctx, c.chat, t(c.lang, "screening", address)) }})
	if err != nil {
		b.refusal(ctx, c, err)
		return
	}
	switch kind {
	case kindPDF:
		err = b.tg.sendDocument(ctx, c.chat, out.Chain+"-"+out.Address+".pdf", out.PDF, t(c.lang, "pdf_caption", out.Address))
	case kindDetails:
		b.startFollowUp(c.user.ID, c.lang, out.Chain, out.Address, out.Result)
		err = b.tg.sendMessage(ctx, c.chat, format(out.Result), nil)
	default:
		following := b.startFollowUp(c.user.ID, c.lang, out.Chain, out.Address, out.Result)
		err = b.tg.sendMessage(ctx, c.chat, summary(out.Result, c.lang, following), nil)
	}
	if err != nil {
		b.log.Warn("deliver screen", "user", c.user.ID, "error", err)
	}
	if out.Limit > 0 && out.Limit-out.Used <= 3 {
		b.say(ctx, c.chat, t(c.lang, "screens_left", out.Limit-out.Used, out.Limit))
	}
}

// refusal tells a chat user why the gate said no.
func (b *bot) refusal(ctx context.Context, c chatCtx, err error) {
	var ge *gateError
	if !errors.As(err, &ge) {
		b.log.Error("gate", "user", c.user.ID, "error", err)
		b.say(ctx, c.chat, t(c.lang, "error_ours"))
		return
	}
	switch ge.Code {
	case "bad_address":
		b.say(ctx, c.chat, t(c.lang, "bad_address"))
	case "chain_unavailable":
		name := ge.Chain
		if ci, ok := b.chain(ge.Chain); ok {
			name = ci.Name
		}
		b.say(ctx, c.chat, t(c.lang, "chain_unavailable", name))
	case "no_plan":
		b.sayWith(ctx, c.chat, t(c.lang, "no_plan"), b.plansKeyboard(ctx, c))
	case "feature_locked":
		acc, _ := b.access(ctx, c.user.ID)
		b.sayWith(ctx, c.chat, t(c.lang, "feature_locked", ge.Feature, acc.planName()), b.plansKeyboard(ctx, c))
	case "busy":
		b.say(ctx, c.chat, t(c.lang, "busy"))
	case "limit_reached":
		b.sayWith(ctx, c.chat, t(c.lang, "limit_reached", ge.Limit), b.plansKeyboard(ctx, c))
	case "screen_failed":
		b.log.Warn("screen failed", "user", c.user.ID, "error", ge.Cause)
		b.say(ctx, c.chat, t(c.lang, "screen_failed", ge.Cause))
	default:
		b.say(ctx, c.chat, t(c.lang, "error_ours"))
	}
}

// claim marks a user as having a screen in flight; false if one already is.
func (b *bot) claim(userID int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.busy[userID] {
		return false
	}
	b.busy[userID] = true
	return true
}

func (b *bot) release(userID int64) {
	b.mu.Lock()
	delete(b.busy, userID)
	b.mu.Unlock()
}

// --- watches and history ---------------------------------------------------

func (b *bot) watchCommand(ctx context.Context, c chatCtx, arg string) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		b.say(ctx, c.chat, t(c.lang, "watch_usage"))
		return
	}
	// "bsc 0x… name" or "0x… name": the chain word, if any, is part of the target.
	target, label := fields[0], strings.Join(fields[1:], " ")
	if _, ok := chainAliases[strings.ToLower(fields[0])]; ok && len(fields) > 1 {
		target, label = fields[0]+" "+fields[1], strings.Join(fields[2:], " ")
	}
	w, n, limit, err := b.addWatch(ctx, c.user.ID, target, "", label)
	if err != nil {
		var ge *gateError
		if errors.As(err, &ge) && ge.Code == "limit_reached" {
			b.sayWith(ctx, c.chat, t(c.lang, "watch_limit", ge.Limit), b.plansKeyboard(ctx, c))
			return
		}
		b.refusal(ctx, c, err)
		return
	}
	b.say(ctx, c.chat, t(c.lang, "watch_added", w.Address, humanInterval(c.lang, b.billing.Monitor.Interval), n, limitText(c.lang, limit)))
}

// addWatch is the watch-adding path shared by the chat, app and API. It
// returns the watch, how many the user now has, and their limit.
func (b *bot) addWatch(ctx context.Context, userID int64, text, chain, label string) (billing.Watch, int, int, error) {
	chain, address, err := b.parseTarget(text, chain)
	if err != nil {
		var te *targetError
		if errors.As(err, &te) {
			return billing.Watch{}, 0, 0, &gateError{Code: te.code, Status: 400, Chain: te.chain}
		}
		return billing.Watch{}, 0, 0, err
	}
	acc, err := b.access(ctx, userID)
	if err != nil {
		return billing.Watch{}, 0, 0, err
	}
	if !acc.Admin && acc.Plan == nil {
		return billing.Watch{}, 0, 0, refuse("no_plan", 402)
	}
	if acc.Watches == 0 {
		return billing.Watch{}, 0, 0, &gateError{Code: "feature_locked", Status: 403, Feature: "/watch"}
	}
	if len(label) > 64 {
		label = label[:64]
	}
	w, err := b.store.AddWatch(ctx, userID, chain, address, label, acc.Watches, b.now())
	if errors.Is(err, billing.ErrWatchLimit) {
		return billing.Watch{}, 0, 0, &gateError{Code: "limit_reached", Status: 429, Limit: acc.Watches}
	}
	if err != nil {
		return billing.Watch{}, 0, 0, err
	}
	all, _ := b.store.Watches(ctx, userID)
	b.wakeMonitor()
	return w, len(all), acc.Watches, nil
}

func (b *bot) unwatchCommand(ctx context.Context, c chatCtx, arg string) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		b.say(ctx, c.chat, t(c.lang, "watch_usage"))
		return
	}
	addr := fields[len(fields)-1]
	if evmAddress.MatchString(addr) {
		addr = strings.ToLower(addr)
	}
	if err := b.store.RemoveWatchByAddress(ctx, c.user.ID, addr, b.now()); err != nil {
		if errors.Is(err, billing.ErrNotFound) {
			b.say(ctx, c.chat, t(c.lang, "watch_notfound"))
			return
		}
		b.log.Error("remove watch", "error", err)
		b.say(ctx, c.chat, t(c.lang, "error_ours"))
		return
	}
	b.say(ctx, c.chat, t(c.lang, "watch_removed", addr))
}

func (b *bot) watchesCommand(ctx context.Context, c chatCtx) {
	ws, err := b.store.Watches(ctx, c.user.ID)
	if err != nil {
		b.log.Error("watches", "error", err)
		b.say(ctx, c.chat, t(c.lang, "error_ours"))
		return
	}
	if len(ws) == 0 {
		b.say(ctx, c.chat, t(c.lang, "watches_none"))
		return
	}
	acc, _ := b.access(ctx, c.user.ID)
	var sb strings.Builder
	sb.WriteString(t(c.lang, "watches_title", len(ws), limitText(c.lang, acc.Watches)))
	for _, w := range ws {
		name := w.Address
		if w.Label != "" {
			name = w.Label + " · " + w.Address
		}
		state := t(c.lang, "watch_pending")
		if w.Last != nil {
			state = fmt.Sprintf("%s %.1f", bandWord(c.lang, w.Last.Band), w.Last.Score)
			if len(w.Last.RiskCategories) > 0 {
				state += " · " + strings.Join(w.Last.RiskCategories, ", ")
			}
		}
		fmt.Fprintf(&sb, "\n%s\n  %s\n", name, state)
	}
	b.say(ctx, c.chat, sb.String())
}

func (b *bot) historyCommand(ctx context.Context, c chatCtx) {
	items, err := b.store.History(ctx, c.user.ID, 15)
	if err != nil {
		b.log.Error("history", "error", err)
		b.say(ctx, c.chat, t(c.lang, "error_ours"))
		return
	}
	if len(items) == 0 {
		b.say(ctx, c.chat, t(c.lang, "history_none"))
		return
	}
	var sb strings.Builder
	sb.WriteString(t(c.lang, "history_title"))
	for _, h := range items {
		result := "PDF"
		if h.Band != nil && h.Score != nil {
			result = fmt.Sprintf("%s %.1f", bandWord(c.lang, *h.Band), *h.Score)
		}
		fmt.Fprintf(&sb, "\n%s  %s\n  %s\n", h.CreatedAt.UTC().Format("01-02 15:04"), result, h.Address)
	}
	b.say(ctx, c.chat, sb.String())
}

func bandWord(lang, band string) string {
	words := map[string]map[string]string{
		langTR: {"low": "Düşük", "medium": "Orta", "high": "Yüksek"},
		langRU: {"low": "Низкий", "medium": "Средний", "high": "Высокий"},
	}[lang]
	if w, ok := words[band]; ok {
		return w
	}
	if band == "" {
		return band
	}
	return strings.ToUpper(band[:1]) + band[1:]
}

func limitText(lang string, n int) string {
	if n < 0 {
		switch lang {
		case langTR:
			return "sınırsız"
		case langRU:
			return "без ограничений"
		}
		return "unlimited"
	}
	return strconv.Itoa(n)
}

func humanInterval(lang string, d time.Duration) string {
	h := int(d.Hours())
	switch lang {
	case langTR:
		return fmt.Sprintf("%d saatte", h)
	case langRU:
		return fmt.Sprintf("%d ч", h)
	}
	return fmt.Sprintf("%d hours", h)
}

// --- API keys --------------------------------------------------------------

func (b *bot) apiKeyCommand(ctx context.Context, c chatCtx, arg string) {
	acc, err := b.access(ctx, c.user.ID)
	if err != nil {
		b.say(ctx, c.chat, t(c.lang, "error_ours"))
		return
	}
	if !acc.API {
		b.sayWith(ctx, c.chat, t(c.lang, "apikey_locked"), b.plansKeyboard(ctx, c))
		return
	}
	fields := strings.Fields(arg)
	switch {
	case len(fields) == 0:
		keys, err := b.store.Keys(ctx, c.user.ID)
		if err != nil {
			b.say(ctx, c.chat, t(c.lang, "error_ours"))
			return
		}
		if len(keys) == 0 {
			b.say(ctx, c.chat, t(c.lang, "apikey_none"))
			return
		}
		var sb strings.Builder
		sb.WriteString(t(c.lang, "apikey_list"))
		for _, k := range keys {
			used := "-"
			if k.LastUsedAt != nil {
				used = datetime(*k.LastUsedAt)
			}
			fmt.Fprintf(&sb, "\n#%d  %s…  %s\n  %s · %s\n", k.ID, k.Prefix, k.Name, date(k.CreatedAt), used)
		}
		sb.WriteString("\n" + t(c.lang, "apikey_usage"))
		b.say(ctx, c.chat, sb.String())
	case fields[0] == "new":
		key, _, err := b.store.CreateKey(ctx, c.user.ID, strings.Join(fields[1:], " "), b.now())
		if errors.Is(err, billing.ErrKeyLimit) {
			b.say(ctx, c.chat, t(c.lang, "apikey_too_many"))
			return
		}
		if err != nil {
			b.log.Error("create key", "error", err)
			b.say(ctx, c.chat, t(c.lang, "error_ours"))
			return
		}
		b.say(ctx, c.chat, t(c.lang, "apikey_new", key, b.publicBase()))
	case fields[0] == "revoke" && len(fields) > 1:
		id, _ := strconv.ParseInt(strings.TrimPrefix(fields[1], "#"), 10, 64)
		if err := b.store.RevokeKey(ctx, c.user.ID, id, b.now()); err != nil {
			b.say(ctx, c.chat, t(c.lang, "apikey_usage"))
			return
		}
		b.say(ctx, c.chat, t(c.lang, "apikey_revoked", id))
	default:
		b.say(ctx, c.chat, t(c.lang, "apikey_usage"))
	}
}

// publicBase is the public origin customers reach the API at.
func (b *bot) publicBase() string {
	base := strings.TrimSuffix(b.appURL, "/")
	return strings.TrimSuffix(base, "/app")
}

// --- plans and status ------------------------------------------------------

func (b *bot) plansText(lang string, acc billing.Access) string {
	var sb strings.Builder
	sb.WriteString(t(lang, "plans_title"))
	for _, p := range b.billing.Plans {
		sb.WriteString(t(lang, "plan_line", p.Name, p.DailyScreens))
		if p.Details {
			sb.WriteString(t(lang, "feat_details"))
		}
		if p.PDF {
			sb.WriteString(t(lang, "feat_pdf"))
		}
		if p.Watches > 0 {
			sb.WriteString(t(lang, "feat_watches", p.Watches))
		}
		if p.Batch > 0 {
			sb.WriteString(t(lang, "feat_batch", p.Batch))
		}
		if p.API {
			sb.WriteString(t(lang, "feat_api"))
		}
		sb.WriteString(t(lang, "price_stars", p.PriceStars))
		if b.usdtAddr != "" {
			sb.WriteString(t(lang, "price_usdt", billing.FormatUSDT(p.PriceMicroUSDT), b.billing.PeriodDays))
		}
		sb.WriteString("\n")
	}
	if acc.Plan != nil {
		sb.WriteString(t(lang, "your_plan", acc.Plan.Name, date(acc.Until)))
		if acc.Renews {
			sb.WriteString(t(lang, "renews"))
		}
	}
	sb.WriteString(t(lang, "no_days_lost"))
	return sb.String()
}

func (b *bot) plans(ctx context.Context, c chatCtx) {
	acc, err := b.store.Access(ctx, c.user.ID, b.now())
	if err != nil {
		b.log.Error("access", "error", err)
	}
	b.sayWith(ctx, c.chat, b.plansText(c.lang, acc), b.plansKeyboard(ctx, c))
}

// renewingPlans are the plans a user renews through Stars.
func (b *bot) renewingPlans(ctx context.Context, userID int64) map[string]bool {
	subs, err := b.store.Subscriptions(ctx, userID)
	if err != nil {
		b.log.Error("subscriptions", "error", err)
	}
	now := b.now()
	renewing := map[string]bool{}
	for _, s := range subs {
		if s.Source == "stars" && s.IsRecurring && s.RenewalCanceledAt == nil && s.Active(now) {
			renewing[s.Plan] = true
		}
	}
	return renewing
}

// plansKeyboard has a Stars button and, when enabled, a USDT button per plan.
// A plan the user already renews through Stars shows as active rather than
// offering a second concurrent subscription.
func (b *bot) plansKeyboard(ctx context.Context, c chatCtx) *keyboard {
	renewing := b.renewingPlans(ctx, c.user.ID)
	kb := &keyboard{}
	for _, p := range b.billing.Plans {
		var row []button
		if renewing[p.ID] {
			row = append(row, button{Text: t(c.lang, "btn_active", p.Name), CallbackData: "status"})
		} else if link, err := b.starsLink(ctx, &p, c.lang); err == nil {
			row = append(row, button{Text: t(c.lang, "btn_stars", p.Name, p.PriceStars), URL: link})
		} else {
			b.log.Error("stars invoice link", "plan", p.ID, "error", err)
		}
		if b.usdtAddr != "" {
			row = append(row, button{Text: t(c.lang, "btn_usdt", p.Name, billing.FormatUSDT(p.PriceMicroUSDT)),
				CallbackData: "usdt:" + p.ID})
		}
		if len(row) > 0 {
			kb.InlineKeyboard = append(kb.InlineKeyboard, row)
		}
	}
	return kb
}

// starsPayload identifies the plan in a Stars invoice. The price is not in
// it: pre-checkout compares the amount against the current configuration.
func starsPayload(planID string) string { return "plan:" + planID }

// starsLink returns the plan's Stars subscription link in the user's
// language, creating it once per plan and language.
func (b *bot) starsLink(ctx context.Context, p *billing.Plan, lang string) (string, error) {
	key := p.ID + "/" + lang
	b.mu.Lock()
	link, ok := b.links[key]
	b.mu.Unlock()
	if ok {
		return link, nil
	}
	desc := t(lang, "inv_screens", p.DailyScreens)
	if p.Details {
		desc += t(lang, "inv_details")
	}
	if p.PDF {
		desc += t(lang, "inv_pdf")
	}
	desc += t(lang, "inv_renew")
	link, err := b.tg.createSubscriptionLink(ctx, t(lang, "inv_title", p.Name), desc, starsPayload(p.ID), int64(p.PriceStars))
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	b.links[key] = link
	b.mu.Unlock()
	return link, nil
}

func (b *bot) status(ctx context.Context, c chatCtx) {
	now := b.now()
	if b.isAdmin(c.user.ID) {
		b.say(ctx, c.chat, t(c.lang, "status_admin", c.user.ID))
		return
	}
	acc, err := b.store.Access(ctx, c.user.ID, now)
	if err != nil {
		b.log.Error("access", "error", err)
		b.say(ctx, c.chat, t(c.lang, "error_ours"))
		return
	}
	used, _ := b.store.Used(ctx, c.user.ID, now)
	var sb strings.Builder
	if acc.Plan == nil {
		sb.WriteString(t(c.lang, "status_none"))
	} else {
		sb.WriteString(t(c.lang, "status_plan", acc.Plan.Name, t(c.lang, "src_"+acc.Source), datetime(acc.Until)))
		if acc.Renews {
			sb.WriteString(t(c.lang, "status_renews"))
		} else if acc.Source != "trial" {
			sb.WriteString(t(c.lang, "status_manual"))
		}
		sb.WriteString(t(c.lang, "status_today", used, acc.Plan.DailyScreens))
		if acc.Plan.Watches > 0 {
			ws, _ := b.store.Watches(ctx, c.user.ID)
			sb.WriteString(t(c.lang, "status_watch", len(ws), acc.Plan.Watches))
		}
	}
	sb.WriteString(t(c.lang, "status_id", c.user.ID))
	b.say(ctx, c.chat, sb.String())
}

func (b *bot) cancel(ctx context.Context, c chatCtx) {
	now := b.now()
	charges, err := b.store.RenewingCharges(ctx, c.user.ID, now)
	if err != nil {
		b.log.Error("renewing charges", "error", err)
		b.say(ctx, c.chat, t(c.lang, "error_ours"))
		return
	}
	if len(charges) == 0 {
		b.say(ctx, c.chat, t(c.lang, "cancel_none"))
		return
	}
	for _, ch := range charges {
		if err := b.tg.editUserStarSubscription(ctx, c.user.ID, ch, true); err != nil {
			b.log.Error("cancel stars subscription", "user", c.user.ID, "charge", ch, "error", err)
			b.say(ctx, c.chat, t(c.lang, "cancel_failed", b.support))
			return
		}
		if err := b.store.MarkRenewalCanceled(ctx, ch, now); err != nil {
			b.log.Error("mark renewal canceled", "charge", ch, "error", err)
		}
	}
	acc, _ := b.store.Access(ctx, c.user.ID, now)
	msg := t(c.lang, "cancel_done")
	if acc.Plan != nil {
		msg += t(c.lang, "cancel_until", acc.Plan.Name, date(acc.Until))
	}
	b.say(ctx, c.chat, msg)
}

func (b *bot) paySupport(ctx context.Context, c chatCtx) {
	subs, _ := b.store.Subscriptions(ctx, c.user.ID)
	var sb strings.Builder
	sb.WriteString(t(c.lang, "paysupport", b.support, c.user.ID))
	var paid []billing.Subscription
	for _, s := range subs {
		if s.Source == "stars" || s.Source == "usdt" {
			paid = append(paid, s)
		}
	}
	if len(paid) > 0 {
		sb.WriteString(t(c.lang, "paysupport_list"))
		for i := len(paid) - 1; i >= 0 && i >= len(paid)-5; i-- {
			s := paid[i]
			name := s.Plan
			if p, ok := b.billing.Plan(s.Plan); ok {
				name = p.Name
			}
			fmt.Fprintf(&sb, "\n%s  %s via %s", date(s.StartsAt), name, s.Source)
			if s.StarsChargeID != "" {
				fmt.Fprintf(&sb, "\n  charge %s", s.StarsChargeID)
			}
			if s.RevokedAt != nil {
				sb.WriteString(t(c.lang, "refunded_tag"))
			}
		}
	} else {
		sb.WriteString(".")
	}
	sb.WriteString(t(c.lang, "paysupport_note"))
	b.say(ctx, c.chat, sb.String())
}

// --- Stars payments --------------------------------------------------------

// preCheckout approves a Stars payment only if it matches a current plan and
// price. Telegram allows 10 seconds for the answer, so nothing slow happens
// here.
func (b *bot) preCheckout(ctx context.Context, q *preCheckoutQuery) {
	lang := normLang("", q.From.LanguageCode)
	ok, reason := b.checkStarsOrder(q.Currency, q.TotalAmount, q.InvoicePayload)
	msg := ""
	if !ok {
		b.log.Warn("pre-checkout rejected", "user", q.From.ID, "payload", q.InvoicePayload, "reason", reason)
		msg = t(lang, reason)
	}
	if err := b.tg.answerPreCheckoutQuery(ctx, q.ID, ok, msg); err != nil {
		b.log.Error("answer pre-checkout", "user", q.From.ID, "error", err)
	}
}

// checkStarsOrder returns whether an order is valid, and if not the message
// key explaining why.
func (b *bot) checkStarsOrder(currency string, amount int64, payload string) (bool, string) {
	id, found := strings.CutPrefix(payload, "plan:")
	p, ok := b.billing.Plan(id)
	if !found || !ok {
		return false, "offer_gone"
	}
	if currency != "XTR" || amount != int64(p.PriceStars) {
		return false, "price_changed"
	}
	return true, ""
}

func (b *bot) paid(ctx context.Context, c chatCtx, m *message) {
	sp := m.SuccessfulPayment
	if sp.Currency != "XTR" {
		b.log.Error("payment in unexpected currency", "user", c.user.ID, "currency", sp.Currency)
		return
	}
	id, _ := strings.CutPrefix(sp.InvoicePayload, "plan:")
	now := b.now()
	var expires time.Time
	if sp.SubscriptionExpirationDate > 0 {
		expires = time.Unix(sp.SubscriptionExpirationDate, 0).UTC()
	}
	recorded, err := b.store.RecordStars(ctx, billing.StarsPayment{
		UserID: c.user.ID, Plan: id, Amount: sp.TotalAmount, ChargeID: sp.TelegramPaymentChargeID,
		Recurring: sp.IsRecurring, ExpiresAt: expires, ReceivedAt: now,
	})
	if err != nil {
		// The money has been taken. Never leave a paying customer without
		// access silently: tell them and every admin.
		b.log.Error("record stars payment", "user", c.user.ID, "charge", sp.TelegramPaymentChargeID, "error", err)
		b.say(ctx, c.chat, t(c.lang, "paid_failed", b.support))
		b.notifyAdmins(ctx, fmt.Sprintf("⚠️ Stars payment NOT activated\nuser %d, charge %s, %d Stars, payload %q\nerror: %v",
			c.user.ID, sp.TelegramPaymentChargeID, sp.TotalAmount, sp.InvoicePayload, err))
		return
	}
	if !recorded {
		return // a repeated delivery of a payment already handled
	}
	acc, _ := b.store.Access(ctx, c.user.ID, now)
	name, until := id, expires
	if acc.Plan != nil {
		name, until = acc.Plan.Name, acc.Until
	}
	msg := t(c.lang, "paid_thanks", name, date(until))
	if sp.IsRecurring && !sp.IsFirstRecurring {
		msg = t(c.lang, "paid_renewed", name, date(until))
	}
	b.say(ctx, c.chat, msg)
	b.notifyAdmins(ctx, fmt.Sprintf("⭐ %d Stars from %s for %s", sp.TotalAmount, who(m.From), name))
}

func (b *bot) refunded(ctx context.Context, c chatCtx, m *message) {
	rp := m.RefundedPayment
	uid, err := b.store.RevokeCharge(ctx, rp.TelegramPaymentChargeID, "refunded", b.now())
	if err != nil {
		b.log.Error("revoke refunded charge", "charge", rp.TelegramPaymentChargeID, "error", err)
		return
	}
	if uid != 0 {
		b.say(ctx, c.chat, t(c.lang, "refunded"))
	}
}

// --- callbacks: USDT invoices, language, status ------------------------------

func (b *bot) callback(ctx context.Context, q *callbackQuery) {
	chat := q.From.ID
	if q.Message != nil {
		chat = q.Message.Chat.ID
	}
	user, err := b.store.Touch(ctx, billing.User{ID: q.From.ID, Username: q.From.Username,
		FirstName: q.From.FirstName, ClientLang: q.From.LanguageCode})
	if err != nil {
		b.log.Error("touch user", "error", err)
	}
	c := chatCtx{chat: chat, user: user, lang: normLang(user.Lang, q.From.LanguageCode)}
	c.user.ID = q.From.ID

	switch {
	case q.Data == "status":
		_ = b.tg.answerCallbackQuery(ctx, q.ID, "")
		b.status(ctx, c)
	case strings.HasPrefix(q.Data, "lang:"):
		lang := normLang(strings.TrimPrefix(q.Data, "lang:"), "")
		if err := b.store.SetLang(ctx, q.From.ID, lang); err != nil {
			b.log.Error("set lang", "error", err)
		}
		_ = b.tg.answerCallbackQuery(ctx, q.ID, "")
		b.say(ctx, chat, t(lang, "lang_set"))
	case strings.HasPrefix(q.Data, "usdt:"):
		b.usdtInvoiceChat(ctx, q, c, strings.TrimPrefix(q.Data, "usdt:"))
	default:
		_ = b.tg.answerCallbackQuery(ctx, q.ID, "")
	}
}

// usdtInvoice issues a USDT invoice for a plan. Shared by the chat and the app.
func (b *bot) usdtInvoice(ctx context.Context, userID int64, planID string) (billing.Invoice, *billing.Plan, error) {
	p, ok := b.billing.Plan(planID)
	if !ok || b.usdtAddr == "" {
		return billing.Invoice{}, nil, refuse("not_found", 404)
	}
	inv, err := b.store.CreateInvoice(ctx, userID, p.ID, b.usdtAddr, b.now(), rand.IntN(1000))
	return inv, p, err
}

func (b *bot) usdtInvoiceChat(ctx context.Context, q *callbackQuery, c chatCtx, planID string) {
	inv, p, err := b.usdtInvoice(ctx, q.From.ID, planID)
	if err != nil {
		var ge *gateError
		if errors.As(err, &ge) {
			_ = b.tg.answerCallbackQuery(ctx, q.ID, t(c.lang, "option_gone"))
			return
		}
		b.log.Error("create usdt invoice", "user", q.From.ID, "error", err)
		_ = b.tg.answerCallbackQuery(ctx, q.ID, t(c.lang, "invoice_failed"))
		return
	}
	_ = b.tg.answerCallbackQuery(ctx, q.ID, "")
	amount := billing.FormatUSDT(inv.Amount)
	mins := int(inv.ExpiresAt.Sub(b.now()).Round(time.Minute).Minutes())
	b.say(ctx, c.chat, t(c.lang, "usdt_invoice", p.Name, b.billing.PeriodDays, amount, amount, amount, mins))
	// On its own so it can be copied with one tap.
	b.say(ctx, c.chat, b.usdtAddr)
}

// --- admin -----------------------------------------------------------------

func (b *bot) admin(ctx context.Context, chat int64, cmd, arg string) {
	args := strings.Fields(arg)
	now := b.now()
	switch cmd {
	case "/stats":
		st, err := b.store.Stats(ctx, now)
		if err != nil {
			b.say(ctx, chat, "stats: "+err.Error())
			return
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Users: %d (active in 7 days: %d)\n\nActive subscribers by plan/source:\n", st.Users, st.ActiveUsers7d)
		if len(st.ActiveByPlanSource) == 0 {
			sb.WriteString("  none\n")
		}
		for k, n := range st.ActiveByPlanSource {
			fmt.Fprintf(&sb, "  %s: %d\n", k, n)
		}
		fmt.Fprintf(&sb, "\nLast 30 days: %d Stars, %s USDT\nUnmatched USDT payments: %d",
			st.Stars30d, billing.FormatUSDT(st.USDT30d), st.Unmatched)
		b.say(ctx, chat, sb.String())

	case "/grant":
		if len(args) < 3 {
			b.say(ctx, chat, "Usage: /grant <user id or @username> <plan> <days> [note]")
			return
		}
		uid, err := b.store.FindUser(ctx, args[0])
		if err != nil {
			b.say(ctx, chat, err.Error())
			return
		}
		days, err := strconv.Atoi(args[2])
		if err != nil {
			b.say(ctx, chat, "days must be a number")
			return
		}
		if err := b.store.EnsureUser(ctx, uid); err != nil {
			b.say(ctx, chat, err.Error())
			return
		}
		note := "granted by admin"
		if len(args) > 3 {
			note = strings.Join(args[3:], " ")
		}
		until, err := b.store.Grant(ctx, uid, args[1], days, note, now)
		if err != nil {
			b.say(ctx, chat, "grant: "+err.Error())
			return
		}
		b.say(ctx, chat, fmt.Sprintf("Granted %s to %d until %s.", args[1], uid, datetime(until)))
		p, _ := b.billing.Plan(args[1])
		b.say(ctx, uid, t(b.langOf(ctx, uid), "granted", p.Name, date(until)))

	case "/revoke":
		if len(args) < 1 {
			b.say(ctx, chat, "Usage: /revoke <user id or @username>")
			return
		}
		uid, err := b.store.FindUser(ctx, args[0])
		if err != nil {
			b.say(ctx, chat, err.Error())
			return
		}
		// Stop Stars renewals first, so a revoked user is not charged again.
		charges, _ := b.store.RenewingCharges(ctx, uid, now)
		for _, ch := range charges {
			if err := b.tg.editUserStarSubscription(ctx, uid, ch, true); err != nil {
				b.say(ctx, chat, fmt.Sprintf("Could not stop renewal of charge %s: %v", ch, err))
			} else {
				_ = b.store.MarkRenewalCanceled(ctx, ch, now)
			}
		}
		n, err := b.store.Revoke(ctx, uid, "revoked by admin", now)
		if err != nil {
			b.say(ctx, chat, "revoke: "+err.Error())
			return
		}
		b.say(ctx, chat, fmt.Sprintf("Revoked %d active period(s) of %d; stopped %d renewal(s). Payments are not refunded; use /refund for that.", n, uid, len(charges)))

	case "/refund":
		if len(args) < 1 {
			b.say(ctx, chat, "Usage: /refund <charge id>  (Stars only; the customer's /paysupport lists their charge ids)")
			return
		}
		uid, err := b.store.ChargeOwner(ctx, args[0])
		if err != nil {
			b.say(ctx, chat, err.Error())
			return
		}
		if err := b.tg.refundStarPayment(ctx, uid, args[0]); err != nil {
			b.say(ctx, chat, "Telegram refused the refund: "+err.Error())
			return
		}
		// Telegram also sends a refunded_payment message; revoking here too
		// makes the outcome independent of its delivery.
		if _, err := b.store.RevokeCharge(ctx, args[0], "refunded by admin", now); err != nil {
			b.say(ctx, chat, "Refunded, but recording it failed: "+err.Error())
			return
		}
		b.say(ctx, chat, fmt.Sprintf("Refunded charge %s to %d; its period has ended.", args[0], uid))

	case "/user":
		if len(args) < 1 {
			b.say(ctx, chat, "Usage: /user <user id or @username>")
			return
		}
		uid, err := b.store.FindUser(ctx, args[0])
		if err != nil {
			b.say(ctx, chat, err.Error())
			return
		}
		acc, _ := b.store.Access(ctx, uid, now)
		used, _ := b.store.Used(ctx, uid, now)
		subs, _ := b.store.Subscriptions(ctx, uid)
		ws, _ := b.store.Watches(ctx, uid)
		keys, _ := b.store.Keys(ctx, uid)
		var sb strings.Builder
		fmt.Fprintf(&sb, "User %d\n", uid)
		if acc.Plan != nil {
			fmt.Fprintf(&sb, "Plan: %s via %s until %s (renews: %v)\nToday: %d/%d · watches %d · API keys %d\n",
				acc.Plan.Name, acc.Source, datetime(acc.Until), acc.Renews, used, acc.Plan.DailyScreens, len(ws), len(keys))
		} else {
			sb.WriteString("No active plan\n")
		}
		sb.WriteString("\nHistory:\n")
		for _, s := range subs {
			fmt.Fprintf(&sb, "  %s → %s  %s/%s", date(s.StartsAt), date(s.ExpiresAt), s.Plan, s.Source)
			if s.RevokedAt != nil {
				sb.WriteString(" revoked")
			}
			if s.StarsChargeID != "" {
				sb.WriteString(" " + s.StarsChargeID)
			}
			sb.WriteString("\n")
		}
		b.say(ctx, chat, sb.String())
	}
}

func (b *bot) notifyAdmins(ctx context.Context, text string) {
	for id := range b.admins {
		b.say(ctx, id, text)
	}
}

func who(u *tgUser) string {
	if u.Username != "" {
		return "@" + u.Username
	}
	return strconv.FormatInt(u.ID, 10)
}

func date(t time.Time) string     { return t.UTC().Format("2006-01-02") }
func datetime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }
