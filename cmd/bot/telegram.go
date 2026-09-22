package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// telegram is a minimal Bot API client: the methods this bot uses, nothing
// else. base is https://api.telegram.org in production and a test server in
// tests.
type telegram struct {
	base  string
	token string
	http  *http.Client
}

func newTelegram(base, token string) *telegram {
	// Long polls hold the connection for up to 30 seconds; the timeout must
	// outlast that.
	return &telegram{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 90 * time.Second}}
}

// apiError is a Bot API "ok": false response.
type apiError struct {
	Method      string
	Code        int
	Description string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// call posts params as JSON and decodes the result into out.
func (t *telegram) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url(method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return t.do(req, method, out)
}

func (t *telegram) url(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", t.base, t.token, method)
}

func (t *telegram) do(req *http.Request, method string, out any) error {
	resp, err := t.http.Do(req)
	if err != nil {
		// The URL holds the token; never let it reach a log line.
		return fmt.Errorf("telegram %s: %s", method, strings.ReplaceAll(err.Error(), t.token, "<token>"))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("telegram %s: undecodable response (%d)", method, resp.StatusCode)
	}
	if !env.OK {
		return &apiError{Method: method, Code: env.ErrorCode, Description: env.Description}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// --- types -----------------------------------------------------------------

type tgUser struct {
	ID           int64  `json:"id"`
	IsBot        bool   `json:"is_bot"`
	Username     string `json:"username"`
	FirstName    string `json:"first_name"`
	LanguageCode string `json:"language_code"`
}

type document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type successfulPayment struct {
	Currency                   string `json:"currency"`
	TotalAmount                int64  `json:"total_amount"`
	InvoicePayload             string `json:"invoice_payload"`
	SubscriptionExpirationDate int64  `json:"subscription_expiration_date"`
	IsRecurring                bool   `json:"is_recurring"`
	IsFirstRecurring           bool   `json:"is_first_recurring"`
	TelegramPaymentChargeID    string `json:"telegram_payment_charge_id"`
}

type refundedPayment struct {
	Currency                string `json:"currency"`
	TotalAmount             int64  `json:"total_amount"`
	InvoicePayload          string `json:"invoice_payload"`
	TelegramPaymentChargeID string `json:"telegram_payment_charge_id"`
}

type message struct {
	MessageID         int64              `json:"message_id"`
	From              *tgUser            `json:"from"`
	Chat              tgChat             `json:"chat"`
	Text              string             `json:"text"`
	Document          *document          `json:"document"`
	SuccessfulPayment *successfulPayment `json:"successful_payment"`
	RefundedPayment   *refundedPayment   `json:"refunded_payment"`
}

type callbackQuery struct {
	ID      string   `json:"id"`
	From    tgUser   `json:"from"`
	Message *message `json:"message"`
	Data    string   `json:"data"`
}

type preCheckoutQuery struct {
	ID             string `json:"id"`
	From           tgUser `json:"from"`
	Currency       string `json:"currency"`
	TotalAmount    int64  `json:"total_amount"`
	InvoicePayload string `json:"invoice_payload"`
}

type update struct {
	UpdateID         int64             `json:"update_id"`
	Message          *message          `json:"message"`
	CallbackQuery    *callbackQuery    `json:"callback_query"`
	PreCheckoutQuery *preCheckoutQuery `json:"pre_checkout_query"`
}

type button struct {
	Text         string  `json:"text"`
	URL          string  `json:"url,omitempty"`
	CallbackData string  `json:"callback_data,omitempty"`
	WebApp       *webApp `json:"web_app,omitempty"`
}

type webApp struct {
	URL string `json:"url"`
}

type keyboard struct {
	InlineKeyboard [][]button `json:"inline_keyboard"`
}

type labeledPrice struct {
	Label  string `json:"label"`
	Amount int64  `json:"amount"`
}

// --- methods ---------------------------------------------------------------

func (t *telegram) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	var out []update
	err := t.call(ctx, "getUpdates", map[string]any{
		"offset":  offset,
		"timeout": 30,
		// Stated explicitly so payments keep arriving even if Telegram's
		// default ever changes.
		"allowed_updates": []string{"message", "callback_query", "pre_checkout_query"},
	}, &out)
	return out, err
}

// maxMessage is Telegram's limit on one message's text.
const maxMessage = 4096

func (t *telegram) sendMessage(ctx context.Context, chatID int64, text string, kb *keyboard) error {
	chunks := splitMessage(text, maxMessage-96)
	for i, c := range chunks {
		params := map[string]any{"chat_id": chatID, "text": c, "link_preview_options": map[string]bool{"is_disabled": true}}
		if kb != nil && i == len(chunks)-1 {
			params["reply_markup"] = kb
		}
		if err := t.call(ctx, "sendMessage", params, nil); err != nil {
			return err
		}
	}
	return nil
}

// splitMessage cuts text into pieces under limit, at line breaks where it can.
func splitMessage(text string, limit int) []string {
	var out []string
	for len(text) > limit {
		cut := strings.LastIndex(text[:limit], "\n")
		if cut <= 0 {
			cut = limit
		}
		out = append(out, text[:cut])
		text = strings.TrimLeft(text[cut:], "\n")
	}
	return append(out, text)
}

func (t *telegram) answerCallbackQuery(ctx context.Context, id, text string) error {
	return t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

func (t *telegram) answerPreCheckoutQuery(ctx context.Context, id string, ok bool, errMsg string) error {
	params := map[string]any{"pre_checkout_query_id": id, "ok": ok}
	if !ok {
		params["error_message"] = errMsg
	}
	return t.call(ctx, "answerPreCheckoutQuery", params, nil)
}

// createSubscriptionLink creates a Stars invoice link that renews every 30
// days. The Bot API accepts 2592000 as the only subscription period.
func (t *telegram) createSubscriptionLink(ctx context.Context, title, description, payload string, stars int64) (string, error) {
	var link string
	err := t.call(ctx, "createInvoiceLink", map[string]any{
		"title":               title,
		"description":         description,
		"payload":             payload,
		"provider_token":      "",
		"currency":            "XTR",
		"prices":              []labeledPrice{{Label: title, Amount: stars}},
		"subscription_period": 2592000,
	}, &link)
	return link, err
}

func (t *telegram) editUserStarSubscription(ctx context.Context, userID int64, chargeID string, canceled bool) error {
	return t.call(ctx, "editUserStarSubscription", map[string]any{
		"user_id": userID, "telegram_payment_charge_id": chargeID, "is_canceled": canceled,
	}, nil)
}

func (t *telegram) refundStarPayment(ctx context.Context, userID int64, chargeID string) error {
	return t.call(ctx, "refundStarPayment", map[string]any{
		"user_id": userID, "telegram_payment_charge_id": chargeID,
	}, nil)
}

type botCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// setMyCommands registers the command menu; lang "" is the default for
// every language without its own list.
func (t *telegram) setMyCommands(ctx context.Context, cmds []botCommand, lang string) error {
	params := map[string]any{"commands": cmds}
	if lang != "" {
		params["language_code"] = lang
	}
	return t.call(ctx, "setMyCommands", params, nil)
}

// setMenuButton makes the button beside the input field open the Mini App.
func (t *telegram) setMenuButton(ctx context.Context, text, appURL string) error {
	return t.call(ctx, "setChatMenuButton", map[string]any{
		"menu_button": map[string]any{"type": "web_app", "text": text, "web_app": webApp{URL: appURL}},
	}, nil)
}

// maxDownload bounds files the bot downloads: batch lists, never media.
const maxDownload = 1 << 20

// download fetches a file a user sent, up to maxDownload bytes.
func (t *telegram) download(ctx context.Context, fileID string) ([]byte, error) {
	var f struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := t.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		return nil, err
	}
	if f.FileSize > maxDownload {
		return nil, fmt.Errorf("file too large")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/file/bot%s/%s", t.base, t.token, f.FilePath), nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram download: %s", strings.ReplaceAll(err.Error(), t.token, "<token>"))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram download: %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownload {
		return nil, fmt.Errorf("file too large")
	}
	return data, nil
}

// sendDocument uploads a file as multipart form data.
func (t *telegram) sendDocument(ctx context.Context, chatID int64, filename string, data []byte, caption string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", fmt.Sprint(chatID))
	if caption != "" {
		_ = w.WriteField("caption", caption)
	}
	part, err := w.CreateFormFile("document", filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url("sendDocument"), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return t.do(req, "sendDocument", nil)
}
