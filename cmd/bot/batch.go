package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// Batch screening: a list of addresses in, one CSV out (docs/DECISIONS.md
// D27). Each address goes through the gate and uses one daily screen.

// batchTarget is one address from a batch, already parsed.
type batchTarget struct{ chain, address string }

// parseBatch reads addresses from free text: one per line, or separated by
// commas, semicolons or spaces, with an optional chain word before an EVM
// address. It returns the valid targets without duplicates and how many
// tokens were not addresses. A CSV header or other words simply count as
// invalid.
func (b *bot) parseBatch(text string) ([]batchTarget, int) {
	var out []batchTarget
	seen := map[string]bool{}
	invalid := 0
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		tokens := strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == ';' || r == '\t' || unicode.IsSpace(r)
		})
		for i := 0; i < len(tokens); i++ {
			tok := strings.Trim(tokens[i], `"'`)
			if tok == "" {
				continue
			}
			text := tok
			if _, isChain := chainAliases[strings.ToLower(tok)]; isChain && i+1 < len(tokens) {
				text = tok + " " + strings.Trim(tokens[i+1], `"'`)
				i++
			}
			chain, addr, err := b.parseTarget(text, "")
			if err != nil {
				invalid++
				continue
			}
			key := chain + ":" + addr
			if !seen[key] {
				seen[key] = true
				out = append(out, batchTarget{chain, addr})
			}
		}
	}
	return out, invalid
}

// checkBatch decides whether a user may run a batch of n addresses now.
func (b *bot) checkBatch(ctx context.Context, userID int64, n int) error {
	acc, err := b.access(ctx, userID)
	if err != nil {
		return err
	}
	if !acc.Admin && acc.Plan == nil {
		return refuse("no_plan", 402)
	}
	if acc.Batch == 0 {
		return &gateError{Code: "feature_locked", Status: 403, Feature: "batch"}
	}
	if acc.Batch > 0 && n > acc.Batch {
		return &gateError{Code: "limit_reached", Status: 429, Limit: acc.Batch, Feature: "batch"}
	}
	if acc.DailyScreens > 0 {
		used, err := b.store.Used(ctx, userID, b.now())
		if err != nil {
			return err
		}
		if left := acc.DailyScreens - used; n > left {
			return &gateError{Code: "limit_reached", Status: 429, Limit: left, Feature: "batch_day"}
		}
	}
	return nil
}

// batchFile handles a document sent to the bot.
func (b *bot) batchFile(ctx context.Context, c chatCtx, doc *document) {
	ext := strings.ToLower(filepath.Ext(doc.FileName))
	if (ext != ".txt" && ext != ".csv") || doc.FileSize > maxDownload {
		b.say(ctx, c.chat, t(c.lang, "batch_bad_file"))
		return
	}
	acc, err := b.access(ctx, c.user.ID)
	if err != nil {
		b.say(ctx, c.chat, t(c.lang, "error_ours"))
		return
	}
	if acc.Batch == 0 {
		b.sayWith(ctx, c.chat, t(c.lang, "batch_locked"), b.plansKeyboard(ctx, c))
		return
	}
	data, err := b.tg.download(ctx, doc.FileID)
	if err != nil {
		b.log.Warn("download batch file", "user", c.user.ID, "error", err)
		b.say(ctx, c.chat, t(c.lang, "batch_bad_file"))
		return
	}
	targets, invalid := b.parseBatch(string(data))
	if len(targets) == 0 {
		b.say(ctx, c.chat, t(c.lang, "batch_empty"))
		return
	}
	if err := b.checkBatch(ctx, c.user.ID, len(targets)); err != nil {
		var ge *gateError
		if errors.As(err, &ge) && ge.Code == "limit_reached" {
			if ge.Feature == "batch_day" {
				b.say(ctx, c.chat, t(c.lang, "batch_over_day", len(targets), ge.Limit))
			} else {
				b.say(ctx, c.chat, t(c.lang, "batch_too_many", len(targets), ge.Limit))
			}
			return
		}
		b.refusal(ctx, c, err)
		return
	}
	if !b.startBatch(c.user.ID, c.chat, c.lang, targets) {
		b.say(ctx, c.chat, t(c.lang, "busy"))
		return
	}
	b.say(ctx, c.chat, t(c.lang, "batch_started", len(targets), invalid))
}

