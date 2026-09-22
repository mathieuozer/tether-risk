package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Checking a payment before it is sent (docs/DECISIONS.md D34): /send
// <recipient>, or /send <your wallet> <recipient> to also catch a
// look-alike of someone the wallet really pays.

type preSendResponse struct {
	Decision      string         `json:"decision"`
	LookalikeOf   string         `json:"lookalike_of"`
	LookalikeUSD  float64        `json:"lookalike_usd"`
	PaidBeforeUSD float64        `json:"paid_before_usd"`
	FirstPayment  bool           `json:"first_payment"`
	SenderKnown   bool           `json:"sender_known"`
	Recipient     screenResponse `json:"recipient"`
	Detail        string         `json:"detail"`
}

func (b *bot) preSend(ctx context.Context, chain, from, to string) (*preSendResponse, error) {
	body, err := json.Marshal(map[string]string{"chain": chain, "from": from, "to": to})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiURL+"/v1/presend", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("screening service unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var out preSendResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unexpected response from the screening service: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if out.Detail != "" {
			return nil, fmt.Errorf("%s", out.Detail)
		}
		return nil, fmt.Errorf("screening service returned %d", resp.StatusCode)
	}
	return &out, nil
}

// sendCommand answers /send.
func (b *bot) sendCommand(ctx context.Context, c chatCtx, arg string) {
	f := strings.Fields(arg)
	var from, to string
	switch len(f) {
	case 1:
		to = f[0]
	case 2:
		from, to = f[0], f[1]
	default:
		b.say(ctx, c.chat, t(c.lang, "send_usage"))
		return
	}
	if from != "" {
		if _, _, err := b.parseTarget(from, ""); err != nil {
			b.say(ctx, c.chat, t(c.lang, "bad_address"))
			return
		}
	}
	out, err := b.gate(ctx, screenRequest{UserID: c.user.ID, Text: to, From: from, Kind: kindPreSend, Channel: chanBot,
		Started: func(_, address string) { b.say(ctx, c.chat, t(c.lang, "screening", address)) }})
	if err != nil {
		b.refusal(ctx, c, err)
		return
	}
	following := b.startFollowUp(c.user.ID, c.lang, out.Chain, out.Address, out.Result)
	text := preSendText(out.PreSend, c.lang) + "\n" + summary(out.Result, c.lang, following)
	if err := b.tg.sendMessage(ctx, c.chat, text, nil); err != nil {
		b.log.Warn("deliver presend", "user", c.user.ID, "error", err)
	}
	if out.Limit > 0 && out.Limit-out.Used <= 3 {
		b.say(ctx, c.chat, t(c.lang, "screens_left", out.Limit-out.Used, out.Limit))
	}
}

// preSendText is the decision and why, above the recipient's own summary.
func preSendText(p *preSendResponse, lang string) string {
	var sb strings.Builder
	if p.Decision == "do_not_send" {
		sb.WriteString(t(lang, "send_no"))
	} else {
		sb.WriteString(t(lang, "send_yes"))
	}
	sb.WriteString("\n")
	switch {
	case p.LookalikeOf != "":
		sb.WriteString(t(lang, "send_lookalike", shortAddr(p.LookalikeOf), usdShort(p.LookalikeUSD)))
	case p.Decision == "do_not_send":
		sb.WriteString(t(lang, "send_risky"))
	}
	switch {
	case !p.SenderKnown:
		sb.WriteString(t(lang, "send_no_sender"))
	case p.FirstPayment && p.LookalikeOf == "":
		sb.WriteString(t(lang, "send_first"))
	case !p.FirstPayment:
		sb.WriteString(t(lang, "send_paid_before", usdShort(p.PaidBeforeUSD)))
	}
	return sb.String()
}

func shortAddr(a string) string {
	if len(a) < 12 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
}

func usdShort(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("$%.2fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("$%.1fk", v/1e3)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}
