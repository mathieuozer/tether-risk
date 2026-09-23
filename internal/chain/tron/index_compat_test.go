package tron

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mozer/tether-risk/pkg/tronindex"
)

// Rows our own index will write must be the rows TronGrid gave us, byte for
// byte in the key, or after cutover a transfer already stored from TronGrid
// is stored again and the edges count it twice (docs/INDEXER_PLAN.md, D2,
// D11). The fixtures are real transactions: each one's node form
// (gettransactionbyid, gettransactioninfobyid) and the rows stored for it
// from TronGrid's account endpoints.
func TestIndexRowsMatchTronGridRows(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "testdata", "tronindex")
	names := []string{"usdt", "multi", "dust", "trx", "usdc", "usdd", "other"}
	cfg := tronindex.Config{NativeTRX: true, Keys: tronindex.KeyTronGrid} // every token
	for _, name := range names {
		var tx tronindex.RawTx
		var info tronindex.RawTxInfo
		readJSON(t, filepath.Join(dir, name+".tx.json"), &tx)
		readJSON(t, filepath.Join(dir, name+".info.json"), &info)
		at := time.UnixMilli(info.BlockTimeStamp).UTC()
		if info.BlockTimeStamp == 0 { // a plain TRX transfer has no receipt
			var raw struct {
				RawData struct {
					Timestamp int64 `json:"timestamp"`
				} `json:"raw_data"`
			}
			readJSON(t, filepath.Join(dir, name+".tx.json"), &raw)
			at = time.UnixMilli(raw.RawData.Timestamp).UTC()
		}
		ts, _, err := tronindex.ParseTx(tx, &info, info.BlockNumber, at, cfg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for _, x := range ts {
			asset := "TRX"
			if x.Token != "" {
				asset = assetForContract(x.Token)
			}
			got = append(got, fmt.Sprintf("%s\t%d\t%s\t%s\t%s\t%s", x.TxID, x.Index, x.From, x.To, asset, x.Value))
		}
		// Every stored row must come out of the index identically. The index
		// may yield more: our store holds only transfers touching addresses
		// someone fetched, while the index sees the whole transaction (the
		// usdd and other fixtures carry transfers between wallets never
		// fetched). Seeing those is the index's purpose.
		want := expectedRows(t, filepath.Join(dir, name+".expected.tsv"))
		have := map[string]bool{}
		for _, g := range got {
			have[g] = true
		}
		for _, w := range want {
			if !have[w] {
				sort.Strings(got)
				t.Errorf("%s: TronGrid row missing from the index or keyed differently:\n%s\nindex gave:\n%s", name, w, strings.Join(got, "\n"))
			}
		}
		if len(want) == 0 {
			t.Errorf("%s: fixture has no stored rows to compare", name)
		}
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// expectedRows reads tx, index, from, to, asset, value; the time column is
// left out because a TRX transfer's block time is not in its receipt here.
func expectedRows(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		cols := strings.Split(sc.Text(), "\t")
		if len(cols) < 6 {
			continue
		}
		out = append(out, strings.Join(cols[:6], "\t"))
	}
	return out
}
