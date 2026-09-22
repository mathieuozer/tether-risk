package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mozer/tether-risk/internal/billing"
	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/store/storetest"
)

const (
	addrA   = "TNwf8VBNCkg7Y1pgyzbHdWdekkamoqcrmL"
	payAddr = "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7"
	adminID = 1000
)

// call is one request the bot made to the fake Telegram.
type call struct {
	Method string
	Params map[string]any
}

type fakeTelegram struct {
	mu    sync.Mutex
	calls []call
}

func (f *fakeTelegram) handler(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/file/") {
		// A batch file: two valid addresses, one repeated, one invalid.
		w.Write([]byte(addrA + "\n" + addrA + "\n" + payAddr + "\nnot-an-address\n"))
		return
	}
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	params := map[string]any{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(r.Body).Decode(&params)
	} else {
		_ = r.ParseMultipartForm(1 << 20)
		if r.MultipartForm != nil {
			for k, v := range r.MultipartForm.Value {
				params[k] = v[0]
			}
			if fh := r.MultipartForm.File["document"]; len(fh) > 0 {
				params["document"] = fh[0].Filename
			}
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{method, params})
	f.mu.Unlock()

	var result any = true
	switch method {
	case "createInvoiceLink":
		result = "https://t.me/$invoice-" + fmt.Sprint(params["payload"])
	case "getFile":
		result = map[string]any{"file_path": "docs/batch.txt", "file_size": 100}
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// sent returns the texts of messages sent to a chat, in order.
func (f *fakeTelegram) sent(chat int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.Method == "sendMessage" && int64(c.Params["chat_id"].(float64)) == chat {
			out = append(out, c.Params["text"].(string))
		}
	}
	return out
}

func (f *fakeTelegram) last(method string) (call, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Method == method {
			return f.calls[i], true
		}
	}
	return call{}, false
}

func (f *fakeTelegram) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

type harness struct {
	b     *bot
	tg    *fakeTelegram
	clock time.Time
	tron  *string // body the fake TronGrid returns

	mu     sync.Mutex
	result map[string]any // fields the fake screening API returns
}

// setResult changes what the fake screening API returns.
func (h *harness) setResult(fields map[string]any) {
	h.mu.Lock()
	h.result = fields
	h.mu.Unlock()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pg := storetest.Postgres(t, "tether_risk_bot_test")
	for _, tbl := range []string{"usage_daily", "subscriptions", "usdt_invoices", "usdt_unmatched", "usdt_watch",
		"screen_history", "watches", "api_keys", "bot_users"} {
		if _, err := pg.Exec("TRUNCATE TABLE " + tbl + " CASCADE"); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := billing.Load(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}

	tg := &fakeTelegram{}
	tgSrv := httptest.NewServer(http.HandlerFunc(tg.handler))
	t.Cleanup(tgSrv.Close)

	h := &harness{tg: tg, clock: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), result: map[string]any{
		"score": 15.0, "band": "low", "coverage": 0.99,
	}}
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/report" {
			w.Header().Set("Content-Type", "application/pdf")
			w.Write([]byte("%PDF-1.4 fake"))
			return
		}
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		out := map[string]any{"address": req["address"], "chain": req["chain"]}
		for k, v := range h.result {
			out[k] = v
		}
		h.mu.Unlock()
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(apiSrv.Close)

	tronBody := `{"success":true,"meta":{},"data":[]}`
	tronSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(tronBody))
	}))
	t.Cleanup(tronSrv.Close)

	h.tron = &tronBody
	h.b = &bot{
		tg:        newTelegram(tgSrv.URL, "TEST:TOKEN"),
		apiURL:    apiSrv.URL,
		chainID:   "tron",
		http:      apiSrv.Client(),
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		billing:   cfg,
		store:     billing.NewStore(pg, cfg),
		admins:    map[int64]bool{adminID: true},
		support:   "@support",
		terms:     map[string]string{langEN: "TERMS @support", langTR: "KOŞULLAR @support"},
		usdtAddr:  payAddr,
		tron:      tron.NewClient(tron.Options{BaseURL: tronSrv.URL, RequestsPerSecond: 1000}),
		screening: make(chan struct{}, 2),
		busy:      map[int64]bool{},
		links:     map[string]string{},
		now:       func() time.Time { return h.clock },

		chains:      []chainInfo{{ID: "tron", Name: "Tron", Enabled: true}, {ID: "ethereum", Name: "Ethereum"}},
		appURL:      "https://risk.example.com/app/",
		monitorWake: make(chan struct{}, 1),
		root:        context.Background(),
		limiter:     &keyLimiter{},
	}
	return h
}

