package evm

import (
	"context"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/mozer/tether-risk/internal/chain"
)

func alchemyAdapter(t *testing.T, url string) *AlchemyAdapter {
	t.Helper()
	return NewAlchemyAdapter(NewClient(Options{
		URL: url, RequestsPerSecond: 1000, Burst: 1000,
	}), AlchemyOptions{ChainID: "ethereum"})
}

// sampleTransfer is shaped exactly as the documented response.
func sampleTransfer() assetTransfer {
	t := assetTransfer{
		BlockNum: "0x18d0a00",
		Hash:     "0xAAAA111122223333444455556666777788889999aaaabbbbccccddddeeeeffff",
		From:     "0x28C6c06298d514Db089934071355E5743bf21d60",
		To:       "0x5041ed759Dd4aFc3a72b8192C143F72f4724081A",
		Value:    1234.5678,
		Asset:    "USDT",
		Category: "erc20",
		UniqueID: "0xaaaa111122223333444455556666777788889999aaaabbbbccccddddeeeeffff:log:42",
	}
	t.RawContract.Value = "0x499602d2" // 1234567890 exactly
	t.RawContract.Address = "0xdAC17F958D2ee523a2206206994597C13D831ec7"
	t.RawContract.Decimal = "0x6"
	t.Metadata.BlockTimestamp = "2026-09-21T06:00:00.000Z"
	return t
}

func TestAlchemyToTransfer(t *testing.T) {
	a := alchemyAdapter(t, "http://127.0.0.1:1")

	got, ok, err := a.toTransfer(sampleTransfer())
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !ok {
		t.Fatal("a valid erc20 transfer was rejected")
	}
	if err := got.Validate(); err != nil {
		t.Errorf("converted transfer is invalid: %v", err)
	}

	// Everything lower-cased, or edges split across encodings.
	for _, s := range []string{got.FromAddress, got.ToAddress, got.TxHash} {
		if s != strings.ToLower(s) {
			t.Errorf("field %q is not lower-cased", s)
		}
	}
	if got.BlockNumber != 0x18d0a00 {
		t.Errorf("block number = %d", got.BlockNumber)
	}
	if got.BlockTime.IsZero() || got.BlockTime.Year() != 2026 {
		t.Errorf("block time = %v", got.BlockTime)
	}
	// The log index must come from uniqueId, so this path and the raw
	// eth_getLogs path produce the same natural key and deduplicate against
	// each other (docs/DECISIONS.md D2).
	if got.LogIndex != 42 {
		t.Errorf("log index = %d, want 42 from the uniqueId", got.LogIndex)
	}
}

// The single most important property of this adapter.
//
// The API's top-level `value` is a JSON float already divided by the token's
// decimals. For a large USDT amount that has silently lost precision before it
// reaches us. `rawContract.value` is an exact hex integer and must win, or a
// score stops being reconstructible from stored data (SPEC.md §2).
func TestExactRawValueBeatsTheFloat(t *testing.T) {
	a := alchemyAdapter(t, "http://127.0.0.1:1")

	s := sampleTransfer()
	// Exactly 1234567890 raw units. The float says 1234.5678, which after
	// scaling by 6 decimals would also be 1234567800 — different, and wrong.
	s.RawContract.Value = "0x499602d2"
	s.Value = 1234.5678

	got, _, err := a.toTransfer(s)
	if err != nil {
		t.Fatal(err)
	}
	want := big.NewInt(1234567890)
	if got.RawValue.Cmp(want) != 0 {
		t.Errorf("raw value = %s, want %s exactly from rawContract.value", got.RawValue, want)
	}
	if got.PriceBasis == "unpriced_approx_amount" {
		t.Error("an exact rawContract.value must not be marked approximate")
	}

	// A very large amount is where the float would visibly fail. 2^90 has far
	// more significant digits than float64 can carry.
	huge := new(big.Int).Lsh(big.NewInt(1), 90)
	s.RawContract.Value = "0x" + huge.Text(16)
	got, _, err = a.toTransfer(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.RawValue.Cmp(huge) != 0 {
		t.Errorf("large raw value = %s, want %s; precision was lost", got.RawValue, huge)
	}
}

// When rawContract.value is absent the float is the only source available. The
// resulting imprecision must travel with the transfer rather than being
// forgotten.
func TestApproximateAmountIsMarked(t *testing.T) {
	a := alchemyAdapter(t, "http://127.0.0.1:1")

	s := sampleTransfer()
	s.RawContract.Value = ""
	s.RawContract.Decimal = ""
	s.Category = "external"
	s.Asset = "ETH"
	s.Value = 1.5

	got, ok, err := a.toTransfer(s)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("transfer rejected")
	}
	if got.PriceBasis != "unpriced_approx_amount" {
		t.Errorf("price basis = %q; a float-derived amount must be marked approximate", got.PriceBasis)
	}
	// 1.5 ETH at 18 decimals.
	want, _ := new(big.Int).SetString("1500000000000000000", 10)
	if got.RawValue.Cmp(want) != 0 {
		t.Errorf("raw value = %s, want %s", got.RawValue, want)
	}
}

