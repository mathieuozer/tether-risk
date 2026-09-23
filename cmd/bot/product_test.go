package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mozer/tether-risk/internal/billing"
)

// --- Mini App authentication -------------------------------------------------

// signInitData builds initData the way Telegram does, for tests.
func signInitData(token string, user map[string]any, authDate time.Time) string {
	u, _ := json.Marshal(user)
	vals := url.Values{"user": {string(u)}, "auth_date": {fmt.Sprint(authDate.Unix())}, "query_id": {"AAE"}}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = k + "=" + vals.Get(k)
	}
	secret := hmacSHA256([]byte("WebAppData"), []byte(token))
	vals.Set("hash", hex.EncodeToString(hmacSHA256(secret, []byte(strings.Join(lines, "\n")))))
	return vals.Encode()
}

func TestInitDataVerification(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	good := signInitData("TOKEN", map[string]any{"id": 42, "first_name": "Ada"}, now.Add(-time.Hour))

	if _, err := verifyInitData(good, "TOKEN", now, 24*time.Hour); err != nil {
		t.Fatalf("valid init data rejected: %v", err)
	}
	if _, err := verifyInitData(good, "OTHER", now, 24*time.Hour); err == nil {
		t.Error("init data signed for another bot accepted")
	}
	tampered := strings.Replace(good, "42", "43", 1)
	if _, err := verifyInitData(tampered, "TOKEN", now, 24*time.Hour); err == nil {
		t.Error("tampered user id accepted")
	}
	if _, err := verifyInitData(good, "TOKEN", now.Add(48*time.Hour), 24*time.Hour); err == nil {
		t.Error("expired init data accepted")
	}
}

// app is a test client for the HTTP surfaces.
type app struct {
	t   *testing.T
	srv *httptest.Server
	h   *harness
}

func newApp(t *testing.T, h *harness) *app {
	srv := httptest.NewServer(h.b.routes())
	t.Cleanup(srv.Close)
	return &app{t: t, srv: srv, h: h}
}

func (a *app) do(method, path, auth string, body any) (int, map[string]any, []byte) {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, a.srv.URL+path, rd)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.srv.Client().Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, raw
}

func (a *app) tma(userID int64, lang string) string {
	return "tma " + signInitData("TEST:TOKEN",
		map[string]any{"id": userID, "first_name": "Ada", "username": fmt.Sprintf("ada%d", userID), "language_code": lang},
		a.h.clock.Add(-time.Minute))
}

func TestAppRejectsMissingOrForgedAuth(t *testing.T) {
	a := newApp(t, newHarness(t))
	for _, auth := range []string{"", "tma garbage", "tma " + signInitData("WRONG", map[string]any{"id": 1}, time.Now())} {
		code, body, _ := a.do("GET", "/app/api/me", auth, nil)
		if code != http.StatusUnauthorized || body["error"] != "unauthorized" {
			t.Errorf("auth %q: got %d %v", auth, code, body)
		}
	}
}

