package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/mozer/tether-risk/internal/billing"
	"github.com/mozer/tether-risk/internal/chain/tron"
)

// publicCommands is the menu Telegram shows next to the input field.
var publicCommands = []botCommand{
	{"plans", "Plans and prices, subscribe"},
	{"status", "Your plan, renewal and today's usage"},
	{"details", "Full breakdown: /details <address> (Pro)"},
	{"pdf", "One-page PDF report: /pdf <address> (Pro)"},
	{"cancel", "Stop automatic renewal"},
	{"help", "How to use this bot"},
	{"terms", "Terms of service"},
	{"support", "Contact support"},
	{"paysupport", "Help with a payment"},
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

func (b *bot) message(ctx context.Context, m *message) {
	if m.From == nil || m.From.IsBot {
		return
	}
	// Payments and screening are personal: groups would share one person's
	// allowance with everyone in them.
	if m.Chat.Type != "private" {
		return
	}
	user, err := b.store.Touch(ctx, billing.User{ID: m.From.ID, Username: m.From.Username, FirstName: m.From.FirstName})
	if err != nil {
		b.log.Error("touch user", "error", err)
		b.say(ctx, m.Chat.ID, "Something went wrong on our side. Please try again in a minute.")
		return
	}

	switch {
	case m.SuccessfulPayment != nil:
		b.paid(ctx, m)
		return
	case m.RefundedPayment != nil:
		b.refunded(ctx, m)
		return
	}

	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}
	cmd, arg := splitCommand(text)
	chat := m.Chat.ID

	// A first contact of any kind starts the trial, so /start and a pasted
	// address both work as a first message.
	trialStarted := false
	if billing.TrialDue(b.billing, user.TrialStartedAt) && !b.isAdmin(user.ID) {
		ok, err := b.store.StartTrial(ctx, user.ID, b.now())
		if err != nil {
			b.log.Error("start trial", "user", user.ID, "error", err)
		}
		trialStarted = ok
	}

	switch cmd {
	case "/start":
		b.welcome(ctx, chat, m.From, trialStarted)
	case "/help":
		b.say(ctx, chat, helpText)
	case "/plans", "/subscribe", "/upgrade":
		b.plans(ctx, chat, user.ID)
	case "/status", "/account":
		b.status(ctx, chat, user.ID)
	case "/cancel":
		b.cancel(ctx, chat, user.ID)
	case "/terms":
		b.say(ctx, chat, b.terms)
	case "/support":
		b.say(ctx, chat, "Support: "+b.support+"\n\nPlease include your user id ("+
			strconv.FormatInt(user.ID, 10)+") and, for a payment, the date and amount.")
	case "/paysupport":
		b.paySupport(ctx, chat, user.ID)
	case "/details":
		b.screenCommand(ctx, chat, user.ID, arg, "details")
	case "/pdf":
		b.screenCommand(ctx, chat, user.ID, arg, "pdf")
	case "/grant", "/revoke", "/stats", "/refund", "/user":
		if !b.isAdmin(user.ID) {
			b.say(ctx, chat, "Unknown command. Send an address, or /help.")
			return
		}
		b.admin(ctx, chat, cmd, arg)
	case "":
		if trialStarted {
			b.say(ctx, chat, b.trialNotice())
		}
		b.screenCommand(ctx, chat, user.ID, strings.Fields(text)[0], "summary")
	default:
		b.say(ctx, chat, "Unknown command. Send an address, or /help.")
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

const helpText = `Address risk screening for TRON.

Send an address and you get a summary of its connections: where its funds came from and went, and the risk categories they touch.

/details <address>  full per-direction breakdown with paths (Pro)
/pdf <address>      one-page PDF report (Pro)
/plans              plans and prices
/status             your plan and today's usage
/cancel             stop automatic renewal
/terms  /support  /paysupport

This is automated triage and pre-screening built on open data. It is not a regulated AML determination and must not be used as one.

Always read the coverage figure alongside the score. Low coverage means most traced value could not be attributed to a known entity: unknown, not clean.`

func (b *bot) welcome(ctx context.Context, chat int64, from *tgUser, trialStarted bool) {
	var sb strings.Builder
	name := from.FirstName
	if name == "" {
		name = "there"
	}
	fmt.Fprintf(&sb, "Hi %s. Send me a TRON address and I will screen it: where its money came from, where it went, and which risk categories it touches.\n\n", name)
	switch {
	case b.isAdmin(from.ID):
		sb.WriteString("You are an admin: every feature, no daily limit.\n")
	case trialStarted:
		sb.WriteString(b.trialNotice() + "\n")
	}
	sb.WriteString("\n/plans to subscribe · /help for everything else")
	b.say(ctx, chat, sb.String())
}

func (b *bot) trialNotice() string {
	p, _ := b.billing.Plan(b.billing.Trial.Plan)
	return fmt.Sprintf("Your free %d-day trial has started: the %s plan, %d screens a day.",
		b.billing.Trial.Days, p.Name, p.DailyScreens)
}

// --- screening -------------------------------------------------------------

func (b *bot) screenCommand(ctx context.Context, chat, userID int64, address, kind string) {
	address = strings.TrimSpace(address)
	if address == "" {
		b.say(ctx, chat, "Usage: /"+kind+" <address>")
		return
	}
	// Input hygiene, not a judgement about the address: a typo should not
	// cost a screen from the daily allowance.
	if b.chainID == "tron" && !tron.IsValid(address) {
		b.say(ctx, chat, "That does not look like a TRON address. It should start with T and be 34 characters long.")
		return
	}

	now := b.now()
	limit := 0 // admins: no limit
	if !b.isAdmin(userID) {
		acc, err := b.store.Access(ctx, userID, now)
		if err != nil {
			b.log.Error("access", "user", userID, "error", err)
			b.say(ctx, chat, "Something went wrong on our side. Please try again in a minute.")
			return
		}
		if acc.Plan == nil {
			b.sayWith(ctx, chat, "You have no active plan. Your trial or subscription has ended.\n\nChoose a plan to continue:",
				b.plansKeyboard(ctx, userID))
			return
		}
		if kind == "details" && !acc.Plan.Details || kind == "pdf" && !acc.Plan.PDF {
			b.sayWith(ctx, chat, "/"+kind+" is part of a higher plan. Your plan is "+acc.Plan.Name+".",
				b.plansKeyboard(ctx, userID))
			return
		}
		limit = acc.Plan.DailyScreens
	}

	if !b.claim(userID) {
		b.say(ctx, chat, "Your previous screen is still running. I will answer it first.")
		return
	}
	defer b.release(userID)

	used, ok, err := b.store.Consume(ctx, userID, now, limit)
	if err != nil {
		b.log.Error("consume", "user", userID, "error", err)
		b.say(ctx, chat, "Something went wrong on our side. Please try again in a minute.")
		return
	}
	if !ok {
		b.sayWith(ctx, chat, fmt.Sprintf("You have used all %d screens for today. The count resets at 00:00 UTC.\n\nNeed more? Upgrade:", limit),
			b.plansKeyboard(ctx, userID))
		return
	}

	select {
	case b.screening <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-b.screening }()

	b.say(ctx, chat, "Screening "+address+"...")
	if err := b.deliver(ctx, chat, address, kind); err != nil {
		// A failed screen costs nothing.
		if rerr := b.store.Refund(ctx, userID, now); rerr != nil {
			b.log.Error("refund screen", "user", userID, "error", rerr)
		}
		b.log.Warn("screen failed", "address", address, "error", err)
		b.say(ctx, chat, "Could not screen that address. This did not count toward your daily limit.\n\n"+err.Error())
		return
	}
	if limit > 0 && limit-used <= 3 {
		b.say(ctx, chat, fmt.Sprintf("%d of %d screens left today.", limit-used, limit))
	}
}

func (b *bot) deliver(ctx context.Context, chat int64, address, kind string) error {
	if kind == "pdf" {
		pdf, err := b.report(ctx, address)
		if err != nil {
			return err
		}
		return b.tg.sendDocument(ctx, chat, b.chainID+"-"+address+".pdf", pdf, "Risk report for "+address)
	}
	res, err := b.screen(ctx, address)
	if err != nil {
		return err
	}
	if kind == "details" {
		return b.tg.sendMessage(ctx, chat, format(res), nil)
	}
	return b.tg.sendMessage(ctx, chat, summary(res), nil)
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

// --- plans and status ------------------------------------------------------

func (b *bot) plansText(acc billing.Access) string {
	var sb strings.Builder
	sb.WriteString("Plans, billed monthly:\n")
	for _, p := range b.billing.Plans {
		fmt.Fprintf(&sb, "\n%s: %d screens a day", p.Name, p.DailyScreens)
		if p.Details {
			sb.WriteString(", /details breakdown")
		}
		if p.PDF {
			sb.WriteString(", PDF reports")
		}
		fmt.Fprintf(&sb, "\n  %d Stars a month, renews automatically", p.PriceStars)
		if b.usdtAddr != "" {
			fmt.Fprintf(&sb, "\n  or %s USDT (TRC-20) for %d days", billing.FormatUSDT(p.PriceMicroUSDT), b.billing.PeriodDays)
		}
		sb.WriteString("\n")
	}
	if acc.Plan != nil {
		fmt.Fprintf(&sb, "\nYour plan: %s until %s.", acc.Plan.Name, date(acc.Until))
		if acc.Renews {
			sb.WriteString(" It renews automatically.")
		}
	}
	sb.WriteString("\n\nPaying early never loses days: a new period starts when the current one ends.")
	return sb.String()
}

func (b *bot) plans(ctx context.Context, chat, userID int64) {
	acc, err := b.store.Access(ctx, userID, b.now())
	if err != nil {
		b.log.Error("access", "error", err)
	}
	b.sayWith(ctx, chat, b.plansText(acc), b.plansKeyboard(ctx, userID))
}

// plansKeyboard has a Stars button and, when enabled, a USDT button per plan.
// A plan the user already renews through Stars shows as active rather than
// offering a second concurrent subscription.
func (b *bot) plansKeyboard(ctx context.Context, userID int64) *keyboard {
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

	kb := &keyboard{}
	for _, p := range b.billing.Plans {
		var row []button
		if renewing[p.ID] {
			row = append(row, button{Text: "✅ " + p.Name + " active", CallbackData: "status"})
		} else if link, err := b.starsLink(ctx, &p); err == nil {
			row = append(row, button{Text: fmt.Sprintf("⭐ %s · %d Stars/mo", p.Name, p.PriceStars), URL: link})
		} else {
			b.log.Error("stars invoice link", "plan", p.ID, "error", err)
		}
		if b.usdtAddr != "" {
			row = append(row, button{Text: fmt.Sprintf("💵 %s · %s USDT", p.Name, billing.FormatUSDT(p.PriceMicroUSDT)),
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

func (b *bot) starsLink(ctx context.Context, p *billing.Plan) (string, error) {
	b.mu.Lock()
	link, ok := b.links[p.ID]
	b.mu.Unlock()
	if ok {
		return link, nil
	}
	desc := fmt.Sprintf("%d address screens a day", p.DailyScreens)
	if p.Details {
		desc += ", full breakdowns"
	}
	if p.PDF {
		desc += ", PDF reports"
	}
	desc += ". Renews every 30 days; cancel any time with /cancel."
	link, err := b.tg.createSubscriptionLink(ctx, "Risk screening "+p.Name, desc, starsPayload(p.ID), int64(p.PriceStars))
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	b.links[p.ID] = link
	b.mu.Unlock()
	return link, nil
}

func (b *bot) status(ctx context.Context, chat, userID int64) {
	now := b.now()
	if b.isAdmin(userID) {
		b.say(ctx, chat, fmt.Sprintf("Admin: every feature, no daily limit.\nYour user id: %d", userID))
		return
	}
	acc, err := b.store.Access(ctx, userID, now)
	if err != nil {
		b.log.Error("access", "error", err)
		b.say(ctx, chat, "Something went wrong on our side. Please try again in a minute.")
		return
	}
	used, _ := b.store.Used(ctx, userID, now)
	var sb strings.Builder
	if acc.Plan == nil {
		sb.WriteString("No active plan.\n\n/plans to subscribe.")
	} else {
		how := map[string]string{"trial": "free trial", "stars": "Telegram Stars", "usdt": "USDT", "grant": "granted"}[acc.Source]
		fmt.Fprintf(&sb, "Plan: %s (%s)\nActive until: %s\n", acc.Plan.Name, how, datetime(acc.Until))
		if acc.Renews {
			sb.WriteString("Renews automatically. /cancel to stop.\n")
		} else if acc.Source != "trial" {
			sb.WriteString("Does not renew automatically. /plans to extend.\n")
		}
		fmt.Fprintf(&sb, "Today: %d of %d screens used (resets 00:00 UTC)\n", used, acc.Plan.DailyScreens)
	}
	fmt.Fprintf(&sb, "\nYour user id: %d", userID)
	b.say(ctx, chat, sb.String())
}

func (b *bot) cancel(ctx context.Context, chat, userID int64) {
	now := b.now()
	charges, err := b.store.RenewingCharges(ctx, userID, now)
	if err != nil {
		b.log.Error("renewing charges", "error", err)
		b.say(ctx, chat, "Something went wrong on our side. Please try again in a minute.")
		return
	}
	if len(charges) == 0 {
		b.say(ctx, chat, "Nothing renews automatically, so there is nothing to cancel. USDT payments and trials never renew by themselves.")
		return
	}
	for _, ch := range charges {
		if err := b.tg.editUserStarSubscription(ctx, userID, ch, true); err != nil {
			b.log.Error("cancel stars subscription", "user", userID, "charge", ch, "error", err)
			b.say(ctx, chat, "Telegram did not accept the cancellation. You can also cancel in Telegram: Settings > My Stars > Subscriptions. Or contact "+b.support)
			return
		}
		if err := b.store.MarkRenewalCanceled(ctx, ch, now); err != nil {
			b.log.Error("mark renewal canceled", "charge", ch, "error", err)
		}
	}
	acc, _ := b.store.Access(ctx, userID, now)
	msg := "Automatic renewal is cancelled. You will not be charged again."
	if acc.Plan != nil {
		msg += fmt.Sprintf("\n\nYour %s plan stays active until %s.", acc.Plan.Name, date(acc.Until))
	}
	b.say(ctx, chat, msg)
}

func (b *bot) paySupport(ctx context.Context, chat, userID int64) {
	subs, _ := b.store.Subscriptions(ctx, userID)
	var sb strings.Builder
	sb.WriteString("Payment help: " + b.support + "\n\nPlease include your user id (" + strconv.FormatInt(userID, 10) + ")")
	var paid []billing.Subscription
	for _, s := range subs {
		if s.Source == "stars" || s.Source == "usdt" {
			paid = append(paid, s)
		}
	}
	if len(paid) > 0 {
		sb.WriteString(" and the payment in question. Your payments:\n")
		for i := len(paid) - 1; i >= 0 && i >= len(paid)-5; i-- {
			s := paid[i]
			p, _ := b.billing.Plan(s.Plan)
			name := s.Plan
			if p != nil {
				name = p.Name
			}
			fmt.Fprintf(&sb, "\n%s  %s via %s", date(s.StartsAt), name, s.Source)
			if s.StarsChargeID != "" {
				fmt.Fprintf(&sb, "\n  charge %s", s.StarsChargeID)
			}
			if s.RevokedAt != nil {
				sb.WriteString("  (refunded)")
			}
		}
	} else {
		sb.WriteString(".")
	}
	sb.WriteString("\n\nUSDT sent with the wrong amount or after an invoice expired is kept on record; support can apply it by hand.")
	b.say(ctx, chat, sb.String())
}

// --- Stars payments --------------------------------------------------------

// preCheckout approves a Stars payment only if it matches a current plan and
// price. Telegram allows 10 seconds for the answer, so nothing slow happens
// here.
func (b *bot) preCheckout(ctx context.Context, q *preCheckoutQuery) {
	ok, reason := b.checkStarsOrder(q.Currency, q.TotalAmount, q.InvoicePayload)
	if !ok {
		b.log.Warn("pre-checkout rejected", "user", q.From.ID, "payload", q.InvoicePayload, "reason", reason)
	}
	if err := b.tg.answerPreCheckoutQuery(ctx, q.ID, ok, reason); err != nil {
		b.log.Error("answer pre-checkout", "user", q.From.ID, "error", err)
	}
}

func (b *bot) checkStarsOrder(currency string, amount int64, payload string) (bool, string) {
	id, found := strings.CutPrefix(payload, "plan:")
	p, ok := b.billing.Plan(id)
	if !found || !ok {
		return false, "This plan is no longer offered. Please open /plans again."
	}
	if currency != "XTR" || amount != int64(p.PriceStars) {
		return false, "The price of this plan has changed. Please open /plans again."
	}
	return true, ""
}

func (b *bot) paid(ctx context.Context, m *message) {
	sp := m.SuccessfulPayment
	if sp.Currency != "XTR" {
		b.log.Error("payment in unexpected currency", "user", m.From.ID, "currency", sp.Currency)
		return
	}
	id, _ := strings.CutPrefix(sp.InvoicePayload, "plan:")
	now := b.now()
	var expires time.Time
	if sp.SubscriptionExpirationDate > 0 {
		expires = time.Unix(sp.SubscriptionExpirationDate, 0).UTC()
	}
	recorded, err := b.store.RecordStars(ctx, billing.StarsPayment{
		UserID: m.From.ID, Plan: id, Amount: sp.TotalAmount, ChargeID: sp.TelegramPaymentChargeID,
		Recurring: sp.IsRecurring, ExpiresAt: expires, ReceivedAt: now,
	})
	if err != nil {
		// The money has been taken. Never leave a paying customer without
		// access silently: tell them and every admin.
		b.log.Error("record stars payment", "user", m.From.ID, "charge", sp.TelegramPaymentChargeID, "error", err)
		b.say(ctx, m.Chat.ID, "Your payment went through, but I could not activate your plan. Support has been told and will fix it; you can also write to "+b.support)
		b.notifyAdmins(ctx, fmt.Sprintf("⚠️ Stars payment NOT activated\nuser %d, charge %s, %d Stars, payload %q\nerror: %v",
			m.From.ID, sp.TelegramPaymentChargeID, sp.TotalAmount, sp.InvoicePayload, err))
		return
	}
	if !recorded {
		return // a repeated delivery of a payment already handled
	}
	acc, _ := b.store.Access(ctx, m.From.ID, now)
	name, until := id, expires
	if acc.Plan != nil {
		name, until = acc.Plan.Name, acc.Until
	}
	msg := fmt.Sprintf("Thank you! %s is active until %s and renews automatically. /cancel stops renewal at any time.", name, date(until))
	if sp.IsRecurring && !sp.IsFirstRecurring {
		msg = fmt.Sprintf("Your %s subscription renewed. Active until %s.", name, date(until))
	}
	b.say(ctx, m.Chat.ID, msg)
	b.notifyAdmins(ctx, fmt.Sprintf("⭐ %d Stars from %s for %s", sp.TotalAmount, who(m.From), name))
}

func (b *bot) refunded(ctx context.Context, m *message) {
	rp := m.RefundedPayment
	uid, err := b.store.RevokeCharge(ctx, rp.TelegramPaymentChargeID, "refunded", b.now())
	if err != nil {
		b.log.Error("revoke refunded charge", "charge", rp.TelegramPaymentChargeID, "error", err)
		return
	}
	if uid != 0 {
		b.say(ctx, m.Chat.ID, "Your payment was refunded, and the plan it paid for has ended.")
	}
}

// --- USDT invoices ---------------------------------------------------------

func (b *bot) callback(ctx context.Context, q *callbackQuery) {
	chat := q.From.ID
	if q.Message != nil {
		chat = q.Message.Chat.ID
	}
	switch {
	case q.Data == "status":
		_ = b.tg.answerCallbackQuery(ctx, q.ID, "")
		b.status(ctx, chat, q.From.ID)
	case strings.HasPrefix(q.Data, "usdt:"):
		b.usdtInvoice(ctx, q, chat, strings.TrimPrefix(q.Data, "usdt:"))
	default:
		_ = b.tg.answerCallbackQuery(ctx, q.ID, "")
	}
}

func (b *bot) usdtInvoice(ctx context.Context, q *callbackQuery, chat int64, planID string) {
	p, ok := b.billing.Plan(planID)
	if !ok || b.usdtAddr == "" {
		_ = b.tg.answerCallbackQuery(ctx, q.ID, "This option is no longer available.")
		return
	}
	if _, err := b.store.Touch(ctx, billing.User{ID: q.From.ID, Username: q.From.Username, FirstName: q.From.FirstName}); err != nil {
		b.log.Error("touch user", "error", err)
	}
	inv, err := b.store.CreateInvoice(ctx, q.From.ID, p.ID, b.usdtAddr, b.now(), rand.IntN(1000))
	if err != nil {
		b.log.Error("create usdt invoice", "user", q.From.ID, "error", err)
		_ = b.tg.answerCallbackQuery(ctx, q.ID, "Could not create an invoice. Please try again in a few minutes.")
		return
	}
	_ = b.tg.answerCallbackQuery(ctx, q.ID, "")
	amount := billing.FormatUSDT(inv.Amount)
	mins := int(inv.ExpiresAt.Sub(b.now()).Round(time.Minute).Minutes())
	b.say(ctx, chat, fmt.Sprintf(`%s for %d days, paid in USDT:

Send exactly %s USDT
Network: TRON (TRC-20) only
To the address in the next message

The exact amount identifies your payment. Send %s, not a rounded figure. If you withdraw from an exchange, make sure the amount that arrives is %s: the exchange's fee must not come out of it.

This invoice is valid for %d minutes. I will message you here as soon as the payment is confirmed, usually within 2 minutes.`,
		p.Name, b.billing.PeriodDays, amount, amount, amount, mins))
	// On its own so it can be copied with one tap.
	b.say(ctx, chat, b.usdtAddr)
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
		b.say(ctx, uid, fmt.Sprintf("You have been given the %s plan until %s. Send an address to start.", p.Name, date(until)))

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
		var sb strings.Builder
		fmt.Fprintf(&sb, "User %d\n", uid)
		if acc.Plan != nil {
			fmt.Fprintf(&sb, "Plan: %s via %s until %s (renews: %v)\nToday: %d/%d\n",
				acc.Plan.Name, acc.Source, datetime(acc.Until), acc.Renews, used, acc.Plan.DailyScreens)
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
