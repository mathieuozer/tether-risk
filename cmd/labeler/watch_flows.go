package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/mozer/tether-risk/internal/billing"
	"github.com/mozer/tether-risk/internal/indexer"
	"github.com/mozer/tether-risk/internal/labels"
	"github.com/mozer/tether-risk/internal/store"
)

// Real-time watches (docs/DECISIONS.md D46).
//
// A watch is rescreened every six hours. With the TRON index, a watched
// address that sends to or receives from an address labelled in an alert
// category is made due within a minute of the transfer's block, and the
// bot's monitor rescreens it and alerts as it always does. Only what a
// rescreen would newly report triggers: a category the watch's last state
// already shows does not, and neither does a transfer the last check came
// after.

const watchFlowsCursor = "watch:flows" // indexer_cursors: the last block examined

// tronGridLag is how long after its block TronGrid may take to list a
// transfer. A check made sooner may not have seen it.
const tronGridLag = 2 * time.Minute

type watchRow struct {
	id      int64
	address string
	last    *billing.WatchState
	checked *time.Time
}

func watchFlows(ctx context.Context, pg *sql.DB, st *labels.Store, log *slog.Logger) error {
	ch, err := store.OpenClickHouseDatabase(ctx, indexDB())
	if err != nil {
		log.Info("index unavailable; watches keep their schedule", "error", err)
		return nil
	}
	defer ch.Close()
	cursors := indexer.Cursors{PG: pg}

	var head uint64
	if err := ch.QueryRowContext(ctx, `SELECT max(block) FROM indexed_blocks WHERE chain = 'tron'`).Scan(&head); err != nil {
		return err
	}
	from, ok, err := cursors.Load(ctx, watchFlowsCursor)
	if err != nil {
		return err
	}
	if !ok || head <= from {
		// The first run starts at the head: alerting on the index's whole
		// past at once would be a flood, and the six-hourly checks saw it.
		if !ok && head > 0 {
			return cursors.Save(ctx, watchFlowsCursor, head)
		}
		return nil
	}

	watches, err := loadWatches(ctx, pg)
	if err != nil {
		return err
	}
	if len(watches) == 0 {
		return cursors.Save(ctx, watchFlowsCursor, head)
	}
	byAddr := map[string][]watchRow{}
	var addrs []string
	for _, w := range watches {
		if len(byAddr[w.address]) == 0 {
			addrs = append(addrs, w.address)
		}
		byAddr[w.address] = append(byAddr[w.address], w)
	}

	rows, err := ch.QueryContext(ctx, `
		SELECT from_address, to_address, block_time FROM transfers
		WHERE chain = 'tron' AND from_address IN (?) AND block_number > ? AND block_number <= ?
		UNION ALL
		SELECT to_address, from_address, block_time FROM transfers_by_to
		WHERE chain = 'tron' AND to_address IN (?) AND block_number > ? AND block_number <= ?`,
		addrs, from, head, addrs, from, head)
	if err != nil {
		return err
	}
	var contacts []flow
	others := map[string]bool{}
	for rows.Next() {
		var c flow
		if err := rows.Scan(&c.watched, &c.other, &c.at); err != nil {
			rows.Close()
			return err
		}
		if c.watched != c.other {
			contacts = append(contacts, c)
			others[c.other] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var due []int64
	if len(contacts) > 0 {
		snap, err := st.LatestSealedSnapshot(ctx)
		if err != nil {
			return err
		}
		known, err := st.ForAddresses(ctx, snap, "tron", keys(others))
		if err != nil {
			return err
		}
		due = dueWatches(contacts, byAddr, known)
	}
	if len(due) > 0 {
		if _, err := pg.ExecContext(ctx, `UPDATE watches SET checked_at = NULL WHERE id = ANY($1)`, due); err != nil {
			return err
		}
		log.Info("watches with a new high-risk contact made due", "watches", len(due), "blocks", head-from)
	}
	return cursors.Save(ctx, watchFlowsCursor, head)
}

// flow is one transfer between a watched address and another, either way.
type flow struct {
	watched, other string
	at             time.Time
}

// dueWatches picks the watches a rescreen would report something new for:
// a contact labelled in an alert category the watch's last state lacks,
// from a transfer its last check did not see.
func dueWatches(flows []flow, byAddr map[string][]watchRow, known map[string][]labels.Label) []int64 {
	var due []int64
	seen := map[int64]bool{}
	for _, f := range flows {
		for _, l := range known[f.other] {
			if !slices.Contains(billing.AlertCategories, l.Category) {
				continue
			}
			for _, w := range byAddr[f.watched] {
				if seen[w.id] {
					continue
				}
				if w.last != nil && slices.Contains(w.last.RiskCategories, l.Category) {
					continue
				}
				if w.checked != nil && w.checked.After(f.at.Add(tronGridLag)) {
					continue
				}
				seen[w.id] = true
				due = append(due, w.id)
			}
		}
	}
	slices.Sort(due)
	return due
}

func loadWatches(ctx context.Context, pg *sql.DB) ([]watchRow, error) {
	rows, err := pg.QueryContext(ctx, `
		SELECT id, address, last_state, checked_at FROM watches
		WHERE chain = 'tron' AND removed_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []watchRow
	for rows.Next() {
		var w watchRow
		var state []byte
		if err := rows.Scan(&w.id, &w.address, &state, &w.checked); err != nil {
			return nil, err
		}
		if len(state) > 0 {
			var s billing.WatchState
			if json.Unmarshal(state, &s) == nil {
				w.last = &s
			}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