func TestAppServesTheInterface(t *testing.T) {
	a := newApp(t, newHarness(t))
	resp, err := a.srv.Client().Get(a.srv.URL + "/app/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "telegram-web-app.js") {
		t.Fatalf("GET /app/: %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self' https://telegram.org") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestAppMeScreenAndHistory(t *testing.T) {
	h := newHarness(t)
	a := newApp(t, h)
	auth := a.tma(50, "tr")

	// Opening the app starts the trial.
	code, me, _ := a.do("GET", "/app/api/me", auth, nil)
	if code != 200 {
		t.Fatalf("me: %d %v", code, me)
	}
	access, _ := me["access"].(map[string]any)
	if access == nil || access["source"] != "trial" {
		t.Fatalf("access = %v, want the trial", me["access"])
	}
	if me["user"].(map[string]any)["lang"] != "tr" {
		t.Errorf("lang = %v, want tr from the client", me["user"])
	}

	code, res, _ := a.do("POST", "/app/api/screen", auth, map[string]string{"address": addrA})
	if code != 200 {
		t.Fatalf("screen: %d %v", code, res)
	}
	result := res["result"].(map[string]any)
	if result["address"] != addrA || result["band"] != "low" {
		t.Errorf("result = %v", result)
	}
	if u := res["usage"].(map[string]any); u["screens_today"].(float64) != 1 || u["daily_screens"].(float64) != 20 {
		t.Errorf("usage = %v", u)
	}

	_, hist, _ := a.do("GET", "/app/api/history", auth, nil)
	items := hist["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["channel"] != "app" {
		t.Fatalf("history = %v", hist)
	}

	// A PDF needs Pro; the trial is Basic.
	code, body, _ := a.do("POST", "/app/api/report", auth, map[string]string{"address": addrA})
	if code != http.StatusForbidden || body["error"] != "feature_locked" {
		t.Errorf("report on basic: %d %v", code, body)
	}

	// A payment check: the decision, and the recipient's full screen beside it.
	code, ps, _ := a.do("POST", "/app/api/presend", auth, map[string]string{"to": addrA, "from": addrB})
	if code != 200 {
		t.Fatalf("presend: %d %v", code, ps)
	}
	if p := ps["presend"].(map[string]any); p["decision"] != "do_not_send" || p["lookalike_of"] != addrB || p["to"] != addrA {
		t.Errorf("presend = %v", p)
	}
	if r := ps["result"].(map[string]any); r["address"] != addrA || r["band"] != "low" {
		t.Errorf("presend recipient = %v", r)
	}
	code, body, _ = a.do("POST", "/app/api/presend", auth, map[string]string{"to": addrA, "from": "nonsense"})
	if code != 400 || body["error"] != "bad_address" {
		t.Errorf("presend with an invalid payer: %d %v", code, body)
	}

	// An unsupported chain is named, not treated as a typo.
	code, body, _ = a.do("POST", "/app/api/screen", auth, map[string]string{"address": "0x" + strings.Repeat("a", 40)})
	if code != 400 || body["error"] != "chain_unavailable" {
		t.Errorf("ethereum: %d %v", code, body)
	}
}

func TestAppPayments(t *testing.T) {
	h := newHarness(t)
	a := newApp(t, h)
	auth := a.tma(51, "en")
	code, body, _ := a.do("POST", "/app/api/invoice/stars", auth, map[string]string{"plan": "pro"})
	if code != 200 || !strings.HasPrefix(body["link"].(string), "https://t.me/$invoice-plan:pro") {
		t.Errorf("stars: %d %v", code, body)
	}
	code, body, _ = a.do("POST", "/app/api/invoice/usdt", auth, map[string]string{"plan": "pro"})
	if code != 200 || body["address"] != payAddr || !strings.HasPrefix(body["amount"].(string), "49.") {
		t.Errorf("usdt: %d %v", code, body)
	}
	code, _, _ = a.do("POST", "/app/api/invoice/usdt", auth, map[string]string{"plan": "nope"})
	if code != 404 {
		t.Errorf("unknown plan: %d", code)
	}
}

// --- watches and alerts ------------------------------------------------------

func TestWatchLimitAndAlerts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.text(60, "/start") // basic trial: 3 watches

	addrs := []string{addrA, payAddr, "TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM", "TBA6CypYJizwA9XdC7Ubgc5F1bxrQ7SqPt"}
	for _, ad := range addrs[:3] {
		h.text(60, "/watch "+ad)
	}
	h.tg.reset()
	h.text(60, "/watch "+addrs[3])
	if !contains(h.tg.sent(60), "watches up to 3 addresses") {
		t.Fatalf("fourth watch: %q", h.tg.sent(60))
	}

	// First pass: a baseline, no alert.
	h.tg.reset()
	if err := h.b.monitorPass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.tg.sent(60)) != 0 {
		t.Fatalf("baseline check alerted: %q", h.tg.sent(60))
	}

	// Not due yet: nothing is rechecked.
	h.setResult(map[string]any{"score": 80.0, "band": "high", "coverage": 0.9})
	if err := h.b.monitorPass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.tg.sent(60)) != 0 {
		t.Fatal("checked before the interval")
	}

	// After the interval the address has turned high risk with sanctions.
	h.setResult(map[string]any{"score": 80.0, "band": "high", "coverage": 0.9,
		"inbound": map[string]any{"total_traced": 1.0, "categories": []map[string]any{{"category": "sanctions", "pct": 12.0}}}})
	h.clock = h.clock.Add(7 * time.Hour)
	if err := h.b.monitorPass(ctx); err != nil {
		t.Fatal(err)
	}
	msgs := h.tg.sent(60)
	if len(msgs) != 3 || !contains(msgs, "Band: Low → High (score 15.0 → 80.0)") || !contains(msgs, "New exposure: Sanctions") {
		t.Fatalf("alerts: %q", msgs)
	}

	// Unchanged on the next pass: no repeat.
	h.tg.reset()
	h.clock = h.clock.Add(7 * time.Hour)
	if err := h.b.monitorPass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.tg.sent(60)) != 0 {
		t.Errorf("repeated alert: %q", h.tg.sent(60))
	}
}

