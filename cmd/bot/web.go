package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mozer/tether-risk/internal/billing"
)

// The HTTP side of the bot: the Mini App and the public API
// (docs/APP_API.md, docs/DECISIONS.md D27). Both authenticate a customer and
// then go through the same gate as the chat.

//go:embed web
var webFS embed.FS

// initDataMaxAge bounds how old a Mini App session may be. Telegram signs
// initData once when the app opens; a stolen copy stops working after this.
const initDataMaxAge = 24 * time.Hour

func (b *bot) routes() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(webFS, "web")
	files := http.StripPrefix("/app/", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusMovedPermanently)
	})
	mux.Handle("GET /app/", files)

	app := func(h func(http.ResponseWriter, *http.Request, appUser)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u, err := b.appAuth(r)
			if err != nil {
				writeErr(w, &gateError{Code: "unauthorized", Status: http.StatusUnauthorized}, langEN)
				return
			}
			h(w, r, u)
		}
	}
	mux.HandleFunc("GET /app/api/me", app(b.apiMe))
	mux.HandleFunc("POST /app/api/screen", app(b.apiAppScreen))
	mux.HandleFunc("POST /app/api/report", app(b.apiAppReport))
	mux.HandleFunc("GET /app/api/history", app(b.apiHistory))
	mux.HandleFunc("GET /app/api/watches", app(b.apiWatches))
	mux.HandleFunc("POST /app/api/watches", app(b.apiAddWatch))
	mux.HandleFunc("DELETE /app/api/watches/{id}", app(b.apiRemoveWatch))
	mux.HandleFunc("POST /app/api/invoice/stars", app(b.apiStarsInvoice))
	mux.HandleFunc("POST /app/api/invoice/usdt", app(b.apiUSDTInvoice))
	mux.HandleFunc("POST /app/api/batch", app(b.apiBatch))
	mux.HandleFunc("GET /app/api/keys", app(b.apiKeys))
	mux.HandleFunc("POST /app/api/keys", app(b.apiCreateKey))
	mux.HandleFunc("DELETE /app/api/keys/{id}", app(b.apiRevokeKey))
	mux.HandleFunc("POST /app/api/lang", app(b.apiLang))

	key := func(h func(http.ResponseWriter, *http.Request, appUser)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u, err := b.keyAuth(r)
			if err != nil {
				var ge *gateError
				if !errors.As(err, &ge) {
					ge = &gateError{Code: "unauthorized", Status: http.StatusUnauthorized}
				}
				writeErr(w, ge, langEN)
				return
			}
			h(w, r, u)
		}
	}
	mux.HandleFunc("POST /api/v1/screen", key(b.apiPublicScreen))
	mux.HandleFunc("POST /api/v1/report", key(b.apiPublicReport))
	mux.HandleFunc("GET /api/v1/usage", key(b.apiUsage))
	mux.HandleFunc("GET /api/v1/watches", key(b.apiWatches))
	mux.HandleFunc("POST /api/v1/watches", key(b.apiAddWatch))
	mux.HandleFunc("DELETE /api/v1/watches/{id}", key(b.apiRemoveWatch))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return securityHeaders(mux)
}

// securityHeaders applies to every response. The Mini App loads only its own
// files and Telegram's script, and may be framed only by Telegram Web.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' https://telegram.org; "+
			"style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; "+
			"frame-ancestors https://web.telegram.org https://*.telegram.org")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/app/api/") || strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// appUser is an authenticated customer on either surface.
type appUser struct {
	ID        int64
	FirstName string
	Username  string
	Lang      string // resolved: en, tr or ru
	Channel   string // app or api
}

// appAuth verifies Telegram Mini App initData (Authorization: tma <data>).
// https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app
func (b *bot) appAuth(r *http.Request) (appUser, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "tma ")
	if !ok || raw == "" {
		return appUser{}, errors.New("no init data")
	}
	vals, err := verifyInitData(raw, b.tg.token, b.now(), initDataMaxAge)
	if err != nil {
		return appUser{}, err
	}
	var tu tgUser
	if err := json.Unmarshal([]byte(vals.Get("user")), &tu); err != nil || tu.ID == 0 {
		return appUser{}, errors.New("init data has no user")
	}
	u, err := b.store.Touch(r.Context(), billing.User{ID: tu.ID, Username: tu.Username,
		FirstName: tu.FirstName, ClientLang: tu.LanguageCode})
	if err != nil {
		return appUser{}, err
	}
	// Opening the app is a first contact like any other: it starts the trial.
	if billing.TrialDue(b.billing, u.TrialStartedAt) && !b.isAdmin(tu.ID) {
		if _, err := b.store.StartTrial(r.Context(), tu.ID, b.now()); err != nil {
			b.log.Error("start trial", "user", tu.ID, "error", err)
		}
	}
	return appUser{ID: tu.ID, FirstName: tu.FirstName, Username: tu.Username,
		Lang: normLang(u.Lang, tu.LanguageCode), Channel: chanApp}, nil
}

