package indexapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/indexer"
	"github.com/mozer/tether-risk/internal/store"
	"github.com/mozer/tether-risk/pkg/tronindex"
)

const (
	testDB = "tron_index_test"
	alice  = "TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM"
	bob    = "TWiZJrAmU9jgu64LqstWVnq7xNWHMqxGTS"
)

// openIndex creates an empty index database with the engine's schema.
func openIndex(t *testing.T) *indexer.Sink {
	t.Helper()
	ctx := context.Background()
	admin, err := store.OpenClickHouse(ctx)
	if err != nil {
		t.Skipf("clickhouse unavailable: %v", err)
	}
	for _, q := range []string{"DROP DATABASE IF EXISTS " + testDB, "CREATE DATABASE " + testDB} {
		if _, err := admin.ExecContext(ctx, q); err != nil {
			admin.Close()
			t.Fatal(err)
		}
	}
	admin.Close()
	t.Setenv("CLICKHOUSE_DB", testDB)
	ch, err := store.OpenClickHouse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ch.Close() })
	if err := store.MigrateClickHouse(ctx, ch, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	return &indexer.Sink{CH: ch, Writer: store.NewTransferWriter(ch, nil)}
}

// blocks 100..104: each has alice sending bob 1..5 USDT, and block 102 also
// a TRX transfer from bob to alice and a transfer from alice to herself.
func blocks() []tronindex.BlockData {
	start := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	var out []tronindex.BlockData
	for i := 0; i < 5; i++ {
		n := uint64(100 + i)
		at := start.Add(time.Duration(i) * 3 * time.Second)
		b := tronindex.BlockData{Number: n, Time: at}
		b.Transfers = append(b.Transfers, tronindex.Transfer{TxID: fmt.Sprintf("usdt%d", n), Index: 7, Block: n, Time: at,
			From: alice, To: bob, Token: tron.USDTContract, Value: big.NewInt(int64(i+1) * 1_000_000)})
		if n == 102 {
			b.Transfers = append(b.Transfers,
				tronindex.Transfer{TxID: "trx102", Index: 0, Block: n, Time: at, From: bob, To: alice, Value: big.NewInt(2_500_000)},
				tronindex.Transfer{TxID: "self102", Index: 0, Block: n, Time: at, From: alice, To: alice, Token: tron.USDTContract, Value: big.NewInt(9)})
		}
		out = append(out, b)
	}
	return out
}

func TestWritingBlocksTwiceStoresThemOnce(t *testing.T) {
	sink := openIndex(t)
	ctx := context.Background()
	bs := blocks()
	if err := sink.WriteBlocks(ctx, bs[:3]); err != nil {
		t.Fatal(err)
	}
	// A crash before the cursor moved: the next run writes 100..104, of
	// which 100..102 are already there.
	if err := sink.WriteBlocks(ctx, bs); err != nil {
		t.Fatal(err)
	}
	var rows uint64
	if err := sink.CH.QueryRowContext(ctx, `SELECT count() FROM transfers`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 7 {
		t.Errorf("transfers holds %d rows before any merge, want 7: a block was written twice", rows)
	}
	// The edge views aggregate on insert, so a repeat would double them
	// even after the transfers table collapsed it (D2).
	var usdt string
	if err := sink.CH.QueryRowContext(ctx, `
		SELECT toString(sum(total_raw_value)) FROM edges WHERE from_address = ? AND to_address = ? AND asset = 'USDT'`,
		alice, bob).Scan(&usdt); err != nil {
		t.Fatal(err)
	}
	if usdt != "15000000" {
		t.Errorf("alice->bob USDT edge = %s, want 15000000 (1+2+3+4+5 USDT, once each)", usdt)
	}
}

func get(t *testing.T, h http.Handler, url string, out any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
	if out != nil && rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s: %v", url, err)
		}
	}
	return rec.Code
}

type page struct {
	Data []Transfer `json:"data"`
	Meta Meta       `json:"meta"`
}