func TestCompareOnlyAlertsOnWorsening(t *testing.T) {
	low := &billing.WatchState{Band: "low", RiskCategories: []string{"gambling"}}
	if compare(nil, billing.WatchState{Band: "high"}).any() {
		t.Error("first check alerted")
	}
	if compare(low, billing.WatchState{Band: "low"}).any() {
		t.Error("a category disappearing alerted")
	}
	if w := compare(low, billing.WatchState{Band: "medium", RiskCategories: []string{"gambling", "mixer"}}); !w.BandUp || len(w.NewCategories) != 1 {
		t.Errorf("worsening missed: %+v", w)
	}
	if !compare(low, billing.WatchState{Band: "low", RiskCategories: []string{"gambling"}, Listed: true}).NewlyListed {
		t.Error("new direct listing missed")
	}
}

// --- batch -------------------------------------------------------------------

func TestParseBatch(t *testing.T) {
	h := newHarness(t)
	text := "address,note\n" + addrA + ",x\n" + addrA + "\n" + payAddr + "; junk\n0x" + strings.Repeat("b", 40)
	targets, invalid := h.b.parseBatch(text)
	if len(targets) != 2 {
		t.Fatalf("targets = %v, want the two TRON addresses once each", targets)
	}
	// "address", "note", "x", "junk" and the disabled-chain address.
	if invalid != 5 {
		t.Errorf("invalid = %d, want 5", invalid)
	}
}

func TestBatchFileNeedsProAndDelivers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	doc := func() {
		h.b.dispatch(ctx, update{Message: &message{From: &tgUser{ID: 70}, Chat: tgChat{ID: 70, Type: "private"},
			Document: &document{FileID: "f1", FileName: "list.txt", FileSize: 100}}})
	}
	doc()
	if !contains(h.tg.sent(70), "Batch screening is part of the Pro plan") {
		t.Fatalf("basic batch: %q", h.tg.sent(70))
	}

	if _, err := h.b.store.Grant(ctx, 70, "pro", 30, "test", h.clock); err != nil {
		t.Fatal(err)
	}
	h.tg.reset()
	doc()
	h.b.wg.Wait()
	if !contains(h.tg.sent(70), "Screening 2 addresses") || !contains(h.tg.sent(70), "Batch done: 2 screened, 0 failed") {
		t.Fatalf("batch messages: %q", h.tg.sent(70))
	}
	if d, ok := h.tg.last("sendDocument"); !ok || !strings.HasSuffix(fmt.Sprint(d.Params["document"]), ".csv") {
		t.Errorf("no CSV sent: %+v", d)
	}
	if used, _ := h.b.store.Used(ctx, 70, h.clock); used != 2 {
		t.Errorf("used = %d, want 2", used)
	}
}

// --- API keys and the public API ---------------------------------------------

func TestPublicAPI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := newApp(t, h)
	auth := a.tma(80, "en")

	// Keys need the business plan.
	code, body, _ := a.do("POST", "/app/api/keys", auth, map[string]string{"name": "prod"})
	if code != http.StatusForbidden || body["error"] != "feature_locked" {
		t.Fatalf("key on trial: %d %v", code, body)
	}
	if _, err := h.b.store.Grant(ctx, 80, "business", 30, "test", h.clock); err != nil {
		t.Fatal(err)
	}
	code, body, _ = a.do("POST", "/app/api/keys", auth, map[string]string{"name": "prod"})
	if code != 200 {
		t.Fatalf("create key: %d %v", code, body)
	}
	key := body["key"].(string)
	bearer := "Bearer " + key

	code, _, raw := a.do("POST", "/api/v1/screen", bearer, map[string]string{"address": addrA})
	if code != 200 || !strings.Contains(string(raw), `"band":"low"`) {
		t.Fatalf("public screen: %d %s", code, raw)
	}
	code, body, _ = a.do("GET", "/api/v1/usage", bearer, nil)
	if code != 200 || body["plan"] != "Business" || body["screens_today"].(float64) != 1 {
		t.Errorf("usage: %d %v", code, body)
	}

	// The clock is frozen, so the bucket does not refill: 5 a second at most.
	var limited bool
	for i := 0; i < 6; i++ {
		if code, _, _ := a.do("GET", "/api/v1/usage", bearer, nil); code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("rate limit not applied")
	}

	// A revoked key stops working at once.
	_, list, _ := a.do("GET", "/app/api/keys", auth, nil)
	id := list["items"].([]any)[0].(map[string]any)["id"].(float64)
	a.do("DELETE", fmt.Sprintf("/app/api/keys/%d", int(id)), auth, nil)
	h.clock = h.clock.Add(time.Minute)
	if code, _, _ := a.do("GET", "/api/v1/usage", bearer, nil); code != http.StatusUnauthorized {
		t.Errorf("revoked key: %d", code)
	}
}