// verifyInitData checks initData's HMAC and age and returns its fields.
func verifyInitData(raw, token string, now time.Time, maxAge time.Duration) (url.Values, error) {
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return nil, err
	}
	got := vals.Get("hash")
	if got == "" {
		return nil, errors.New("no hash")
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		if k != "hash" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = k + "=" + vals.Get(k)
	}
	secret := hmacSHA256([]byte("WebAppData"), []byte(token))
	want := hex.EncodeToString(hmacSHA256(secret, []byte(strings.Join(lines, "\n"))))
	if !hmac.Equal([]byte(got), []byte(want)) {
		return nil, errors.New("bad hash")
	}
	authDate, err := strconv.ParseInt(vals.Get("auth_date"), 10, 64)
	if err != nil || now.Sub(time.Unix(authDate, 0)) > maxAge {
		return nil, errors.New("init data expired")
	}
	return vals, nil
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// keyAuth verifies a public API key (Authorization: Bearer trk_…), checks
// the plan includes the API, and applies the per-key rate limit.
func (b *bot) keyAuth(r *http.Request) (appUser, error) {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || key == "" {
		return appUser{}, errors.New("no key")
	}
	uid, keyID, err := b.store.KeyOwner(r.Context(), strings.TrimSpace(key), b.now())
	if err != nil {
		return appUser{}, err
	}
	if !b.limiter.allow(keyID, b.now()) {
		return appUser{}, &gateError{Code: "rate_limited", Status: http.StatusTooManyRequests}
	}
	acc, err := b.access(r.Context(), uid)
	if err != nil {
		return appUser{}, err
	}
	if !acc.API {
		return appUser{}, &gateError{Code: "feature_locked", Status: http.StatusForbidden, Feature: "api"}
	}
	return appUser{ID: uid, Lang: langEN, Channel: chanAPI}, nil
}

// keyLimiter allows a burst of keyRate requests per second per key.
type keyLimiter struct {
	mu      sync.Mutex
	buckets map[int64]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

const keyRate = 5.0

func (l *keyLimiter) allow(key int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[int64]*bucket{}
	}
	bk, ok := l.buckets[key]
	if !ok {
		bk = &bucket{tokens: keyRate, last: now}
		l.buckets[key] = bk
	}
	bk.tokens = min(keyRate, bk.tokens+now.Sub(bk.last).Seconds()*keyRate)
	bk.last = now
	if bk.tokens < 1 {
		return false
	}
	bk.tokens--
	return true
}

// --- responses ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr renders a refusal as {error, message}. Anything that is not a
// gate refusal is our fault and says so without detail.
func writeErr(w http.ResponseWriter, err error, lang string) {
	var ge *gateError
	if !errors.As(err, &ge) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": t(lang, "error_ours")})
		return
	}
	msg := ""
	switch ge.Code {
	case "bad_address":
		msg = t(lang, "bad_address")
	case "chain_unavailable":
		msg = t(lang, "chain_unavailable", ge.Chain)
	case "no_plan":
		msg = strings.SplitN(t(lang, "no_plan"), "\n", 2)[0]
	case "feature_locked":
		msg = t(lang, "feature_locked", ge.Feature, "")
	case "busy":
		msg = t(lang, "busy")
	case "limit_reached":
		msg = strings.SplitN(t(lang, "limit_reached", ge.Limit), "\n", 2)[0]
	case "screen_failed":
		msg = t(lang, "screen_failed", ge.Cause)
	case "unauthorized":
		msg = "Missing or invalid credentials."
	case "rate_limited":
		msg = "Too many requests; at most 5 per second per key."
	case "not_found":
		msg = "Not found."
	case "bad_request":
		msg = "Invalid request."
	}
	status := ge.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": ge.Code, "message": msg})
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 256<<10)).Decode(v); err != nil {
		return &gateError{Code: "bad_request", Status: http.StatusBadRequest}
	}
	return nil
}

// --- Mini App endpoints ------------------------------------------------------

type limitsJSON struct {
	DailyScreens int  `json:"daily_screens"`
	Details      bool `json:"details"`
	PDF          bool `json:"pdf"`
	Watches      int  `json:"watches"`
	Batch        int  `json:"batch"`
	API          bool `json:"api"`
}

