package main

import (
	"context"
	"errors"
	"strings"

	"github.com/mozer/tether-risk/internal/billing"
	"github.com/mozer/tether-risk/internal/report"
)

// Inline mode (docs/DECISIONS.md D34): typing "@bot T…" in any chat, a P2P
// group for instance, offers a check of that address. Telegram wants an
// inline answer within seconds and a screen can take a minute, so the result
// sent is a placeholder, and once the user sends it (chosen_inline_result,
// which needs inline feedback enabled with @BotFather) the screen runs and
// the placeholder is edited into the verdict. It counts as a screen of the
// user who sent it, like any other.

// inlineUserLang registers the user as any update does and returns their
// language.
func (b *bot) inlineUserLang(ctx context.Context, u tgUser) string {
	user, err := b.store.Touch(ctx, billing.User{ID: u.ID, Username: u.Username,
		FirstName: u.FirstName, ClientLang: u.LanguageCode})
	if err != nil {
		b.log.Error("touch user", "error", err)
	}
	return normLang(user.Lang, u.LanguageCode)
}

// inlineKeyboard carries the link to the bot. A result must carry a
// keyboard for Telegram to report its inline_message_id, without which the
// placeholder could not be edited.
func (b *bot) inlineKeyboard(lang string) *keyboard {
	btn := button{Text: t(lang, "inline_btn")}
	if b.username != "" {
		btn.URL = "https://t.me/" + b.username
	} else {
		btn.CallbackData = "inline"
	}
	return &keyboard{InlineKeyboard: [][]button{{btn}}}
}

func (b *bot) inlineQuery(ctx context.Context, q *inlineQuery) {
	lang := b.inlineUserLang(ctx, q.From)
	results := []inlineArticle{}
	if chain, address, err := b.parseTarget(strings.TrimSpace(q.Query), ""); err == nil {
		results = append(results, inlineArticle{
			Type:        "article",
			ID:          "s:" + chain + ":" + address,
			Title:       t(lang, "inline_title", shortAddr(address)),
			Description: t(lang, "inline_desc"),
			Content:     map[string]any{"message_text": t(lang, "inline_checking", address)},
			ReplyMarkup: b.inlineKeyboard(lang),
		})
	}
	if err := b.tg.answerInlineQuery(ctx, q.ID, results); err != nil {
		b.log.Warn("answer inline query", "error", err)
	}
}

func (b *bot) chosenInline(ctx context.Context, r *chosenInlineResult) {
	parts := strings.SplitN(r.ResultID, ":", 3)
	if len(parts) != 3 || parts[0] != "s" || r.InlineMessageID == "" {
		return
	}
	chain, address := parts[1], parts[2]
	lang := b.inlineUserLang(ctx, r.From)
	kb := b.inlineKeyboard(lang)

	out, err := b.gate(ctx, screenRequest{UserID: r.From.ID, Chain: chain, Text: address, Kind: kindSummary, Channel: chanInline})
	var text string
	switch {
	case err == nil:
		text = t(lang, "inline_address", address) + "\n\n" +
			report.VerdictBlock(connectionsVerdict(out.Result), lang) + t(lang, "inline_footer")
	default:
		var ge *gateError
		code := ""
		if errors.As(err, &ge) {
			code = ge.Code
		} else {
			b.log.Error("inline gate", "user", r.From.ID, "error", err)
		}
		switch code {
		case "no_plan":
			text = t(lang, "inline_no_plan")
		case "limit_reached":
			text = t(lang, "inline_limit")
		case "busy":
			text = t(lang, "busy")
		default:
			text = t(lang, "inline_failed")
		}
	}
	if err := b.tg.editInlineText(ctx, r.InlineMessageID, text, kb); err != nil {
		b.log.Warn("edit inline result", "user", r.From.ID, "error", err)
	}
}