func (h *harness) text(user int64, text string) {
	h.b.dispatch(context.Background(), update{Message: &message{
		From: &tgUser{ID: user, Username: fmt.Sprintf("u%d", user), FirstName: "Test"},
		Chat: tgChat{ID: user, Type: "private"}, Text: text,
	}})
}

func contains(msgs []string, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

func TestNewUserGetsTrialAndScreens(t *testing.T) {
	h := newHarness(t)
	h.text(1, addrA)
	msgs := h.tg.sent(1)
	if !contains(msgs, "free 7-day trial has started") {
		t.Errorf("no trial notice in %q", msgs)
	}
	if !contains(msgs, "Screening "+addrA) || !contains(msgs, "Risk level: Low") {
		t.Errorf("no screen result in %q", msgs)
	}
	used, _ := h.b.store.Used(context.Background(), 1, h.clock)
	if used != 1 {
		t.Errorf("used = %d, want 1", used)
	}

	// The trial is granted once: a second message does not announce it.
	h.tg.reset()
	h.text(1, "/start")
	if contains(h.tg.sent(1), "trial has started") {
		t.Error("trial announced twice")
	}
}

func TestInvalidAddressCostsNothing(t *testing.T) {
	h := newHarness(t)
	h.text(2, "hello there")
	if !contains(h.tg.sent(2), "does not look like a blockchain address") {
		t.Errorf("got %q", h.tg.sent(2))
	}
	if used, _ := h.b.store.Used(context.Background(), 2, h.clock); used != 0 {
		t.Errorf("an invalid address used %d screens", used)
	}
}

func TestExpiredTrialIsAskedToSubscribe(t *testing.T) {
	h := newHarness(t)
	h.text(3, "/start")
	h.clock = h.clock.Add(8 * 24 * time.Hour)
	h.tg.reset()
	h.text(3, addrA)
	if !contains(h.tg.sent(3), "no active plan") {
		t.Fatalf("got %q", h.tg.sent(3))
	}
	c, _ := h.tg.last("sendMessage")
	if c.Params["reply_markup"] == nil {
		t.Error("the prompt to subscribe carries no plan buttons")
	}
	if contains(h.tg.sent(3), "Screening") {
		t.Error("screened without a plan")
	}
}

func TestDailyLimitAndProFeatures(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.text(4, "/start") // basic trial: 20 a day, no /details or /pdf

	h.tg.reset()
	h.text(4, "/details "+addrA)
	if !contains(h.tg.sent(4), "part of a higher plan") {
		t.Errorf("basic user got /details: %q", h.tg.sent(4))
	}

	for i := 0; i < 20; i++ {
		if _, ok, _ := h.b.store.Consume(ctx, 4, h.clock, 20); !ok {
			t.Fatal("consume failed early")
		}
	}
	h.tg.reset()
	h.text(4, addrA)
	if !contains(h.tg.sent(4), "used all 20 screens for today") {
		t.Errorf("limit not enforced: %q", h.tg.sent(4))
	}

	// Pro unlocks the PDF.
	if _, err := h.b.store.Grant(ctx, 4, "pro", 30, "test", h.clock); err != nil {
		t.Fatal(err)
	}
	h.tg.reset()
	h.text(4, "/pdf "+addrA)
	doc, ok := h.tg.last("sendDocument")
	if !ok || doc.Params["document"] != "tron-"+addrA+".pdf" {
		t.Errorf("no PDF sent; calls: %+v", h.tg.calls)
	}
}

func TestStarsPurchaseFlow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.text(5, "/plans")
	link, ok := h.tg.last("createInvoiceLink")
	if !ok || link.Params["subscription_period"].(float64) != 2592000 || link.Params["currency"] != "XTR" {
		t.Fatalf("invoice link params: %+v", link.Params)
	}

	// Pre-checkout: a stale price is refused, the current one accepted.
	h.b.dispatch(ctx, update{PreCheckoutQuery: &preCheckoutQuery{ID: "q1", From: tgUser{ID: 5},
		Currency: "XTR", TotalAmount: 1, InvoicePayload: "plan:pro"}})
	if c, _ := h.tg.last("answerPreCheckoutQuery"); c.Params["ok"] != false {
		t.Errorf("wrong price accepted: %+v", c.Params)
	}
	h.b.dispatch(ctx, update{PreCheckoutQuery: &preCheckoutQuery{ID: "q2", From: tgUser{ID: 5},
		Currency: "XTR", TotalAmount: 3800, InvoicePayload: "plan:pro"}})
	if c, _ := h.tg.last("answerPreCheckoutQuery"); c.Params["ok"] != true {
		t.Errorf("correct order refused: %+v", c.Params)
	}

	paid := &message{From: &tgUser{ID: 5}, Chat: tgChat{ID: 5, Type: "private"},
		SuccessfulPayment: &successfulPayment{Currency: "XTR", TotalAmount: 3800, InvoicePayload: "plan:pro",
			SubscriptionExpirationDate: h.clock.Add(30 * 24 * time.Hour).Unix(),
			IsRecurring:                true, IsFirstRecurring: true, TelegramPaymentChargeID: "charge-1"}}
	h.tg.reset()
	h.b.dispatch(ctx, update{Message: paid})
	h.b.dispatch(ctx, update{Message: paid}) // Telegram redelivers
	if n := len(h.tg.sent(5)); n != 1 {
		t.Errorf("customer told %d times, want once: %q", n, h.tg.sent(5))
	}
	if !contains(h.tg.sent(adminID), "3800 Stars") {
		t.Error("admin not told about the sale")
	}
	acc, _ := h.b.store.Access(ctx, 5, h.clock.Add(time.Hour))
	if acc.Plan == nil || acc.Plan.ID != "pro" || !acc.Renews {
		t.Fatalf("access after payment: %+v", acc)
	}

	// /cancel stops renewal with Telegram and keeps access.
	h.text(5, "/cancel")
	if c, ok := h.tg.last("editUserStarSubscription"); !ok || c.Params["is_canceled"] != true ||
		c.Params["telegram_payment_charge_id"] != "charge-1" {
		t.Errorf("cancel call: %+v", c.Params)
	}
	acc, _ = h.b.store.Access(ctx, 5, h.clock.Add(time.Hour))
	if acc.Plan == nil || acc.Renews {
		t.Errorf("after cancel: %+v", acc)
	}

	// A refund ends access.
	h.b.dispatch(ctx, update{Message: &message{From: &tgUser{ID: 5}, Chat: tgChat{ID: 5, Type: "private"},
		RefundedPayment: &refundedPayment{Currency: "XTR", TotalAmount: 3800, TelegramPaymentChargeID: "charge-1"}}})
	if acc, _ := h.b.store.Access(ctx, 5, h.clock.Add(2*time.Hour)); acc.Plan != nil && acc.Source == "stars" {
		t.Errorf("refunded plan still active: %+v", acc)
	}
}