func (b *bot) apiMe(w http.ResponseWriter, r *http.Request, u appUser) {
	ctx := r.Context()
	now := b.now()
	acc, err := b.access(ctx, u.ID)
	if err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	used, _ := b.store.Used(ctx, u.ID, now)
	ws, _ := b.store.Watches(ctx, u.ID)

	var accessJSON any
	if acc.Plan != nil {
		accessJSON = map[string]any{
			"plan":   map[string]string{"id": acc.Plan.ID, "name": acc.Plan.Name},
			"until":  acc.Until.UTC(),
			"source": acc.Source,
			"renews": acc.Renews,
		}
	} else if acc.Admin {
		accessJSON = map[string]any{"plan": map[string]string{"id": "admin", "name": "Admin"}, "source": "grant", "renews": false}
	}
	plans := make([]map[string]any, 0, len(b.billing.Plans))
	for _, p := range b.billing.Plans {
		plans = append(plans, map[string]any{
			"id": p.ID, "name": p.Name, "daily_screens": p.DailyScreens, "details": p.Details, "pdf": p.PDF,
			"watches": p.Watches, "batch": p.Batch, "api": p.API,
			"price_stars": p.PriceStars, "price_usdt": billing.FormatUSDT(p.PriceMicroUSDT),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{"id": u.ID, "first_name": u.FirstName, "username": u.Username,
			"lang": u.Lang, "admin": acc.Admin},
		"access": accessJSON,
		"limits": limitsJSON{DailyScreens: acc.DailyScreens, Details: acc.Details, PDF: acc.PDF,
			Watches: acc.Watches, Batch: acc.Batch, API: acc.API},
		"usage":        map[string]any{"screens_today": used, "watches": len(ws), "resets_at": resetsAt(now).UTC()},
		"plans":        plans,
		"usdt_enabled": b.usdtAddr != "",
		"chains":       b.chains,
		"support":      b.support,
	})
}

type targetJSON struct {
	Address string `json:"address"`
	Chain   string `json:"chain"`
	Label   string `json:"label"`
}

func (b *bot) apiAppScreen(w http.ResponseWriter, r *http.Request, u appUser) {
	var req targetJSON
	if err := decode(r, &req); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	out, err := b.gate(r.Context(), screenRequest{UserID: u.ID, Text: req.Address, Chain: req.Chain,
		Kind: kindSummary, Channel: u.Channel})
	if err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	following := b.startFollowUp(u.ID, u.Lang, out.Chain, out.Address, out.Result)
	writeJSON(w, http.StatusOK, map[string]any{
		"result":    json.RawMessage(out.Raw),
		"usage":     map[string]int{"screens_today": out.Used, "daily_screens": out.Limit},
		"follow_up": following,
	})
}

// apiAppReport sends the PDF to the user's Telegram chat: a Mini App cannot
// reliably save a file.
func (b *bot) apiAppReport(w http.ResponseWriter, r *http.Request, u appUser) {
	var req targetJSON
	if err := decode(r, &req); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	out, err := b.gate(r.Context(), screenRequest{UserID: u.ID, Text: req.Address, Chain: req.Chain,
		Kind: kindPDF, Channel: u.Channel})
	if err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	if err := b.tg.sendDocument(r.Context(), u.ID, out.Chain+"-"+out.Address+".pdf", out.PDF,
		t(u.Lang, "pdf_caption", out.Address)); err != nil {
		b.log.Error("send app report", "user", u.ID, "error", err)
		writeErr(w, err, u.Lang)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

func (b *bot) apiHistory(w http.ResponseWriter, r *http.Request, u appUser) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := b.store.History(r.Context(), u.ID, limit)
	if err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (b *bot) apiWatches(w http.ResponseWriter, r *http.Request, u appUser) {
	ws, err := b.store.Watches(r.Context(), u.ID)
	if err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	acc, _ := b.access(r.Context(), u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"items": ws, "limit": acc.Watches})
}

func (b *bot) apiAddWatch(w http.ResponseWriter, r *http.Request, u appUser) {
	var req targetJSON
	if err := decode(r, &req); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	wt, _, _, err := b.addWatch(r.Context(), u.ID, req.Address, req.Chain, strings.TrimSpace(req.Label))
	if err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	writeJSON(w, http.StatusOK, wt)
}

func (b *bot) apiRemoveWatch(w http.ResponseWriter, r *http.Request, u appUser) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := b.store.RemoveWatch(r.Context(), u.ID, id, b.now()); err != nil {
		if errors.Is(err, billing.ErrNotFound) {
			err = refuse("not_found", http.StatusNotFound)
		}
		writeErr(w, err, u.Lang)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
}

type planJSON struct {
	Plan string `json:"plan"`
}

func (b *bot) apiStarsInvoice(w http.ResponseWriter, r *http.Request, u appUser) {
	var req planJSON
	if err := decode(r, &req); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	p, ok := b.billing.Plan(req.Plan)
	if !ok {
		writeErr(w, refuse("not_found", http.StatusNotFound), u.Lang)
		return
	}
	link, err := b.starsLink(r.Context(), p, u.Lang)
	if err != nil {
		b.log.Error("stars link", "error", err)
		writeErr(w, err, u.Lang)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"link": link})
}

func (b *bot) apiUSDTInvoice(w http.ResponseWriter, r *http.Request, u appUser) {
	var req planJSON
	if err := decode(r, &req); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	inv, _, err := b.usdtInvoice(r.Context(), u.ID, req.Plan)
	if err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"amount": billing.FormatUSDT(inv.Amount), "address": inv.Address,
		"network": "TRON (TRC-20)", "expires_at": inv.ExpiresAt.UTC(),
	})
}

