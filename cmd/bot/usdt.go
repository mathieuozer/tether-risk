package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mozer/tether-risk/internal/billing"
	"github.com/mozer/tether-risk/internal/chain/tron"
)

// watchUSDT checks the payment address for incoming USDT and settles the
// invoices it matches, until ctx ends.
func (b *bot) watchUSDT(ctx context.Context) {
	tick := time.NewTicker(b.billing.USDT.Poll)
	defer tick.Stop()
	for {
		if err := b.scanUSDT(ctx); err != nil && ctx.Err() == nil {
			b.log.Warn("usdt scan failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// scanOverlap re-reads a little of what was already scanned. TronGrid lists
// only confirmed transfers, and a transfer confirmed late can carry a block
// time just before the previous scan's end. Already-known transactions are
// skipped, so the overlap costs nothing.
const scanOverlap = 10 * time.Minute

func (b *bot) scanUSDT(ctx context.Context) error {
	now := b.now()
	open, err := b.store.OpenInvoices(ctx, b.usdtAddr, now)
	if err != nil {
		return err
	}
	if len(open) == 0 {
		// Nothing can be matched, so there is nothing to read. Keep the
		// position current so the next invoice does not trigger a long scan.
		return b.store.SetScannedTo(ctx, b.usdtAddr, now.Add(-scanOverlap))
	}

	since := open[0].CreatedAt.Add(-time.Minute)
	if last, ok, err := b.store.ScannedTo(ctx, b.usdtAddr); err != nil {
		return err
	} else if ok && last.Add(-scanOverlap).After(since) {
		since = last.Add(-scanOverlap)
	}

	moves, err := b.tron.Inbound(ctx, b.usdtAddr, tron.USDTContract, since)
	if err != nil {
		return err
	}
	scanned := since
	for _, m := range moves {
		if m.Time.After(scanned) {
			scanned = m.Time
		}
		if err := b.handlePayment(ctx, m, open, now); err != nil {
			// Leave the position where it was so this payment is retried.
			return err
		}
	}
	return b.store.SetScannedTo(ctx, b.usdtAddr, scanned)
}

func (b *bot) handlePayment(ctx context.Context, m tron.Movement, open []billing.Invoice, now time.Time) error {
	known, err := b.store.KnownTx(ctx, m.TxID)
	if err != nil || known {
		return err
	}
	if !m.Value.IsInt64() {
		return nil // no plan costs this much; ignore rather than overflow
	}
	p := billing.Payment{Tx: m.TxID, From: m.From, Amount: m.Value.Int64(), BlockTime: m.Time}

	inv, ok := billing.Match(p, open, b.billing.USDT.Grace)
	if !ok {
		first, err := b.store.RecordUnmatched(ctx, p)
		if err != nil {
			return err
		}
		if first {
			b.notifyAdmins(ctx, fmt.Sprintf("💵 USDT received that matches no invoice: %s USDT from %s\ntx %s\nResolve with /grant if it is a customer's.",
				billing.FormatUSDT(p.Amount), p.From, p.Tx))
		}
		return nil
	}

	until, settled, err := b.store.Settle(ctx, inv, p, now)
	if err != nil || !settled {
		return err
	}
	plan := inv.Plan
	if pl, ok := b.billing.Plan(inv.Plan); ok {
		plan = pl.Name
	}
	b.say(ctx, inv.UserID, t(b.langOf(ctx, inv.UserID), "usdt_received", billing.FormatUSDT(p.Amount), plan, date(until)))
	b.notifyAdmins(ctx, fmt.Sprintf("💵 %s USDT from user %d for %s\ntx %s", billing.FormatUSDT(p.Amount), inv.UserID, plan, p.Tx))
	return nil
}

// report asks the API for the one-page PDF report.
func (b *bot) report(ctx context.Context, chain, address, lang string) ([]byte, error) {
	body, err := json.Marshal(map[string]string{"chain": chain, "address": address, "lang": lang})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.apiURL+"/v1/report", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("screening service unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Detail != "" {
			return nil, fmt.Errorf("%s", e.Detail)
		}
		return nil, fmt.Errorf("report failed (%d)", resp.StatusCode)
	}
	return raw, nil
}
