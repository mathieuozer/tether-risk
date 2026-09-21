package tron

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mozer/tether-risk/internal/chain"
)

// Fixtures are real TronGrid responses captured from the live API, so the
// parser is tested against what the service actually returns rather than what
// its documentation implies. SPEC.md §9 requires golden fixtures so traversal
// and scoring are testable without network access.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "trongrid", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// fixtureServer serves captured responses, rewriting the upstream `next` link
// to point back at itself so pagination can be followed in tests.
func fixtureServer(t *testing.T, responses map[string]string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for pattern, name := range responses {
			if strings.Contains(r.URL.Path, pattern) {
				body := fixture(t, name)
				body = []byte(strings.ReplaceAll(string(body), "https://api.trongrid.io", srv.URL))
				w.Header().Set("Content-Type", "application/json")
				w.Write(body)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testAdapter(t *testing.T, baseURL string) *Adapter {
	t.Helper()
	return NewAdapter(NewClient(Options{
		BaseURL:           baseURL,
		RequestsPerSecond: 1000, // no artificial delay in tests
		Burst:             1000,
	}))
}

func TestFetchAddressParsesTRC20(t *testing.T) {
	srv := fixtureServer(t, map[string]string{"/transactions/trc20": "trc20_page1.json"})
	a := testAdapter(t, srv.URL)

	page, err := a.FetchAddress(context.Background(), "TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM", chain.Cursor{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(page.Transfers) == 0 {
		t.Fatal("no transfers parsed from a fixture that contains 20 items")
	}

	for i, tr := range page.Transfers {
		if err := tr.Validate(); err != nil {
			t.Errorf("transfer %d is invalid: %v", i, err)
		}
		if tr.Chain != "tron" {
			t.Errorf("transfer %d chain = %q, want tron", i, tr.Chain)
		}
		// Every address must be canonical base58, or edges split (see
		// address.go).
		for _, addr := range []string{tr.FromAddress, tr.ToAddress} {
			norm, err := Normalise(addr)
			if err != nil || norm != addr {
				t.Errorf("transfer %d address %q is not canonical", i, addr)
			}
		}
		if tr.BlockTime.IsZero() || tr.BlockTime.Year() < 2017 {
			t.Errorf("transfer %d has implausible block_time %v", i, tr.BlockTime)
		}
		if tr.RawValue == nil || tr.RawValue.Sign() < 0 {
			t.Errorf("transfer %d has bad raw_value", i)
		}
		// Phase 1 stores no prices; Phase 3 fills them in. "Unpriced" must not
		// be confused with zero.
		if tr.USDValue != nil {
			t.Errorf("transfer %d carries a USD value before pricing runs", i)
		}
		if tr.PriceBasis != "unpriced" {
			t.Errorf("transfer %d price_basis = %q, want unpriced", i, tr.PriceBasis)
		}
	}

	if page.PageKey == "" {
		t.Error("page key is empty; the ingest ledger cannot record this batch")
	}
	if page.Next.Done {
		t.Error("cursor reports done although the fixture has a next link")
	}
}

func TestFetchAddressParsesNative(t *testing.T) {
	srv := fixtureServer(t, map[string]string{"/transactions": "native_page1.json"})
	a := testAdapter(t, srv.URL)

	// Start with TRC-20 already drained so the native endpoint is used.
	cur := chain.Cursor{Value: encodeCursor(cursorState{TRC20Done: true})}
	page, err := a.FetchAddress(context.Background(), "TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM", cur)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	for i, tr := range page.Transfers {
		if err := tr.Validate(); err != nil {
			t.Errorf("native transfer %d invalid: %v", i, err)
		}
		if tr.Asset != "TRX" {
			t.Errorf("native transfer %d asset = %q, want TRX", i, tr.Asset)
		}
		// The native endpoint does return a block number, unlike TRC-20.
		if tr.BlockNumber == 0 {
			t.Errorf("native transfer %d has no block number", i)
		}
	}
}

// The synthetic log index is what makes TRC-20 deduplication possible at all,
// so its stability is load-bearing: the same transfer seen on any page, in any
// order, must produce the same index or it will be inserted twice and inflate
// `edges` (docs/DECISIONS.md D2 and D11).
func TestSyntheticLogIndexIsStable(t *testing.T) {
	item := trc20Item{
		TransactionID: "abc123",
		From:          "TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM",
		To:            "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		Value:         "5848284",
	}
	item.TokenInfo.Address = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

	first := syntheticLogIndex(item)
	for i := 0; i < 100; i++ {
		if got := syntheticLogIndex(item); got != first {
			t.Fatalf("index is not stable: %d then %d", first, got)
		}
	}

	// Distinct transfers must not collide.
	seen := map[uint32]string{first: "original"}
	variants := []struct {
		name   string
		mutate func(*trc20Item)
	}{
		{"different value", func(i *trc20Item) { i.Value = "5848285" }},
		{"different sender", func(i *trc20Item) { i.From = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t" }},
		{"different recipient", func(i *trc20Item) { i.To = "TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM" }},
		{"different token", func(i *trc20Item) { i.TokenInfo.Address = "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7" }},
	}
	for _, v := range variants {
		m := item
		v.mutate(&m)
		idx := syntheticLogIndex(m)
		if prev, clash := seen[idx]; clash {
			t.Errorf("%s collides with %s (index %d)", v.name, prev, idx)
		}
		seen[idx] = v.name
	}
}

// A page key must be identical across retries of the same request, or a
// retried page is recorded twice in the ingest ledger and its rows are
// inserted twice.
func TestPageKeyIsStableAcrossRetries(t *testing.T) {
	a := pageKey("trc20", "TZ8Ksz21", "https://api.trongrid.io/next?fingerprint=xyz", "fp1")
	b := pageKey("trc20", "TZ8Ksz21", "https://api.trongrid.io/next?fingerprint=xyz", "fp1")
	if a != b {
		t.Errorf("page key is not stable: %s vs %s", a, b)
	}

	// Different positions must produce different keys, or two distinct pages
	// would be treated as one and the second silently dropped.
	c := pageKey("trc20", "TZ8Ksz21", "https://api.trongrid.io/next?fingerprint=abc", "fp2")
	if a == c {
		t.Error("distinct pages produced the same key; one would be skipped")
	}
	if d := pageKey("native", "TZ8Ksz21", "", ""); d == pageKey("trc20", "TZ8Ksz21", "", "") {
		t.Error("the two endpoints' start pages share a key")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	in := cursorState{
		TRC20Next:  "https://api.trongrid.io/v1/x?fingerprint=abc",
		TRC20Done:  false,
		NativeNext: "https://api.trongrid.io/v1/y?fingerprint=def",
		NativeDone: true,
	}
	out := decodeCursor(encodeCursor(in))
	if out != in {
		t.Errorf("cursor round trip changed state:\n in: %+v\nout: %+v", in, out)
	}
}

// A malformed cursor must restart the address rather than fail the job.
// Re-fetching is safe because ingestion deduplicates; giving up is not.
func TestMalformedCursorRestarts(t *testing.T) {
	got := decodeCursor("{not json at all")
	if got != (cursorState{}) {
		t.Errorf("malformed cursor produced %+v, want a zero state", got)
	}
}

// Reverted transactions moved no value. Storing them would attribute exposure
// to transfers that never happened.
func TestRevertedTransactionsAreSkipped(t *testing.T) {
	a := &Adapter{}
	var tx nativeTx
	raw := `{
		"txID":"deadbeef","blockNumber":100,"block_timestamp":1620000000000,
		"ret":[{"contractRet":"REVERT"}],
		"raw_data":{"contract":[{"type":"TransferContract","parameter":{"value":{
			"amount":1000,
			"owner_address":"41a614f803b6fd780986a42c78ec9c7f77e6ded13c",
			"to_address":"41a614f803b6fd780986a42c78ec9c7f77e6ded13c"}}}]}}`
	if err := json.Unmarshal([]byte(raw), &tx); err != nil {
		t.Fatal(err)
	}
	got, err := a.nativeTransfers(tx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("reverted transaction produced %d transfers, want 0", len(got))
	}
}

// FetchRange must fail loudly rather than return an empty slice. An empty
// result reads as "this range had no transfers", which is a different and
// wrong claim.
func TestFetchRangeIsHonestlyUnsupported(t *testing.T) {
	a := testAdapter(t, "http://127.0.0.1:1")
	got, err := a.FetchRange(context.Background(), 1, 100)
	if err == nil {
		t.Fatal("FetchRange returned no error; an empty result would be read as 'no transfers'")
	}
	if got != nil {
		t.Errorf("FetchRange returned %d transfers alongside an error", len(got))
	}
	if !strings.Contains(err.Error(), "demand-driven") {
		t.Errorf("error should explain the design, got: %v", err)
	}
}

// Rate limiting and retry behaviour.
func TestClientRetriesServerErrors(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer srv.Close()

	c := NewClient(Options{BaseURL: srv.URL, RequestsPerSecond: 1000, Burst: 1000, MaxRetries: 5})
	var out trc20Response
	if err := c.get(context.Background(), srv.URL+"/x", &out); err != nil {
		t.Fatalf("expected retries to succeed: %v", err)
	}
	if calls != 3 {
		t.Errorf("made %d calls, want 3 (two failures then success)", calls)
	}
}

func TestClientDoesNotRetryClientErrors(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(Options{BaseURL: srv.URL, RequestsPerSecond: 1000, Burst: 1000, MaxRetries: 5})
	var out trc20Response
	if err := c.get(context.Background(), srv.URL+"/x", &out); err == nil {
		t.Fatal("expected a client error to be returned")
	}
	if calls != 1 {
		t.Errorf("made %d calls, want 1; a 400 will not fix itself", calls)
	}
}

func TestClientRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	c := NewClient(Options{BaseURL: srv.URL, RequestsPerSecond: 1000, Burst: 1000, MaxRetries: 20})
	var out trc20Response
	start := time.Now()
	if err := c.get(ctx, srv.URL+"/x", &out); err == nil {
		t.Fatal("expected cancellation to abort the retry loop")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("retry loop ignored cancellation for %v", elapsed)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("5"); got != 5*time.Second {
		t.Errorf(`parseRetryAfter("5") = %v, want 5s`, got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf(`parseRetryAfter("") = %v, want 0`, got)
	}
	if got := parseRetryAfter("not-a-date"); got != 0 {
		t.Errorf(`parseRetryAfter("not-a-date") = %v, want 0`, got)
	}
}

// The asset comes from the contract, never from the self-declared symbol.
// THk5qH79SoAaUnUh8JVdRarSESTZpqPjSQ is a real counterfeit found in stored
// data: symbol "USDT", 18 decimals. Read as Tether's 6 decimals, it was valued
// at $10.35 quadrillion (docs/DECISIONS.md D18).
func TestTRC20AssetComesFromContractNotSymbol(t *testing.T) {
	const counterfeit = "THk5qH79SoAaUnUh8JVdRarSESTZpqPjSQ"

	cases := []struct {
		name     string
		contract string
		symbol   string
		want     string
	}{
		{"genuine tether", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "USDT", "USDT"},
		{"genuine tether, symbol ignored", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "whatever", "USDT"},
		{"counterfeit claiming USDT", counterfeit, "USDT", counterfeit},
		{"counterfeit claiming the native asset", counterfeit, "TRX", counterfeit},
		{"unnamed token", counterfeit, "", counterfeit},
	}
	a := NewAdapter(nil)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it := trc20Item{
				TransactionID: "ce39232e160a507848c4a4a475d5394462f305d8cd74a71cb640e40feb46cc61",
				BlockTime:     1681823094000,
				From:          "TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM",
				To:            "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7",
				Type:          "Transfer",
				Value:         "10350355963000000000000",
			}
			it.TokenInfo.Address = c.contract
			it.TokenInfo.Symbol = c.symbol
			it.TokenInfo.Decimals = 18

			tr, ok, err := a.toTransfer(it)
			if err != nil || !ok {
				t.Fatalf("toTransfer: ok=%v err=%v", ok, err)
			}
			if tr.Asset != c.want {
				t.Errorf("asset = %q, want %q", tr.Asset, c.want)
			}
		})
	}
}

// A transfer whose contract cannot be identified is rejected rather than
// stored under a guessed asset.
func TestTRC20WithoutValidContractIsRejected(t *testing.T) {
	it := trc20Item{
		TransactionID: "abc",
		From:          "TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM",
		To:            "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7",
		Type:          "Transfer",
		Value:         "1",
	}
	it.TokenInfo.Symbol = "USDT"
	if _, _, err := NewAdapter(nil).toTransfer(it); err == nil {
		t.Fatal("expected an error for a transfer with no token contract")
	}
}

// Registry keys must already be in normalised form, or a genuine contract
// would miss the lookup and be silently demoted to unpriced.
func TestCanonicalTokensAreNormalised(t *testing.T) {
	for contract := range canonicalTokens {
		got, err := Normalise(contract)
		if err != nil || got != contract {
			t.Errorf("%s normalises to %q (err %v)", contract, got, err)
		}
	}
}