func TestUSDTPurchaseFlow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.text(6, "/start")
	h.b.dispatch(ctx, update{CallbackQuery: &callbackQuery{ID: "cb", From: tgUser{ID: 6},
		Message: &message{Chat: tgChat{ID: 6, Type: "private"}}, Data: "usdt:pro"}})

	invs, _ := h.b.store.OpenInvoices(ctx, payAddr, h.clock)
	if len(invs) != 1 {
		t.Fatalf("%d invoices open, want 1", len(invs))
	}
	amount := billing.FormatUSDT(invs[0].Amount)
	if !contains(h.tg.sent(6), "Send exactly "+amount+" USDT") || !contains(h.tg.sent(6), payAddr) {
		t.Fatalf("invoice message: %q", h.tg.sent(6))
	}

	// The payment arrives on chain: a wrong amount first, then the right one.
	paidAt := h.clock.Add(3 * time.Minute)
	item := func(tx string, value int64) string {
		return fmt.Sprintf(`{"transaction_id":"%s","block_timestamp":%d,"from":"TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM","to":"%s","type":"Transfer","value":"%d","token_info":{"address":"%s"}}`,
			tx, paidAt.UnixMilli(), payAddr, value, tron.USDTContract)
	}
	*h.tron = `{"success":true,"meta":{},"data":[` + item("tx-wrong", 49_000_000) + `,` + item("tx-right", invs[0].Amount) + `]}`
	h.clock = h.clock.Add(4 * time.Minute)
	h.tg.reset()
	if err := h.b.scanUSDT(ctx); err != nil {
		t.Fatal(err)
	}
	if !contains(h.tg.sent(6), "Payment received: "+amount+" USDT") {
		t.Errorf("customer not told: %q", h.tg.sent(6))
	}
	if !contains(h.tg.sent(adminID), "matches no invoice: 49.00 USDT") {
		t.Errorf("admin not told about the unmatched payment: %q", h.tg.sent(adminID))
	}
	acc, _ := h.b.store.Access(ctx, 6, h.clock)
	if acc.Plan == nil || acc.Plan.ID != "pro" || acc.Source != "usdt" {
		t.Fatalf("access after USDT: %+v", acc)
	}

	// A second scan of the same transfers grants nothing more.
	h.tg.reset()
	if err := h.b.scanUSDT(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.tg.sent(6)) != 0 || len(h.tg.sent(adminID)) != 0 {
		t.Errorf("rescan sent messages: %q %q", h.tg.sent(6), h.tg.sent(adminID))
	}
}