func TestTransfersPagesThroughEveryRowOnce(t *testing.T) {
	sink := openIndex(t)
	if err := sink.WriteBlocks(context.Background(), blocks()); err != nil {
		t.Fatal(err)
	}
	h := (&Server{CH: sink.CH}).Handler()

	var all []Transfer
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination does not end")
		}
		var p page
		if code := get(t, h, "/v1/accounts/"+alice+"/transfers?limit=2&cursor="+cursor, &p); code != 200 {
			t.Fatalf("status %d", code)
		}
		all = append(all, p.Data...)
		if p.Meta.Next == "" {
			break
		}
		cursor = p.Meta.Next
	}
	// 5 USDT out, 1 TRX in, and the self-transfer once.
	if len(all) != 7 {
		t.Fatalf("got %d transfers, want 7: %+v", len(all), all)
	}
	seen := map[string]bool{}
	for i, tr := range all {
		if seen[tr.TxHash] {
			t.Errorf("%s returned twice", tr.TxHash)
		}
		seen[tr.TxHash] = true
		if i > 0 && tr.Timestamp > all[i-1].Timestamp {
			t.Errorf("not newest first at %d", i)
		}
		if tr.TxHash == "trx102" && (tr.Direction != "in" || tr.Amount != "2.5" || tr.Asset != "TRX") {
			t.Errorf("TRX transfer = %+v", tr)
		}
		if tr.TxHash == "self102" && tr.Direction != "out" {
			t.Errorf("self-transfer direction %q, want out", tr.Direction)
		}
	}

	var in page
	get(t, h, "/v1/accounts/"+alice+"/transfers?direction=in", &in)
	if len(in.Data) != 1 || in.Data[0].TxHash != "trx102" {
		t.Errorf("direction=in returned %+v", in.Data)
	}
	var usdt page
	get(t, h, "/v1/accounts/"+bob+"/transfers?asset=usdt&min_timestamp="+
		fmt.Sprint(time.Date(2026, 9, 23, 12, 0, 6, 0, time.UTC).UnixMilli()), &usdt)
	if len(usdt.Data) != 3 {
		t.Errorf("bob's USDT from 12:00:06 = %d transfers, want 3 (blocks 102..104)", len(usdt.Data))
	}
	// The window starts inside the index, so it is complete; with no
	// minimum it reaches before the index and must say so.
	if usdt.Meta.Partial {
		t.Error("a window inside the index is marked partial")
	}
	if !in.Meta.Partial {
		t.Error("a window reaching before the index is not marked partial")
	}
	if in.Meta.Coverage.FirstBlock != 100 || in.Meta.Coverage.LastBlock != 104 || in.Meta.Coverage.Missing != 0 {
		t.Errorf("coverage = %+v", in.Meta.Coverage)
	}
}

func TestKeysAndBadInput(t *testing.T) {
	sink := openIndex(t)
	h := (&Server{CH: sink.CH, Keys: map[string]bool{"k1": true}}).Handler()
	if code := get(t, h, "/v1/status", nil); code != http.StatusUnauthorized {
		t.Errorf("no key: status %d, want 401", code)
	}
	open := (&Server{CH: sink.CH}).Handler()
	for _, u := range []string{
		"/v1/accounts/nonsense/transfers",
		"/v1/accounts/" + alice + "/transfers?limit=500",
		"/v1/accounts/" + alice + "/transfers?direction=up",
		"/v1/accounts/" + alice + "/transfers?cursor=bad!cursor",
		"/v1/accounts/" + alice + "/transfers?min_timestamp=yesterday",
	} {
		if code := get(t, open, u, nil); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", u, code)
		}
	}
}

func TestWordAddress(t *testing.T) {
	hexAddr, err := tron.Base58ToHex(alice)
	if err != nil {
		t.Fatal(err)
	}
	word := "000000000000000000000000" + hexAddr[2:]
	if got := wordAddress(word); got != alice {
		t.Errorf("wordAddress = %q, want %s", got, alice)
	}
}