func (b *bot) apiBatch(w http.ResponseWriter, r *http.Request, u appUser) {
	var req struct {
		Addresses []string `json:"addresses"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	targets, _ := b.parseBatch(strings.Join(req.Addresses, "\n"))
	if len(targets) == 0 {
		writeErr(w, refuse("bad_address", http.StatusBadRequest), u.Lang)
		return
	}
	if err := b.checkBatch(r.Context(), u.ID, len(targets)); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	if !b.startBatch(u.ID, u.ID, u.Lang, targets) {
		writeErr(w, refuse("busy", http.StatusConflict), u.Lang)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"accepted": len(targets)})
}

func (b *bot) apiKeys(w http.ResponseWriter, r *http.Request, u appUser) {
	if err := b.requireAPI(r.Context(), u.ID); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	keys, err := b.store.Keys(r.Context(), u.ID)
	if err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": keys})
}

func (b *bot) apiCreateKey(w http.ResponseWriter, r *http.Request, u appUser) {
	if err := b.requireAPI(r.Context(), u.ID); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	key, k, err := b.store.CreateKey(r.Context(), u.ID, req.Name, b.now())
	if errors.Is(err, billing.ErrKeyLimit) {
		err = &gateError{Code: "limit_reached", Status: http.StatusTooManyRequests, Limit: 10}
	}
	if err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": k.ID, "key": key, "prefix": k.Prefix})
}

func (b *bot) apiRevokeKey(w http.ResponseWriter, r *http.Request, u appUser) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := b.store.RevokeKey(r.Context(), u.ID, id, b.now()); err != nil {
		if errors.Is(err, billing.ErrNotFound) {
			err = refuse("not_found", http.StatusNotFound)
		}
		writeErr(w, err, u.Lang)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (b *bot) requireAPI(ctx context.Context, userID int64) error {
	acc, err := b.access(ctx, userID)
	if err != nil {
		return err
	}
	if !acc.API {
		return &gateError{Code: "feature_locked", Status: http.StatusForbidden, Feature: "api"}
	}
	return nil
}

func (b *bot) apiLang(w http.ResponseWriter, r *http.Request, u appUser) {
	var req struct {
		Lang string `json:"lang"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	if req.Lang != langEN && req.Lang != langTR && req.Lang != langRU {
		writeErr(w, refuse("bad_request", http.StatusBadRequest), u.Lang)
		return
	}
	if err := b.store.SetLang(r.Context(), u.ID, req.Lang); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"lang": req.Lang})
}

// --- public API endpoints ----------------------------------------------------

func (b *bot) apiPublicScreen(w http.ResponseWriter, r *http.Request, u appUser) {
	var req targetJSON
	if err := decode(r, &req); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	out, err := b.gate(r.Context(), screenRequest{UserID: u.ID, Text: req.Address, Chain: req.Chain,
		Kind: kindSummary, Channel: chanAPI})
	if err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	w.Header().Set("X-Screens-Used", strconv.Itoa(out.Used))
	w.Header().Set("X-Screens-Limit", strconv.Itoa(out.Limit))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Raw)
}

func (b *bot) apiPublicReport(w http.ResponseWriter, r *http.Request, u appUser) {
	var req targetJSON
	if err := decode(r, &req); err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	out, err := b.gate(r.Context(), screenRequest{UserID: u.ID, Text: req.Address, Chain: req.Chain,
		Kind: kindPDF, Channel: chanAPI})
	if err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.pdf"`, out.Chain, out.Address))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.PDF)
}

func (b *bot) apiUsage(w http.ResponseWriter, r *http.Request, u appUser) {
	now := b.now()
	acc, err := b.access(r.Context(), u.ID)
	if err != nil {
		writeErr(w, err, u.Lang)
		return
	}
	used, _ := b.store.Used(r.Context(), u.ID, now)
	body := map[string]any{"plan": acc.planName(), "screens_today": used, "daily_screens": acc.DailyScreens,
		"resets_at": resetsAt(now).UTC()}
	if acc.Plan != nil {
		body["until"] = acc.Until.UTC()
	}
	writeJSON(w, http.StatusOK, body)
}