// --- chains ------------------------------------------------------------------

func TestParseTarget(t *testing.T) {
	h := newHarness(t)
	h.b.chains = []chainInfo{{ID: "tron", Enabled: true}, {ID: "ethereum", Enabled: true}, {ID: "bsc", Enabled: true}}
	evm := "0xAbC" + strings.Repeat("0", 37)
	for _, tc := range []struct {
		text, explicit, chain, addr, err string
	}{
		{addrA, "", "tron", addrA, ""},
		{"  " + addrA + "  ", "", "tron", addrA, ""},
		{evm, "", "ethereum", strings.ToLower(evm), ""},
		{"bsc " + evm, "", "bsc", strings.ToLower(evm), ""},
		{evm, "bsc", "bsc", strings.ToLower(evm), ""},
		{"eth " + addrA, "", "", "", "bad_address"},
		{"hello", "", "", "", "bad_address"},
		{addrA[:30], "", "", "", "bad_address"},
	} {
		chain, addr, err := h.b.parseTarget(tc.text, tc.explicit)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.err || (tc.err == "" && (chain != tc.chain || addr != tc.addr)) {
			t.Errorf("parseTarget(%q, %q) = %q %q %v", tc.text, tc.explicit, chain, addr, err)
		}
	}
}

// --- language ------------------------------------------------------------------

func TestTurkishUserIsAnsweredInTurkish(t *testing.T) {
	h := newHarness(t)
	h.b.dispatch(context.Background(), update{Message: &message{
		From: &tgUser{ID: 90, FirstName: "Ayşe", LanguageCode: "tr"},
		Chat: tgChat{ID: 90, Type: "private"}, Text: addrA}})
	msgs := h.tg.sent(90)
	if !contains(msgs, "günlük ücretsiz denemeniz başladı") || !contains(msgs, "Adresin bağlantıları") {
		t.Fatalf("messages: %q", msgs)
	}

	// /language overrides the client.
	h.b.dispatch(context.Background(), update{CallbackQuery: &callbackQuery{ID: "c", From: tgUser{ID: 90, LanguageCode: "tr"},
		Message: &message{Chat: tgChat{ID: 90, Type: "private"}}, Data: "lang:en"}})
	h.tg.reset()
	h.b.dispatch(context.Background(), update{Message: &message{
		From: &tgUser{ID: 90, LanguageCode: "tr"}, Chat: tgChat{ID: 90, Type: "private"}, Text: "/status"}})
	if !contains(h.tg.sent(90), "Active until") {
		t.Errorf("after choosing English: %q", h.tg.sent(90))
	}
}

