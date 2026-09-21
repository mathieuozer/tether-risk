// Command bot is a Telegram front end for the screening API.
//
// SPEC.md §8: "Address in, formatted breakdown out. Mirror the API exactly;
// the bot holds no logic of its own."
//
// That is taken literally. This binary speaks HTTP to the API and formats the
// response. It performs no traversal, applies no thresholds, and makes no
// judgement about a band. If the bot and the API ever disagreed about an
// address, the bot would be wrong by construction — so it is built so that
// cannot happen.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mozer/tether-risk/internal/report"
)

func main() {
	var (
		apiURL  = flag.String("api", "http://localhost:8080", "screening API base URL")
		chainID = flag.String("chain", "tron", "default chain")
		poll    = flag.Duration("poll", 2*time.Second, "long-poll interval")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// SPEC.md §2: no secrets in the repo, configuration via environment.
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Error("TELEGRAM_BOT_TOKEN is not set")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b := &bot{
		token:   token,
		apiURL:  strings.TrimRight(*apiURL, "/"),
		chainID: *chainID,
		http:    &http.Client{Timeout: 3 * time.Minute},
		log:     log,
	}

	log.Info("bot starting", "api", b.apiURL, "chain", b.chainID)
	if err := b.run(ctx, *poll); err != nil && ctx.Err() == nil {
		log.Error("bot failed", "error", err)
		os.Exit(1)
	}
}

type bot struct {
	token   string
	apiURL  string
	chainID string
	http    *http.Client
	log     *slog.Logger
	offset  int64
}

func (b *bot) run(ctx context.Context, poll time.Duration) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		updates, err := b.getUpdates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			b.log.Warn("get updates failed", "error", err)
			time.Sleep(poll)
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= b.offset {
				b.offset = u.UpdateID + 1
			}
			if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
				continue
			}
			b.handle(ctx, u.Message)
		}
	}
}

type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

func (b *bot) getUpdates(ctx context.Context) ([]update, error) {
	u := fmt.Sprintf("%s/bot%s/getUpdates?timeout=30&offset=%d",
		"https://api.telegram.org", b.token, b.offset)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payload struct {
		OK     bool     `json:"ok"`
		Result []update `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if !payload.OK {
		return nil, fmt.Errorf("telegram returned not-ok")
	}
	return payload.Result, nil
}

func (b *bot) handle(ctx context.Context, msg *struct {
	Text string `json:"text"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}) {
	text := strings.TrimSpace(msg.Text)

	switch {
	case text == "/start", text == "/help":
		b.send(ctx, msg.Chat.ID, helpText)
		return
	case strings.HasPrefix(text, "/details"):
		fields := strings.Fields(text)
		if len(fields) < 2 {
			b.send(ctx, msg.Chat.ID, "Usage: /details <address>")
			return
		}
		b.reply(ctx, msg.Chat.ID, fields[1], format)
		return
	case strings.HasPrefix(text, "/"):
		b.send(ctx, msg.Chat.ID, "Unknown command. Send an address, or /help.")
		return
	}

	b.reply(ctx, msg.Chat.ID, strings.Fields(text)[0], summary)
}

// reply screens an address and sends the result in the given format.
func (b *bot) reply(ctx context.Context, chatID int64, address string, render func(*screenResponse) string) {
	b.send(ctx, chatID, "Screening "+address+"...")

	res, err := b.screen(ctx, address)
	if err != nil {
		b.log.Warn("screen failed", "address", address, "error", err)
		b.send(ctx, chatID, "Could not screen that address.\n\n"+err.Error())
		return
	}
	b.send(ctx, chatID, render(res))
}

const helpText = `Address risk screening.

Send a blockchain address and I will return a summary of its connections.
Send /details <address> for the full per-direction breakdown with paths.

This is automated triage and pre-screening built on open data. It is not a
regulated AML determination and must not be used as one.

Always read the coverage figure alongside the score. Low coverage means most
traced value could not be attributed to a known entity — unknown, not clean.`