// NFT transfers move no fungible value. Including them would distort the
// proportional split the haircut depends on.
func TestNFTCategoriesAreSkipped(t *testing.T) {
	a := alchemyAdapter(t, "http://127.0.0.1:1")

	for _, cat := range []string{"erc721", "erc1155", "specialnft"} {
		s := sampleTransfer()
		s.Category = cat
		_, ok, err := a.toTransfer(s)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Errorf("category %q was converted; it moves no fungible value", cat)
		}
	}

	for _, cat := range []string{"erc20", "external", "internal"} {
		s := sampleTransfer()
		s.Category = cat
		_, ok, err := a.toTransfer(s)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("category %q was skipped; it does move value", cat)
		}
	}
}

func TestLogIndexFromUniqueID(t *testing.T) {
	cases := []struct {
		id   string
		want uint32
	}{
		{"0xabc:log:42", 42},
		{"0xabc:log:0", 0},
		{"0xabc:external", 0},
		{"0xabc:internal:1", 0}, // not a log index
		{"", 0},
	}
	for _, tc := range cases {
		if got := logIndexFromUniqueID(tc.id); got != tc.want {
			t.Errorf("logIndexFromUniqueID(%q) = %d, want %d", tc.id, got, tc.want)
		}
	}
}

// The API filters on fromAddress OR toAddress, never both, so a complete
// history takes two passes. The cursor must track them independently or half
// the history is silently missed.
func TestFetchAddressDrainsBothDirections(t *testing.T) {
	var seenFrom, seenTo int

	srv := rpcServer(t, func(method string, params []any) (any, *rpcError) {
		if method != "alchemy_getAssetTransfers" {
			return nil, &rpcError{Code: -32601, Message: "unexpected " + method}
		}
		p := params[0].(map[string]any)

		_, hasFrom := p["fromAddress"]
		_, hasTo := p["toAddress"]
		if hasFrom && hasTo {
			return nil, &rpcError{Code: -32602,
				Message: "both fromAddress and toAddress set; the API filters on one"}
		}
		if hasFrom {
			seenFrom++
		}
		if hasTo {
			seenTo++
		}
		// One page per direction, then done.
		return map[string]any{"pageKey": "", "transfers": []any{}}, nil
	})

	a := alchemyAdapter(t, srv.URL)
	addr := "0x28c6c06298d514db089934071355e5743bf21d60"

	var cur chain.Cursor
	for i := 0; i < 10; i++ {
		page, err := a.FetchAddress(context.Background(), addr, cur)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		cur = page.Next
		if cur.Done {
			break
		}
	}

	if !cur.Done {
		t.Error("cursor never reported done")
	}
	if seenFrom == 0 {
		t.Error("outbound direction was never queried")
	}
	if seenTo == 0 {
		t.Error("inbound direction was never queried; half the history would be missed")
	}
}