var botVerb = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*\d*(\.\d+)?[a-zA-Z%]`)

func verbKinds(s string) string {
	var out []string
	for _, m := range botVerb.FindAllString(s, -1) {
		if m != "%%" {
			out = append(out, m[len(m)-1:])
		}
	}
	sort.Strings(out)
	return strings.Join(out, "")
}

func TestMessageCatalogueIsComplete(t *testing.T) {
	for key, m := range messages {
		for i, name := range []string{"English", "Turkish", "Russian"} {
			if m[i] == "" {
				t.Errorf("%q has no %s", key, name)
			} else if verbKinds(m[0]) != verbKinds(m[i]) {
				t.Errorf("%q: English verbs %q, %s %q", key, verbKinds(m[0]), name, verbKinds(m[i]))
			}
		}
	}
	for _, lang := range []string{langTR, langRU} {
		if len(publicCommands[lang]) != len(publicCommands[langEN]) {
			t.Errorf("%s command menu has %d entries, English %d", lang, len(publicCommands[lang]), len(publicCommands[langEN]))
		}
	}
}

func TestNormLang(t *testing.T) {
	for _, c := range []struct{ chosen, client, want string }{
		{"", "ru", langRU}, {"", "uk", langRU}, {"", "be", langRU}, {"", "kk", langRU}, {"", "uz", langRU},
		{"", "ky", langRU}, {"", "tg", langRU}, {"", "ru-RU", langRU}, {"", "tr", langTR}, {"", "en-GB", langEN},
		{"", "de", langEN}, {"", "", langEN}, {"en", "ru", langEN}, {"ru", "tr", langRU},
	} {
		if got := normLang(c.chosen, c.client); got != c.want {
			t.Errorf("normLang(%q, %q) = %q, want %q", c.chosen, c.client, got, c.want)
		}
	}
}

func TestRussianUserIsAnsweredInRussian(t *testing.T) {
	h := newHarness(t)
	h.b.dispatch(context.Background(), update{Message: &message{
		From: &tgUser{ID: 91, FirstName: "Олег", LanguageCode: "kk"},
		Chat: tgChat{ID: 91, Type: "private"}, Text: addrA}})
	msgs := h.tg.sent(91)
	if !contains(msgs, "Бесплатный пробный период начался") || !contains(msgs, "Связи адреса") {
		t.Fatalf("messages: %q", msgs)
	}
	h.tg.reset()
	h.b.dispatch(context.Background(), update{Message: &message{
		From: &tgUser{ID: 91, LanguageCode: "kk"}, Chat: tgChat{ID: 91, Type: "private"}, Text: "/terms"}})
	if !contains(h.tg.sent(91), "УСЛОВИЯ") {
		t.Errorf("terms: %q", h.tg.sent(91))
	}
}

// --- follow-up ---------------------------------------------------------------

func depthOf(pending, traced, of int) map[string]any {
	return map[string]any{"frontier_pending": pending, "frontier_queued": pending, "traced": traced, "counterparties": of}
}

func TestFollowUpDeliversTheFinalResult(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mu.Lock()
	h.seq = []map[string]any{
		{"score": 0.7, "band": "low", "coverage": 0.022, "depth": depthOf(17, 0, 56)},   // the customer's screen
		{"score": 2.8, "band": "low", "coverage": 0.108, "depth": depthOf(183, 56, 56)}, // round 1
		{"score": 9.2, "band": "low", "coverage": 0.387, "depth": depthOf(159, 56, 56)}, // round 2
		{"score": 12.4, "band": "low", "coverage": 0.71, "depth": depthOf(0, 56, 56)},   // round 3: done
	}
	h.mu.Unlock()

	h.text(100, addrA)
	if !contains(h.tg.sent(100), "I will send the final result here") {
		t.Fatalf("first answer does not promise a follow-up: %q", h.tg.sent(100))
	}
	h.b.wg.Wait()

	msgs := h.tg.sent(100)
	last := msgs[len(msgs)-1]
	for _, want := range []string{"Final result for " + addrA, "First answer: Low 0.7/100, coverage 2.2%", "Now: Low 12.4/100, coverage 71.0%"} {
		if !strings.Contains(last, want) {
			t.Errorf("final message lacks %q:\n%s", want, last)
		}
	}
	if h.calls != 4 {
		t.Errorf("%d screens served, want 1 + 3 rounds", h.calls)
	}
	if used, _ := h.b.store.Used(ctx, 100, h.clock); used != 1 {
		t.Errorf("follow-up rounds used daily screens: used = %d, want 1", used)
	}
	items, _ := h.b.store.History(ctx, 100, 10)
	if len(items) != 2 || items[0].Channel != "followup" {
		t.Errorf("history = %+v, want the screen and the follow-up", items)
	}
}

func TestFollowUpStopsWhenCoverageStalls(t *testing.T) {
	h := newHarness(t)
	stuck := map[string]any{"score": 5.0, "band": "low", "coverage": 0.30, "depth": depthOf(40, 56, 56)}
	h.setResult(stuck)
	h.text(101, addrA)
	h.b.wg.Wait()
	// The first screen, then two rounds that add nothing.
	if h.calls != 3 {
		t.Errorf("%d screens, want 3: a stalled trace must stop", h.calls)
	}
	msgs := h.tg.sent(101)
	if !strings.Contains(msgs[len(msgs)-1], "The result did not change") {
		t.Errorf("last message: %q", msgs[len(msgs)-1])
	}
}

func TestFinishedScreenHasNoFollowUp(t *testing.T) {
	h := newHarness(t)
	h.setResult(map[string]any{"score": 5.0, "band": "low", "coverage": 0.9, "depth": depthOf(0, 10, 10)})
	h.text(102, addrA)
	h.b.wg.Wait()
	if h.calls != 1 || contains(h.tg.sent(102), "final result") {
		t.Errorf("follow-up on a finished screen: %d calls, %q", h.calls, h.tg.sent(102))
	}
}