// startBatch runs a batch in the background and delivers the CSV to chat. It
// holds the user's busy slot for the whole batch, so one customer cannot run
// two at once, and returns false if that slot is taken.
func (b *bot) startBatch(userID, chat int64, lang string, targets []batchTarget) bool {
	if !b.claim(userID) {
		return false
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.release(userID)
		b.runBatch(b.root, userID, chat, lang, targets)
	}()
	return true
}

type batchRow struct {
	chain, address string
	band           string
	score, cov     float64
	categories     string
	err            string
}

func (b *bot) runBatch(ctx context.Context, userID, chat int64, lang string, targets []batchTarget) {
	rows := make([]batchRow, len(targets))
	var stop error
	var once sync.Once

	// Two at a time: enough to overlap network waits without one customer
	// taking every screening slot.
	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for i, tg := range targets {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, tg batchTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			row := batchRow{chain: tg.chain, address: tg.address}
			out, err := b.gate(ctx, screenRequest{UserID: userID, Text: tg.address, Chain: tg.chain,
				Kind: kindSummary, Channel: chanBatch, Held: true})
			if err != nil {
				var ge *gateError
				if errors.As(err, &ge) && (ge.Code == "limit_reached" || ge.Code == "no_plan") {
					once.Do(func() { stop = err })
				}
				row.err = err.Error()
			} else {
				r := out.Result
				row.band, row.score, row.cov = r.Band, r.Score, r.Coverage
				row.categories = topCategories(r)
			}
			rows[i] = row
		}(i, tg)
	}
	wg.Wait()

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"chain", "address", "band", "score", "coverage_pct", "main_categories", "error"})
	var ok, failed int
	bands := map[string]int{}
	for _, r := range rows {
		if r.address == "" {
			continue
		}
		if r.err != "" {
			failed++
			_ = w.Write([]string{r.chain, r.address, "", "", "", "", r.err})
			continue
		}
		ok++
		bands[r.band]++
		_ = w.Write([]string{r.chain, r.address, r.band, fmt.Sprintf("%.1f", r.score),
			fmt.Sprintf("%.1f", r.cov*100), r.categories, ""})
	}
	w.Flush()

	name := fmt.Sprintf("screening-%s.csv", b.now().UTC().Format("20060102-1504"))
	if err := b.tg.sendDocument(ctx, chat, name, buf.Bytes(), ""); err != nil {
		b.log.Error("send batch results", "user", userID, "error", err)
	}
	b.say(ctx, chat, t(lang, "batch_done", ok, failed, bands["high"], bands["medium"], bands["low"]))
	if stop != nil {
		b.say(ctx, chat, t(lang, "batch_stopped", stop.Error()))
	}
}

// topCategories lists a result's three largest categories by combined share,
// for one CSV cell.
func topCategories(r *screenResponse) string {
	var total float64
	for _, d := range []*direction{r.Inbound, r.Outbound} {
		if d != nil && d.TotalTraced > 0 {
			total += d.TotalTraced
		}
	}
	if total <= 0 {
		return ""
	}
	share := map[string]float64{}
	for _, d := range []*direction{r.Inbound, r.Outbound} {
		if d == nil || d.TotalTraced <= 0 {
			continue
		}
		for _, c := range d.Categories {
			share[c.Category] += c.Pct * d.TotalTraced / total
		}
	}
	type kv struct {
		k string
		v float64
	}
	var all []kv
	for k, v := range share {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	var parts []string
	for i, c := range all {
		if i == 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %.1f%%", c.k, c.v))
	}
	return strings.Join(parts, "; ")
}