// Page keys must differ per direction and per page, or the ingest ledger
// treats two distinct pages as one and silently drops the second.
func TestAlchemyPageKeysAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	var pages int

	srv := rpcServer(t, func(method string, params []any) (any, *rpcError) {
		p := params[0].(map[string]any)
		_, hasFrom := p["fromAddress"]
		// Two pages per direction.
		key := ""
		if _, ok := p["pageKey"]; !ok {
			key = "page2"
		}
		_ = hasFrom
		return map[string]any{"pageKey": key, "transfers": []any{}}, nil
	})

	a := alchemyAdapter(t, srv.URL)
	var cur chain.Cursor
	for i := 0; i < 8; i++ {
		page, err := a.FetchAddress(context.Background(),
			"0x28c6c06298d514db089934071355e5743bf21d60", cur)
		if err != nil {
			t.Fatal(err)
		}
		if seen[page.PageKey] {
			t.Errorf("page key %q repeated; the ingest ledger would drop this page", page.PageKey)
		}
		seen[page.PageKey] = true
		pages++
		cur = page.Next
		if cur.Done {
			break
		}
	}
	if pages < 4 {
		t.Errorf("drained %d pages, expected at least 4 (two per direction)", pages)
	}
}

func TestAlchemyCursorRoundTrip(t *testing.T) {
	in := alchemyCursor{OutKey: "uuid-a", OutDone: false, InKey: "uuid-b", InDone: true}
	if got := decodeAlchemyCursor(encodeAlchemyCursor(in)); got != in {
		t.Errorf("round trip changed state: %+v -> %+v", in, got)
	}
	if got := decodeAlchemyCursor("{not json"); got != (alchemyCursor{}) {
		t.Errorf("malformed cursor gave %+v, want a zero state", got)
	}
}

func TestAlchemyRejectsBadAddress(t *testing.T) {
	a := alchemyAdapter(t, "http://127.0.0.1:1")
	for _, bad := range []string{"", "0xdeadbeef", "not-an-address",
		"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"} {
		if _, err := a.FetchAddress(context.Background(), bad, chain.Cursor{}); err == nil {
			t.Errorf("FetchAddress(%q) succeeded for a non-EVM address", bad)
		}
	}
}

func TestAlchemyFetchRangePointsAtTheRightAdapter(t *testing.T) {
	a := alchemyAdapter(t, "http://127.0.0.1:1")
	_, err := a.FetchRange(context.Background(), 1, 100)
	if err == nil {
		t.Fatal("expected an error; this adapter is address-oriented")
	}
	if !strings.Contains(err.Error(), "eth_getLogs") {
		t.Errorf("error should name the adapter to use instead: %v", err)
	}
}

// Live check against a real Alchemy key. Skipped unless ALCHEMY_RPC_URL is
// set, so the suite stays runnable without credentials.
func TestLiveAlchemyFetchAddress(t *testing.T) {
	url := os.Getenv("ALCHEMY_RPC_URL")
	if url == "" {
		t.Skip("set ALCHEMY_RPC_URL to run the live check")
	}

	a := NewAlchemyAdapter(NewClient(Options{URL: url, RequestsPerSecond: 5, Burst: 2}),
		AlchemyOptions{ChainID: "ethereum", PageSize: 100})

	// A Binance hot wallet: guaranteed to have history in both directions.
	addr := "0x28c6c06298d514db089934071355e5743bf21d60"

	page, err := a.FetchAddress(context.Background(), addr, chain.Cursor{})
	if err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	t.Logf("fetched %d transfers, done=%v", len(page.Transfers), page.Next.Done)

	if len(page.Transfers) == 0 {
		t.Fatal("no transfers for an address known to be active")
	}
	for i, tr := range page.Transfers {
		if err := tr.Validate(); err != nil {
			t.Errorf("live transfer %d invalid: %v", i, err)
		}
	}
	first := page.Transfers[0]
	t.Logf("sample: %s -> %s, %s raw %s, block %d at %s",
		first.FromAddress, first.ToAddress, first.Asset,
		first.RawValue, first.BlockNumber, first.BlockTime.Format("2006-01-02"))

	var approx int
	for _, tr := range page.Transfers {
		if tr.PriceBasis == "unpriced_approx_amount" {
			approx++
		}
	}
	t.Logf("%d of %d amounts came from the lossy float field", approx, len(page.Transfers))
}
