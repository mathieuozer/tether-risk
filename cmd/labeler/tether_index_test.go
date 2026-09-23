package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/indexer"
	"github.com/mozer/tether-risk/internal/store"
	"github.com/mozer/tether-risk/pkg/tronindex"
)

func ev(name, addr string, minute int, extra ...string) tron.ContractEvent {
	res := map[string]string{"_user": addr}
	if name == "DestroyedBlackFunds" {
		res = map[string]string{"_blackListedUser": addr, "_balance": extra[0]}
	}
	return tron.ContractEvent{Name: name, TxID: name + addr, Result: res,
		Time: time.Date(2026, 9, 23, 12, minute, 0, 0, time.UTC)}
}

func TestLatestBlacklistEventDecides(t *testing.T) {
	add, remove := blacklistChanges([]tron.ContractEvent{
		ev("AddedBlackList", "A", 1),
		ev("DestroyedBlackFunds", "A", 2, "5000000"),
		ev("AddedBlackList", "B", 3),
		ev("RemovedBlackList", "B", 4), // released again
		ev("RemovedBlackList", "C", 5),
		ev("AddedBlackList", "C", 6), // frozen again
		ev("DestroyedBlackFunds", "D", 7, "1"), // not frozen here: ignored
	})
	var got []string
	for _, f := range add {
		got = append(got, f.Address+":"+f.DestroyedRaw.String())
	}
	if strings.Join(got, ",") != "A:5000000,C:0" {
		t.Errorf("frozen = %v, want A with 5 USDT destroyed, and C", got)
	}
	if strings.Join(remove, ",") != "B" {
		t.Errorf("released = %v, want B", remove)
	}
}

// The index vouches only for an unbroken run of blocks after the mark that
// reaches the present.
func TestIndexVouchesOnlyWithoutGaps(t *testing.T) {
	ctx := context.Background()
	admin, err := store.OpenClickHouse(ctx)
	if err != nil {
		t.Skipf("clickhouse unavailable: %v", err)
	}
	const db = "tron_index_test_labeler"
	for _, q := range []string{"DROP DATABASE IF EXISTS " + db, "CREATE DATABASE " + db} {
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	admin.Close()
	ch, err := store.OpenClickHouseDatabase(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	t.Setenv("CLICKHOUSE_DB", db)
	if err := store.MigrateClickHouse(ctx, ch, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	sink := &indexer.Sink{CH: ch, Writer: store.NewTransferWriter(ch, nil)}
	now := time.Now().UTC().Truncate(time.Second)
	write := func(n uint64, at time.Time) {
		if err := sink.WriteBlocks(ctx, []tronindex.BlockData{{Number: n, Time: at}}); err != nil {
			t.Fatal(err)
		}
	}
	for n := uint64(101); n <= 110; n++ {
		if n != 105 {
			write(n, now.Add(-time.Duration(110-n)*3*time.Second))
		}
	}
	if reason, _ := indexVouches(ctx, ch, 100); !strings.Contains(reason, "9 of the 10") {
		t.Errorf("with block 105 missing: %q", reason)
	}
	if reason, _ := indexVouches(ctx, ch, 105); reason != "" {
		t.Errorf("after the gap: %q, want none", reason)
	}
	if reason, _ := indexVouches(ctx, ch, 110); reason == "" {
		t.Error("nothing after the mark, yet the index vouched")
	}
	write(105, now.Add(-15*time.Second))
	if reason, _ := indexVouches(ctx, ch, 100); reason != "" {
		t.Errorf("gap filled: %q, want none", reason)
	}
	write(111, now.Add(-10*time.Minute)) // a stale head
	if reason, _ := indexVouches(ctx, ch, 100); !strings.Contains(reason, "behind") {
		t.Errorf("stale head: %q", reason)
	}
}