func TestAdminCommandsAreAdminOnly(t *testing.T) {
	h := newHarness(t)
	h.text(7, "/start")
	h.text(7, "/grant 7 pro 30")
	if acc, _ := h.b.store.Access(context.Background(), 7, h.clock); acc.Plan.ID == "pro" {
		t.Fatal("a customer granted themselves pro")
	}
	h.text(adminID, "/grant @u7 pro 30 friend")
	acc, _ := h.b.store.Access(context.Background(), 7, h.clock)
	if acc.Plan == nil || acc.Plan.ID != "pro" {
		t.Fatalf("admin grant failed: %+v; admin saw %q", acc, h.tg.sent(adminID))
	}
	h.text(adminID, "/stats")
	if !contains(h.tg.sent(adminID), "Users: 2") {
		t.Errorf("stats: %q", h.tg.sent(adminID))
	}
}

func TestGroupChatsAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.b.dispatch(context.Background(), update{Message: &message{
		From: &tgUser{ID: 8}, Chat: tgChat{ID: -100, Type: "group"}, Text: addrA}})
	if len(h.tg.calls) != 0 {
		t.Errorf("bot answered in a group: %+v", h.tg.calls)
	}
}

func TestSplitMessageKeepsEverything(t *testing.T) {
	text := strings.Repeat("line of text\n", 1000)
	parts := splitMessage(text, 4000)
	if len(parts) < 3 {
		t.Fatalf("got %d parts", len(parts))
	}
	for _, p := range parts {
		if len(p) > 4000 {
			t.Errorf("part of %d bytes", len(p))
		}
	}
	if strings.Count(strings.Join(parts, "\n"), "line of text") != 1000 {
		t.Error("lines lost in splitting")
	}
}