// screenResponse mirrors the API's response. Deliberately a separate decode
// rather than an import: the bot consumes the API as any other client would,
// so a breaking change to the response shows up here as a decode failure
// rather than being papered over by shared types.
type screenResponse struct {
	Address string  `json:"address"`
	Chain   string  `json:"chain"`
	Score   float64 `json:"score"`
	Band    string  `json:"band"`

	Coverage      float64 `json:"coverage"`
	LowConfidence bool    `json:"low_confidence"`

	SanctionsOverride bool `json:"sanctions_override"`
	BandCappedByAbuse bool `json:"band_capped_by_abuse_rule"`

	Inbound  *direction `json:"inbound"`
	Outbound *direction `json:"outbound"`

	OwnLabel *struct {
		Entity   string `json:"entity"`
		Category string `json:"category"`
	} `json:"own_label"`

	Activity *struct {
		InUSD             float64 `json:"in_usd"`
		OutUSD            float64 `json:"out_usd"`
		InTransfers       uint64  `json:"in_transfers"`
		OutTransfers      uint64  `json:"out_transfers"`
		InCounterparties  uint64  `json:"in_counterparties"`
		OutCounterparties uint64  `json:"out_counterparties"`
		FirstSeen         string  `json:"first_seen"`
		LastSeen          string  `json:"last_seen"`
		Assets            []struct {
			Asset  string  `json:"asset"`
			InUSD  float64 `json:"in_usd"`
			OutUSD float64 `json:"out_usd"`
		} `json:"assets"`
		UnpricedTransfers uint64 `json:"unpriced_transfers"`
		UnpricedTokens    uint64 `json:"unpriced_tokens"`
	} `json:"activity"`

	Depth *struct {
		FetchError          string `json:"fetch_error"`
		StillFetching       bool   `json:"still_fetching"`
		HistoryTruncated    bool   `json:"history_truncated"`
		Counterparties      int    `json:"counterparties"`
		Traced              int    `json:"traced"`
		TotalCounterparties int    `json:"total_counterparties"`
	} `json:"depth"`

	LabelSnapshotID int64  `json:"label_snapshot_id"`
	ConfigVersion   string `json:"config_version"`
	Disclaimer      string `json:"disclaimer"`

	Error  string `json:"error"`
	Detail string `json:"detail"`
}

type direction struct {
	Score           float64 `json:"score"`
	Coverage        float64 `json:"coverage"`
	UnattributedPct float64 `json:"unattributed_pct"`
	TotalTraced     float64 `json:"total_traced"`
	Categories      []struct {
		Category string  `json:"category"`
		Pct      float64 `json:"pct"`
	} `json:"categories"`
	TopPaths []struct {
		Explanation string `json:"explanation"`
	} `json:"top_paths"`
	Connections []struct {
		Address  string  `json:"address"`
		Entity   string  `json:"entity"`
		Category string  `json:"category"`
		Pct      float64 `json:"pct"`
		MinHops  int     `json:"min_hops"`
	} `json:"connections"`
	UnattributedReasons []struct {
		Reason string  `json:"reason"`
		Pct    float64 `json:"pct"`
	} `json:"unattributed_reasons"`
	Traversal struct {
		FanoutCapped    bool `json:"fanout_capped"`
		HopLimitReached bool `json:"hop_limit_reached"`
	} `json:"traversal"`
}

func (b *bot) screen(ctx context.Context, address string) (*screenResponse, error) {
	body, err := json.Marshal(map[string]string{"chain": b.chainID, "address": address})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiURL+"/v1/screen", bytes.NewReader(body))
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

	var out screenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unexpected response from the screening service: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if out.Detail != "" {
			return nil, fmt.Errorf("%s", out.Detail)
		}
		return nil, fmt.Errorf("screening failed (%d)", resp.StatusCode)
	}
	return &out, nil
}

func format(r *screenResponse) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Address: %s\nChain: %s\n\n", r.Address, r.Chain)
	fmt.Fprintf(&b, "Risk band: %s\nScore: %.1f / 100\n", strings.ToUpper(r.Band), r.Score)

	// Coverage is never separated from the score. A score without it is not
	// actionable, and in a chat window a reader will take whatever is on the
	// line above.
	fmt.Fprintf(&b, "Coverage: %.1f%%\n", r.Coverage*100)

	if l := r.OwnLabel; l != nil {
		fmt.Fprintf(&b, "\n*** THIS ADDRESS IS DIRECTLY LISTED ***\n%s (%s)\n", l.Entity, l.Category)
	}
	if r.SanctionsOverride {
		b.WriteString("\n*** DIRECT SANCTIONS MATCH ***\n" +
			"This address is on a sanctions list. The band is High regardless of score.\n")
	}
	if r.LowConfidence {
		fmt.Fprintf(&b, "\n*** LOW CONFIDENCE ***\n"+
			"Only %.1f%% of traced value reached a known entity. The remaining %.1f%%\n"+
			"is unknown, not clean.\n", r.Coverage*100, 100-r.Coverage*100)
	}
	if r.BandCappedByAbuse {
		b.WriteString("\nBand capped: the only evidence is unverified abuse reports.\n")
	}

	writeDirection(&b, "Inbound (where funds came from)", r.Inbound)
	writeDirection(&b, "Outbound (where funds went)", r.Outbound)

	fmt.Fprintf(&b, "\nLabel snapshot %d | config %s\n", r.LabelSnapshotID, r.ConfigVersion)
	if r.Disclaimer != "" {
		fmt.Fprintf(&b, "\n%s", r.Disclaimer)
	}
	return b.String()
}

