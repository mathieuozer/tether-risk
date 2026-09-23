package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"sort"
	"time"

	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/indexer"
	"github.com/mozer/tether-risk/internal/labels"
	"github.com/mozer/tether-risk/internal/store"
)

// The blacklist from the TRON index (docs/DECISIONS.md D46).
//
// A full read from TronGrid rebuilds the list from every event since 2017.
// Between full reads, the index's events since the last one are applied to
// the list as it stands. That is exact as long as the index holds every
// block since that read, which is checked, not assumed: for each address
// the latest event decides, and the latest is either before the read (and
// in the list) or after it (and in the index).

const (
	tetherMarkName = "tether:trongrid" // indexer_cursors: the block the last full read vouches for
	fullReadEvery  = 24 * time.Hour    // TronGrid remains the ground truth once a day
	indexMaxLag    = 5 * time.Minute
)

type tetherMark struct {
	through uint64
	at      time.Time
	ok      bool
}

func loadTetherMark(ctx context.Context, pg *sql.DB) (tetherMark, error) {
	var m tetherMark
	var block int64
	err := pg.QueryRowContext(ctx, `SELECT block, updated_at FROM indexer_cursors WHERE name = $1`, tetherMarkName).
		Scan(&block, &m.at)
	if err == sql.ErrNoRows {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	m.through, m.ok = uint64(block), true
	return m, nil
}

func indexDB() string {
	if db := os.Getenv("INDEX_DB"); db != "" {
		return db
	}
	return "tron_index"
}

// tetherFromIndex applies the index's events since the mark. handled is
// false, with the reason logged, when a full read is due or the index cannot
// vouch for every block since the mark; the caller then reads TronGrid.
func tetherFromIndex(ctx context.Context, cfg *config.Config, st *labels.Store, mark tetherMark,
	log *slog.Logger) (fresh []string, handled bool, err error) {
	switch {
	case !mark.ok:
		log.Info("no full blacklist read recorded yet; reading TronGrid")
		return nil, false, nil
	case time.Since(mark.at) > fullReadEvery:
		log.Info("daily full blacklist read due")
		return nil, false, nil
	}
	ch, err := store.OpenClickHouseDatabase(ctx, indexDB())
	if err != nil {
		log.Info("index unavailable; reading TronGrid", "error", err)
		return nil, false, nil
	}
	defer ch.Close()
	if reason, err := indexVouches(ctx, ch, mark.through); err != nil {
		return nil, false, err
	} else if reason != "" {
		log.Warn("index cannot vouch for the blacklist; reading TronGrid", "reason", reason)
		return nil, false, nil
	}

	events, err := indexer.BlacklistSince(ctx, ch, mark.through)
	if err != nil {
		return nil, false, err
	}
	add, remove := blacklistChanges(events)
	before, err := st.CurrentAddresses(ctx, "tether_blacklist", "tron")
	if err != nil {
		return nil, false, err
	}
	var frozen []labels.FrozenAddress
	var released []string
	for _, f := range add {
		if !before[f.Address] {
			frozen = append(frozen, f)
			fresh = append(fresh, f.Address)
		}
	}
	for _, a := range remove {
		if before[a] {
			released = append(released, a)
		}
	}
	if len(frozen) == 0 && len(released) == 0 {
		log.Info("tether blacklist unchanged (index)", "events_since_full_read", len(events), "full_read_block", mark.through)
		return nil, true, nil // no snapshot
	}

	src, _ := cfg.Source("tether_blacklist")
	snapshotID, err := st.OpenSnapshot(ctx, "labeler tether (index)")
	if err != nil {
		return nil, false, err
	}
	if _, err := st.Upsert(ctx, snapshotID, labels.TetherLabels(frozen, src.Confidence)); err != nil {
		return nil, false, err
	}
	if _, err := st.RetireAddresses(ctx, snapshotID, "tether_blacklist", "tron", released); err != nil {
		return nil, false, err
	}
	if _, err := st.SealSnapshot(ctx, snapshotID); err != nil {
		return nil, false, err
	}
	log.Info("tether blacklist updated from the index", "snapshot", snapshotID,
		"newly_frozen", len(fresh), "released", len(released), "events", len(events))
	fmt.Printf("snapshot %d sealed from the index; %d newly frozen, %d released\n", snapshotID, len(fresh), len(released))
	return fresh, true, nil
}

// indexVouches returns why the index cannot stand in for TronGrid after
// block `after`, or "" when it holds every block since and is current.
func indexVouches(ctx context.Context, ch *sql.DB, after uint64) (string, error) {
	var blocks, last uint64
	var lastTime time.Time
	if err := ch.QueryRowContext(ctx, `
		SELECT uniqExact(block), max(block), argMax(block_time, block)
		FROM indexed_blocks WHERE chain = 'tron' AND block > ?`, after).Scan(&blocks, &last, &lastTime); err != nil {
		return "", err
	}
	switch {
	case blocks == 0:
		return fmt.Sprintf("no block after %d indexed", after), nil
	case blocks != last-after:
		return fmt.Sprintf("%d of the %d blocks after %d indexed", blocks, last-after, after), nil
	case time.Since(lastTime) > indexMaxLag:
		return fmt.Sprintf("index %s behind", time.Since(lastTime).Round(time.Second)), nil
	}
	return "", nil
}

// blacklistChanges reduces events, oldest first, to each address's latest
// state: frozen (with the USDT destroyed there since) or released.
func blacklistChanges(events []tron.ContractEvent) (add []labels.FrozenAddress, remove []string) {
	frozen := map[string]*labels.FrozenAddress{}
	released := map[string]bool{}
	for _, e := range events {
		switch e.Name {
		case "AddedBlackList":
			a := e.Result["_user"]
			frozen[a] = &labels.FrozenAddress{Address: a, AddedAt: e.Time, AddedTx: e.TxID, DestroyedRaw: new(big.Int)}
			delete(released, a)
		case "RemovedBlackList":
			a := e.Result["_user"]
			delete(frozen, a)
			released[a] = true
		case "DestroyedBlackFunds":
			if f, ok := frozen[e.Result["_blackListedUser"]]; ok {
				if v, ok := new(big.Int).SetString(e.Result["_balance"], 10); ok {
					f.DestroyedRaw.Add(f.DestroyedRaw, v)
				}
			}
		}
	}
	for _, f := range frozen {
		add = append(add, *f)
	}
	for a := range released {
		remove = append(remove, a)
	}
	sort.Slice(add, func(i, j int) bool { return add[i].Address < add[j].Address })
	sort.Strings(remove)
	return add, remove
}

// indexContacts returns the watched addresses with a transfer to or from
// one of frozen in the index: the days it holds that the main database may
// not have fetched yet. An unavailable index adds nothing.
func indexContacts(ctx context.Context, watched, frozen []string, log *slog.Logger) []string {
	ch, err := store.OpenClickHouseDatabase(ctx, indexDB())
	if err != nil {
		return nil
	}
	defer ch.Close()
	rows, err := ch.QueryContext(ctx, `
		SELECT DISTINCT a FROM (
			SELECT from_address AS a FROM transfers WHERE chain = 'tron' AND from_address IN (?) AND to_address IN (?)
			UNION ALL
			SELECT to_address AS a FROM transfers_by_to WHERE chain = 'tron' AND to_address IN (?) AND from_address IN (?)
		)`, watched, frozen, watched, frozen)
	if err != nil {
		log.Warn("index contacts", "error", err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if rows.Scan(&a) == nil {
			out = append(out, a)
		}
	}
	return out
}
