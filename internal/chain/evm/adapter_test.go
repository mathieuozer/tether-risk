package evm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mozer/tether-risk/internal/chain"
)

// The fixture holds real USDT Transfer logs captured from a live Ethereum
// endpoint, so the parser is tested against what a node actually returns.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "evm", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// rpcServer replies to JSON-RPC calls with canned results.
func rpcServer(t *testing.T, handler func(method string, params []any) (any, *rpcError)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result, rpcErr := handler(req.Method, req.Params)

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testAdapter(t *testing.T, url string) *Adapter {
	t.Helper()
	return NewAdapter(NewClient(Options{
		URL: url, RequestsPerSecond: 1000, Burst: 1000,
	}), AdapterOptions{ChainID: "ethereum"})
}

func TestFetchRangeParsesRealLogs(t *testing.T) {
	logs, err := decodeLogs(fixture(t, "usdt_transfers.json"))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("fixture is empty")
	}

	srv := rpcServer(t, func(method string, params []any) (any, *rpcError) {
		switch method {
		case "eth_getLogs":
			return logs, nil
		case "eth_getBlockByNumber":
			return map[string]any{"timestamp": "0x68cf0000"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unknown method " + method}
	})

	a := testAdapter(t, srv.URL)
	out, err := a.FetchRange(context.Background(), 100, 110)
	if err != nil {
		t.Fatalf("fetch range: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no transfers parsed from a fixture containing Transfer logs")
	}

	for i, tr := range out {
		if err := tr.Validate(); err != nil {
			t.Errorf("transfer %d invalid: %v", i, err)
		}
		if tr.Chain != "ethereum" {
			t.Errorf("transfer %d chain = %q", i, tr.Chain)
		}
		// Lower-case everywhere: EIP-55 and all-lower forms are the same
		// address, and storing both would split the edge.
		for _, s := range []string{tr.FromAddress, tr.ToAddress, tr.Asset, tr.TxHash} {
			if s != strings.ToLower(s) {
				t.Errorf("transfer %d has a non-lower-cased field %q", i, s)
			}
		}
		if !strings.HasPrefix(tr.FromAddress, "0x") || len(tr.FromAddress) != 42 {
			t.Errorf("transfer %d from = %q, not a 20-byte address", i, tr.FromAddress)
		}
		if tr.BlockNumber == 0 {
			t.Errorf("transfer %d has no block number", i)
		}
		if tr.BlockTime.IsZero() {
			t.Errorf("transfer %d has no block time", i)
		}
		if tr.USDValue != nil {
			t.Errorf("transfer %d is priced before pricing runs", i)
		}
	}
}

// A 32-byte topic carries a 20-byte address in its low bytes. Anything else in
// the high bytes means the topic is not an address, and treating it as one
// would invent a counterparty.
func TestTopicToAddress(t *testing.T) {
	good := "0x00000000000000000000000028c6c06298d514db089934071355e5743bf21d60"
	got, err := topicToAddress(good)
	if err != nil {
		t.Fatalf("valid topic rejected: %v", err)
	}
	if got != "0x28c6c06298d514db089934071355e5743bf21d60" {
		t.Errorf("got %q", got)
	}

	for _, bad := range []string{
		"0xdeadbeef", // too short
		"0x1000000000000000000000000028c6c06298d514db089934071355e5743bf21d60", // high bytes set
		"",
	} {
		if _, err := topicToAddress(bad); err == nil {
			t.Errorf("topicToAddress(%q) succeeded; it is not an address", bad)
		}
	}
}

// Logs from reorganised blocks describe value that never moved.
func TestRemovedLogsAreSkipped(t *testing.T) {
	a := testAdapter(t, "http://127.0.0.1:1")
	l := rpcLog{
		Removed:         true,
		Topics:          []string{transferTopic, zeroPadded("0xaa"), zeroPadded("0xbb")},
		Data:            "0x64",
		BlockNumber:     "0x1",
		LogIndex:        "0x0",
		TransactionHash: "0xabc",
		BlockTimestamp:  "0x68cf0000",
	}
	_, ok, err := a.toTransfer(context.Background(), l, newBlockTimeCache(a.client))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a removed log was converted to a transfer")
	}
}

// An endpoint that caps the block span states it in prose. The adapter must
// narrow and continue rather than fail the whole range.
func TestBlockRangeCapIsHonoured(t *testing.T) {
	const cap = 50
	var widest uint64

	srv := rpcServer(t, func(method string, params []any) (any, *rpcError) {
		if method != "eth_getLogs" {
			return map[string]any{"timestamp": "0x68cf0000"}, nil
		}
		filter := params[0].(map[string]any)
		from, _ := hexToUint64(filter["fromBlock"].(string))
		to, _ := hexToUint64(filter["toBlock"].(string))
		span := to - from + 1
		if span > cap {
			return nil, &rpcError{Code: -32000,
				Message: fmt.Sprintf("Block range too large: maximum allowed is %d blocks", cap)}
		}
		if span > widest {
			widest = span
		}
		return []rpcLog{}, nil
	})

	a := NewAdapter(NewClient(Options{URL: srv.URL, RequestsPerSecond: 1000, Burst: 1000}),
		AdapterOptions{ChainID: "ethereum", BlockChunk: 2000})

	if _, err := a.FetchRange(context.Background(), 1000, 1200); err != nil {
		t.Fatalf("fetch should have narrowed and succeeded: %v", err)
	}
	if widest > cap {
		t.Errorf("requested %d blocks, above the endpoint's stated cap of %d", widest, cap)
	}
	if widest == 0 {
		t.Error("no request ever succeeded")
	}
}

// An archive refusal is not transient. It must surface as a typed error rather
// than being retried or turned into an empty result.
func TestArchiveRefusalIsTyped(t *testing.T) {
	var calls int
	srv := rpcServer(t, func(method string, params []any) (any, *rpcError) {
		calls++
		return nil, &rpcError{Code: -32000,
			Message: "Archive requests require a personal token."}
	})

	a := testAdapter(t, srv.URL)
	_, err := a.FetchRange(context.Background(), 1, 10)
	if err == nil {
		t.Fatal("archive refusal must be an error, not an empty result")
	}

	var archiveErr *ErrArchiveRequired
	if !errors.As(err, &archiveErr) {
		t.Errorf("error is not typed as archive-required: %v", err)
	}
	if calls != 1 {
		t.Errorf("made %d calls; an archive refusal will not fix itself on retry", calls)
	}
}

// SPEC.md §5 specifies demand-driven per-address ingestion. Raw JSON-RPC has
// no per-address history call, and public endpoints will not serve the
// historical scan that would substitute for one. Returning empty would read as
// "this address has no history", which is a different and wrong claim.
func TestFetchAddressIsHonestlyUnsupported(t *testing.T) {
	a := testAdapter(t, "http://127.0.0.1:1")

	page, err := a.FetchAddress(context.Background(), "0xabc", chain.Cursor{})
	if err == nil {
		t.Fatal("FetchAddress returned no error; an empty page would read as 'no history'")
	}
	var archiveErr *ErrArchiveRequired
	if !errors.As(err, &archiveErr) {
		t.Errorf("error should be typed as archive-required, got %T", err)
	}
	if len(page.Transfers) != 0 {
		t.Error("transfers returned alongside an error")
	}
	if !strings.Contains(err.Error(), "archive") {
		t.Errorf("error should explain what is missing: %v", err)
	}
}

func TestHead(t *testing.T) {
	srv := rpcServer(t, func(method string, params []any) (any, *rpcError) {
		if method == "eth_blockNumber" {
			return "0x18d0a00", nil
		}
		return nil, &rpcError{Code: -32601, Message: "unexpected"}
	})
	a := testAdapter(t, srv.URL)

	head, err := a.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if head != 0x18d0a00 {
		t.Errorf("head = %d, want %d", head, 0x18d0a00)
	}
}

// Block timestamps are cached per block, so the request count scales with
// blocks rather than with transfers. Without this a busy block costs one
// header fetch per transfer in it.
func TestBlockTimestampsAreCached(t *testing.T) {
	var headerCalls int
	srv := rpcServer(t, func(method string, params []any) (any, *rpcError) {
		if method == "eth_getBlockByNumber" {
			headerCalls++
			return map[string]any{"timestamp": "0x68cf0000"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "unexpected"}
	})

	a := testAdapter(t, srv.URL)
	cache := newBlockTimeCache(a.client)

	for i := 0; i < 20; i++ {
		if _, err := cache.get(context.Background(), 12345); err != nil {
			t.Fatal(err)
		}
	}
	if headerCalls != 1 {
		t.Errorf("made %d header calls for one block, want 1", headerCalls)
	}
}

func TestParseMaxBlocks(t *testing.T) {
	cases := []struct {
		msg  string
		want uint64
	}{
		{"Block range too large: maximum allowed is 50 blocks", 50},
		{"eth_getLogs is limited to 0 - 50 blocks range", 50},
		{"query returned more than 10000 results", 10000},
		{"something with no number", 0},
	}
	for _, tc := range cases {
		if got := parseMaxBlocks(tc.msg); got != tc.want {
			t.Errorf("parseMaxBlocks(%q) = %d, want %d", tc.msg, got, tc.want)
		}
	}
}

func TestClassifyErrors(t *testing.T) {
	archive := classify(&rpcError{Message: "Archive requests require a personal token"})
	if _, ok := archive.(*ErrArchiveRequired); !ok {
		t.Errorf("archive message classified as %T", archive)
	}

	rangeErr := classify(&rpcError{Message: "Block range too large: maximum allowed is 50 blocks"})
	if _, ok := rangeErr.(*ErrRangeTooLarge); !ok {
		t.Errorf("range message classified as %T", rangeErr)
	}

	other := classify(&rpcError{Code: -32601, Message: "method not found"})
	if _, ok := other.(*rpcError); !ok {
		t.Errorf("unrecognised message classified as %T", other)
	}
}

func zeroPadded(addr string) string {
	a := strings.TrimPrefix(addr, "0x")
	return "0x" + strings.Repeat("0", 64-len(a)) + a
}

// Live check against a real endpoint. Skipped unless EVM_RPC_URL is set, so
// the suite stays runnable offline; a fixture cannot catch an endpoint
// changing its response shape, and this can.
func TestLiveFetchRange(t *testing.T) {
	url := os.Getenv("EVM_RPC_URL")
	if url == "" {
		t.Skip("set EVM_RPC_URL to run the live check")
	}

	a := NewAdapter(NewClient(Options{URL: url, RequestsPerSecond: 5, Burst: 2}),
		AdapterOptions{
			ChainID: "ethereum",
			// USDT, the priority asset in SPEC.md §3.
			Tokens:     []string{"0xdac17f958d2ee523a2206206994597c13d831ec7"},
			BlockChunk: 5,
		})

	head, err := a.Head(context.Background())
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	t.Logf("head block: %d", head)

	// Recent blocks only: public endpoints refuse archive ranges.
	out, err := a.FetchRange(context.Background(), head-4, head)
	if err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	t.Logf("parsed %d USDT transfers from 5 blocks", len(out))

	if len(out) == 0 {
		t.Skip("no USDT transfers in the sampled blocks; not a failure")
	}
	for i, tr := range out {
		if err := tr.Validate(); err != nil {
			t.Errorf("live transfer %d invalid: %v", i, err)
		}
	}
	t.Logf("sample: %s -> %s, raw %s, block %d",
		out[0].FromAddress, out[0].ToAddress, out[0].RawValue, out[0].BlockNumber)
}
