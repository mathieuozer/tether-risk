package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mozer/tether-risk/internal/billing"
)

// The gate is the one path from a customer to a screen, for the chat, the
// Mini App, the public API and batch files alike (docs/DECISIONS.md D27): it
// checks the address, the plan, the feature and the daily limit, runs the
// screen, refunds it if it fails, and records it in history. A channel that
// bypassed it would bypass billing.

type screenKind int

const (
	kindSummary screenKind = iota
	kindDetails
	kindPDF
	kindPreSend // the recipient of a payment, checked against the payer (D34)
)

// Channels, as recorded in screen_history.
const (
	chanBot    = "bot"
	chanApp    = "app"
	chanAPI    = "api"
	chanBatch  = "batch"
	chanInline = "inline" // an inline result sent into another chat (D34)
)

// gateError is a refusal every channel can render: the chat in words, the
// app and API as {error, message} with an HTTP status.
type gateError struct {
	Code   string // see docs/APP_API.md
	Status int
	// Detail carries what the message needs: the limit, the feature, the
	// chain, the upstream error.
	Limit   int
	Feature string
	Chain   string
	Cause   error
}

func (e *gateError) Error() string {
	if e.Cause != nil {
		return e.Code + ": " + e.Cause.Error()
	}
	return e.Code
}

func refuse(code string, status int) *gateError { return &gateError{Code: code, Status: status} }

// access is a user's effective access: their plan's limits, or unlimited for
// an admin. A limit of -1 means unlimited.
type access struct {
	billing.Access
	Admin        bool
	DailyScreens int
	Details, PDF bool
	Watches      int
	Batch        int
	API          bool
}

func (b *bot) access(ctx context.Context, userID int64) (access, error) {
	if b.isAdmin(userID) {
		return access{Admin: true, DailyScreens: -1, Details: true, PDF: true, Watches: -1, Batch: -1, API: true}, nil
	}
	acc, err := b.store.Access(ctx, userID, b.now())
	if err != nil {
		return access{}, err
	}
	out := access{Access: acc}
	if p := acc.Plan; p != nil {
		out.DailyScreens, out.Details, out.PDF = p.DailyScreens, p.Details, p.PDF
		out.Watches, out.Batch, out.API = p.Watches, p.Batch, p.API
	}
	return out, nil
}

// planName is the user's plan for messages, or "" with none.
func (a access) planName() string {
	if a.Admin {
		return "Admin"
	}
	if a.Plan == nil {
		return ""
	}
	return a.Plan.Name
}

// screenRequest is one screen asked for through the gate.
type screenRequest struct {
	UserID  int64
	Text    string // the address, optionally preceded by a chain name
	Chain   string // explicit chain, from the app or API; may be empty
	Kind    screenKind
	Channel string
	// From is the paying wallet of a kindPreSend check; optional.
	From string
	// Lang is the language of a kindPDF report.
	Lang string
	// Held means the caller already holds this user's busy slot (a batch).
	Held bool
	// Started, if set, is called once every check has passed and the screen
	// is about to run, so a channel can say "screening…" without saying it
	// to someone about to be refused.
	Started func(chain, address string)
}

// screenOutcome is a successful screen.
type screenOutcome struct {
	Chain, Address string
	Result         *screenResponse
	Raw            []byte // the API's JSON, for the app and API channels
	PDF            []byte
	PreSend        *preSendResponse // kindPreSend only; Result is its recipient
	Used, Limit    int              // today's screens after this one; Limit -1 is unlimited
}

// gate runs a screen for a customer, or refuses with a *gateError.
func (b *bot) gate(ctx context.Context, r screenRequest) (*screenOutcome, error) {
	chain, address, err := b.parseTarget(r.Text, r.Chain)
	if err != nil {
		var te *targetError
		if errors.As(err, &te) {
			return nil, &gateError{Code: te.code, Status: http.StatusBadRequest, Chain: te.chain}
		}
		return nil, err
	}

	acc, err := b.access(ctx, r.UserID)
	if err != nil {
		return nil, err
	}
	if !acc.Admin && acc.Plan == nil {
		return nil, refuse("no_plan", http.StatusPaymentRequired)
	}
	switch {
	case r.Kind == kindDetails && !acc.Details:
		return nil, &gateError{Code: "feature_locked", Status: http.StatusForbidden, Feature: "/details"}
	case r.Kind == kindPDF && !acc.PDF:
		return nil, &gateError{Code: "feature_locked", Status: http.StatusForbidden, Feature: "/pdf"}
	}

	if !r.Held {
		if !b.claim(r.UserID) {
			return nil, refuse("busy", http.StatusConflict)
		}
		defer b.release(r.UserID)
	}

	now := b.now()
	used, ok, err := b.store.Consume(ctx, r.UserID, now, acc.DailyScreens)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &gateError{Code: "limit_reached", Status: http.StatusTooManyRequests, Limit: acc.DailyScreens}
	}

	select {
	case b.screening <- struct{}{}:
	case <-ctx.Done():
		_ = b.store.Refund(context.WithoutCancel(ctx), r.UserID, now)
		return nil, ctx.Err()
	}
	defer func() { <-b.screening }()

	if r.Started != nil {
		r.Started(chain, address)
	}
	out := &screenOutcome{Chain: chain, Address: address, Used: used, Limit: acc.DailyScreens}
	switch r.Kind {
	case kindPDF:
		out.PDF, err = b.report(ctx, chain, address, r.Lang)
	case kindPreSend:
		out.PreSend, err = b.preSend(ctx, chain, strings.TrimSpace(r.From), address)
		if err == nil {
			out.Result = &out.PreSend.Recipient
		}
	default:
		out.Result, out.Raw, err = b.screen(ctx, chain, address)
	}
	if err != nil {
		// A failed screen costs nothing.
		if rerr := b.store.Refund(context.WithoutCancel(ctx), r.UserID, now); rerr != nil {
			b.log.Error("refund screen", "user", r.UserID, "error", rerr)
		}
		return nil, &gateError{Code: "screen_failed", Status: http.StatusBadGateway, Cause: err}
	}

	var score, coverage *float64
	var band *string
	if res := out.Result; res != nil {
		score, coverage, band = &res.Score, &res.Coverage, &res.Band
	}
	if err := b.store.RecordScreen(context.WithoutCancel(ctx), r.UserID, chain, address, r.Channel,
		score, band, coverage, now); err != nil {
		b.log.Error("record screen", "user", r.UserID, "error", err)
	}
	return out, nil
}

// langOf resolves the language to speak to a user who is not writing to us
// right now: their choice, else the client language last seen.
func (b *bot) langOf(ctx context.Context, userID int64) string {
	chosen, client, err := b.store.Langs(ctx, userID)
	if err != nil {
		b.log.Warn("user language", "user", userID, "error", err)
	}
	return normLang(chosen, client)
}

// resetsAt is when today's screen count resets.
func resetsAt(now time.Time) time.Time { return billing.Day(now).Add(24 * time.Hour) }
