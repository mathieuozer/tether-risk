package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"sort"

	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/labels"
)

// activationSources are the labels whose wallets are worth grouping by
// operator: services, and the named wallets that can name a group.
var activationSources = []string{"derived:service", "derived:hotwallet", "derived:deposit",
	"derived:operator", "htx_por", "poloniex_por", "curated"}

// fetchActivations reads who created each labelled service wallet that has
// no stored activation yet, up to the run's budget (docs/DECISIONS.md D31).
func fetchActivations(ctx context.Context, cfg *config.Config, pg *sql.DB, chainID string, log *slog.Logger) error {
	chainCfg, ok := cfg.Chain(chainID)
	if !ok || !chainCfg.Available() || chainID != "tron" {
		return fmt.Errorf("activations are read for tron only")
	}
	key := os.Getenv("TRONGRID_API_KEY")
	client := tron.NewClient(tron.Options{BaseURL: chainCfg.URL, APIKey: key,
		RequestsPerSecond: chainCfg.RequestRate(key != ""), Logger: log})

	budget := cfg.Weights.DerivedOperator.MaxFetches
	if budget <= 0 {
		budget = 500
	}
	rows, err := pg.QueryContext(ctx, `
		SELECT DISTINCT l.address FROM labels l
		LEFT JOIN activations a ON a.chain = l.chain AND a.address = l.address
		WHERE l.chain = $1 AND l.source = ANY($2) AND l.valid_to_snapshot IS NULL AND a.address IS NULL
		ORDER BY l.address LIMIT $3`, chainID, activationSources, budget)
	if err != nil {
		return err
	}
	var todo []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var read, none, failed int
	for _, a := range todo {
		act, err := client.ActivationOf(ctx, a)
		switch {
		case errors.Is(err, tron.ErrNotFound):
			none++
			_, err = pg.ExecContext(ctx, `INSERT INTO activations (chain, address) VALUES ($1, $2)
				ON CONFLICT (chain, address) DO NOTHING`, chainID, a)
		case err != nil:
			// One unreadable account must not end the run; it is retried
			// next time because nothing was stored for it.
			failed++
			log.Warn("activation not read", "address", a, "error", err)
			continue
		default:
			read++
			_, err = pg.ExecContext(ctx, `INSERT INTO activations
				(chain, address, activator, tx_id, tx_type, amount, activated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (chain, address) DO NOTHING`,
				chainID, a, act.Activator, act.TxID, act.Type, act.Amount.String(), act.Time)
		}
		if err != nil {
			return fmt.Errorf("store activation of %s: %w", a, err)
		}
	}
	log.Info("activations read", "candidates", len(todo), "read", read, "no_creating_tx", none, "failed", failed,
		"budget", budget, "full_budget", len(todo) == budget)
	if len(todo) > 0 && failed == len(todo) {
		return fmt.Errorf("no activation could be read (%d failures)", failed)
	}
	return nil
}

// shortAddr is an address as the reports print it.
func shortAddr(a string) string {
	if len(a) < 12 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
}

// deriveOperators names wallets linked to an exchange's reserve wallets by
// account creation (docs/DECISIONS.md D31). It returns the new snapshot, or
// 0 when nothing new was named.
func deriveOperators(ctx context.Context, cfg *config.Config, pg *sql.DB, st *labels.Store,
	snapshotID int64, chainID string) (int64, error) {

	reserves, err := st.BySources(ctx, snapshotID, chainID, cfg.Weights.DerivedHotWallet.ReserveSources)
	if err != nil {
		return 0, err
	}
	namedLabels, err := st.BySources(ctx, snapshotID, chainID, []string{"curated", "htx_por", "poloniex_por",
		"derived:hotwallet", "derived:deposit", "derived:operator"})
	if err != nil {
		return 0, err
	}
	named, previous := labels.OperatorInputs(namedLabels)

	rows, err := pg.QueryContext(ctx, `
		SELECT address, activator, coalesce(tx_id, ''), coalesce(amount, 0)::text, coalesce(activated_at, 'epoch')
		FROM activations WHERE chain = $1 AND activator IS NOT NULL ORDER BY address`, chainID)
	if err != nil {
		return 0, err
	}
	var acts []labels.Activation
	for rows.Next() {
		var a labels.Activation
		var amount string
		if err := rows.Scan(&a.Address, &a.Activator, &a.TxID, &amount, &a.At); err != nil {
			rows.Close()
			return 0, err
		}
		a.Amount, _ = new(big.Int).SetString(amount, 10)
		if a.Amount == nil {
			a.Amount = new(big.Int)
		}
		acts = append(acts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	src, _ := cfg.Source("derived:operator")
	rules := cfg.Weights.DerivedOperator
	derived := labels.ReserveOperators(acts, reserves, named, rules.MinCreatorTRX, src.Confidence, chainID)
	fmt.Printf("operators:           %d named from %d activations and %d reserve wallets\n", len(derived), len(acts), len(reserves))
	for _, l := range derived {
		fmt.Printf("  %s  %s  (%v TRX)\n", l.Address, l.Entity, l.Evidence["trx"])
	}
	// The rules are rerun in full each time, so a label they no longer
	// produce (a threshold raised, a reserve delisted) is withdrawn.
	keep := map[string]bool{}
	for _, l := range derived {
		keep[l.Address] = true
	}
	var stale []string
	for addr := range previous {
		if !keep[addr] {
			stale = append(stale, addr)
		}
	}
	sort.Strings(stale)
	if len(derived) == 0 && len(stale) == 0 {
		return 0, nil
	}
	snap, err := st.OpenSnapshot(ctx, "derived operators")
	if err != nil {
		return 0, err
	}
	if len(stale) > 0 {
		n, err := st.RetireAddresses(ctx, snap, "derived:operator", chainID, stale)
		if err != nil {
			return 0, err
		}
		fmt.Printf("operators withdrawn: %d\n", n)
	}
	if _, err := st.Upsert(ctx, snap, derived); err != nil {
		return 0, err
	}
	if _, err := st.SealSnapshot(ctx, snap); err != nil {
		return 0, err
	}
	return snap, nil
}