// summary renders the compact connections list.
func summary(r *screenResponse) string {
	in := report.ConnectionsInput{
		Address:           r.Address,
		Chain:             r.Chain,
		Score:             r.Score,
		Band:              r.Band,
		Coverage:          r.Coverage,
		LowConfidence:     r.LowConfidence,
		SanctionsOverride: r.SanctionsOverride,
		BandCappedByAbuse: r.BandCappedByAbuse,
		Inbound:           summaryDirection(r.Inbound),
		Outbound:          summaryDirection(r.Outbound),
		Disclaimer:        r.Disclaimer,
	}
	if r.OwnLabel != nil {
		in.OwnLabel = &report.ConnectionsOwnLabel{Entity: r.OwnLabel.Entity, Category: r.OwnLabel.Category}
	}
	if d := r.Depth; d != nil {
		in.Depth = &report.ConnectionsDepth{
			FetchError: d.FetchError, StillFetching: d.StillFetching, HistoryTruncated: d.HistoryTruncated,
			Counterparties: d.Counterparties,
			Traced:         d.Traced, TotalCounterparties: d.TotalCounterparties,
		}
	}
	if a := r.Activity; a != nil {
		act := &report.ConnectionsActivity{
			InUSD: a.InUSD, OutUSD: a.OutUSD,
			InTransfers: a.InTransfers, OutTransfers: a.OutTransfers,
			InCounterparties: a.InCounterparties, OutCounterparties: a.OutCounterparties,
			FirstSeen: a.FirstSeen, LastSeen: a.LastSeen,
			UnpricedTransfers: a.UnpricedTransfers, UnpricedTokens: a.UnpricedTokens,
		}
		for _, as := range a.Assets {
			act.Assets = append(act.Assets, report.ConnectionsAsset{Asset: as.Asset, USD: as.InUSD + as.OutUSD})
		}
		in.Activity = act
	}
	return report.Connections(in)
}

func summaryDirection(d *direction) *report.ConnectionsDirection {
	if d == nil {
		return nil
	}
	out := &report.ConnectionsDirection{
		TracedWeight:    d.TotalTraced,
		UnattributedPct: d.UnattributedPct,
		FanoutCapped:    d.Traversal.FanoutCapped,
		HopLimitReached: d.Traversal.HopLimitReached,
	}
	for _, c := range d.Categories {
		out.Categories = append(out.Categories, report.ConnectionsCategory{Category: c.Category, Pct: c.Pct})
	}
	for _, c := range d.Connections {
		out.Entries = append(out.Entries, report.ConnectionsEntry{
			Address: c.Address, Entity: c.Entity, Category: c.Category, Pct: c.Pct, MinHops: c.MinHops,
		})
	}
	for _, rs := range d.UnattributedReasons {
		out.Reasons = append(out.Reasons, report.ConnectionsReason{Reason: rs.Reason, Pct: rs.Pct})
	}
	return out
}

func writeDirection(b *strings.Builder, title string, d *direction) {
	if d == nil {
		return
	}
	fmt.Fprintf(b, "\n%s\n", title)

	if len(d.Categories) == 0 && d.UnattributedPct == 0 {
		b.WriteString("  no traced value\n")
		return
	}
	for _, c := range d.Categories {
		fmt.Fprintf(b, "  %-20s %6.2f%%\n", c.Category, c.Pct)
	}
	if d.UnattributedPct > 0 {
		fmt.Fprintf(b, "  %-20s %6.2f%%  (unknown)\n", "unattributed", d.UnattributedPct)
	}
	if d.Traversal.FanoutCapped {
		b.WriteString("  note: truncated at the neighbour cap\n")
	}
	if d.Traversal.HopLimitReached {
		b.WriteString("  note: hop limit reached\n")
	}
	for i, p := range d.TopPaths {
		if i == 0 {
			b.WriteString("  top paths:\n")
		}
		if i >= 3 {
			break
		}
		fmt.Fprintf(b, "    - %s\n", p.Explanation)
	}
}

func (b *bot) send(ctx context.Context, chatID int64, text string) {
	form := url.Values{}
	form.Set("chat_id", fmt.Sprint(chatID))
	form.Set("text", text)

	u := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := b.http.Do(req)
	if err != nil {
		b.log.Warn("send failed", "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}
