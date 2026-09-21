package labels

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mozer/tether-risk/internal/chain/tron"
)

// The fixture is cut from the real published SDN feed and covers the four
// cases that matter: an in-scope TRX listing, an ETH listing, a
// chain-ambiguous USDT listing, and an out-of-scope Bitcoin listing.
func openFixture(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "ofac", "sdn_sample.xml"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseOFACFixture(t *testing.T) {
	res, out, err := ParseOFAC(context.Background(), openFixture(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.EntriesScanned == 0 {
		t.Fatal("no entries scanned")
	}
	if len(res.Malformed) > 0 {
		t.Errorf("fixture produced malformed addresses: %v", res.Malformed)
	}
	if res.LabelsProduced == 0 {
		t.Fatal("no labels produced from a fixture containing sanctioned addresses")
	}

	var tronCount, ethCount int
	for _, l := range out {
		switch l.Chain {
		case "tron":
			tronCount++
			// A sanctioned address stored in a different encoding than the
			// chain data would never match, and the miss would be silent.
			// SPEC.md §9.1 calls that a build-breaking bug.
			norm, err := tron.Normalise(l.Address)
			if err != nil || norm != l.Address {
				t.Errorf("tron address %q is not canonical", l.Address)
			}
		case "ethereum", "bsc":
			ethCount++
			if l.Address != strings.ToLower(l.Address) {
				t.Errorf("evm address %q is not lower-cased; the label would split", l.Address)
			}
		default:
			t.Errorf("label on out-of-scope chain %q", l.Chain)
		}

		if l.Confidence != 1.0 {
			t.Errorf("%s: confidence = %v, SPEC.md §6.1 requires 1.0", l.Address, l.Confidence)
		}
		if l.Source != "ofac" {
			t.Errorf("%s: source = %q, want ofac", l.Address, l.Source)
		}
		if l.Category != "sanctions" && l.Category != "terrorist_financing" {
			t.Errorf("%s: category = %q, want sanctions or terrorist_financing", l.Address, l.Category)
		}
		if l.Entity == "" {
			t.Errorf("%s: entity is empty; the report would name no one", l.Address)
		}
		// Every label must be reconstructible back to its source record.
		for _, k := range []string{"sdn_uid", "programs", "currency_code", "listed_as"} {
			if _, ok := l.Evidence[k]; !ok {
				t.Errorf("%s: evidence is missing %q", l.Address, k)
			}
		}
	}

	if tronCount == 0 {
		t.Error("no TRON labels produced; TRON is the only chain with a live data path")
	}
	if ethCount == 0 {
		t.Error("no EVM labels produced from the fixture")
	}

	// Bitcoin must be skipped, not silently dropped. SPEC.md §1: no Bitcoin in
	// v1. Counting it is what tells us how much is waiting if that changes.
	if res.SkippedOffChain["XBT"] == 0 {
		t.Error("Bitcoin addresses must be counted as skipped, not dropped without trace")
	}
}

// USDT is listed by asset, not by chain, and exists on both TRON and Ethereum.
// Guessing would put sanctioned addresses on the wrong chain, where they would
// never match anything.
func TestChainForAmbiguousAssets(t *testing.T) {
	cases := []struct {
		currency, address, wantChain string
		wantOK                       bool
	}{
		{"USDT", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "tron", true},
		{"USDT", "0xa614f803b6fd780986a42c78ec9c7f77e6ded13c", "ethereum", true},
		{"USDC", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "tron", true},
		{"TRX", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "tron", true},
		{"ETH", "0xa614f803b6fd780986a42c78ec9c7f77e6ded13c", "ethereum", true},
		{"BSC", "0xa614f803b6fd780986a42c78ec9c7f77e6ded13c", "bsc", true},

		// Out of scope for v1.
		{"XBT", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "", false},
		{"XMR", "4AdUndXHHZ6cfufTMvppY6JwXNouMBzSkbLYfpAV5Usx3skxNgYeYTRJ5AmD5H3", "", false},
		{"SOL", "ETH2xkzTnJMPtTVSRtUNAFrLBfMEjJHnKHf9xy6Lq6Ks", "", false},

		// Ambiguous asset with an address in neither form.
		{"USDT", "not-an-address", "", false},
	}

	for _, tc := range cases {
		got, ok := chainForAddress(tc.currency, tc.address)
		if ok != tc.wantOK {
			t.Errorf("chainForAddress(%s, %s) ok = %v, want %v", tc.currency, tc.address, ok, tc.wantOK)
			continue
		}
		if got != tc.wantChain {
			t.Errorf("chainForAddress(%s, %s) = %q, want %q", tc.currency, tc.address, got, tc.wantChain)
		}
	}
}

// SDGT, SDT and FTO designations are terrorist financing specifically. Both
// categories weigh 100 and both always win resolution, so this changes no
// score — it makes the explanation accurate.
func TestOFACCategoryFromPrograms(t *testing.T) {
	cases := []struct {
		programs []string
		want     string
	}{
		{[]string{"IRAN", "IFSR", "IRGC", "SDGT"}, "terrorist_financing"},
		{[]string{"SDT"}, "terrorist_financing"},
		{[]string{"FTO", "SDGT"}, "terrorist_financing"},
		{[]string{"IRAN", "IFSR"}, "sanctions"},
		{[]string{"CYBER2"}, "sanctions"},
		{[]string{"DPRK"}, "sanctions"},
		{nil, "sanctions"},
		{[]string{" sdgt "}, "terrorist_financing"}, // whitespace and case
	}
	for _, tc := range cases {
		if got := ofacCategory(tc.programs); got != tc.want {
			t.Errorf("ofacCategory(%v) = %q, want %q", tc.programs, got, tc.want)
		}
	}
}

func TestCanonicaliseRejectsBadAddresses(t *testing.T) {
	for _, tc := range []struct{ chain, addr string }{
		{"tron", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6u"},           // bad checksum
		{"ethereum", "0xdeadbeef"},                               // too short
		{"ethereum", "a614f803b6fd780986a42c78ec9c7f77e6ded13c"}, // no 0x
		{"solana", "whatever"},                                   // unknown chain
	} {
		if _, err := canonicalise(tc.chain, tc.addr); err == nil {
			t.Errorf("canonicalise(%s, %s) succeeded; a sanctioned address we cannot "+
				"parse must be surfaced, not stored wrong", tc.chain, tc.addr)
		}
	}
}

// EIP-55 checksummed and all-lower-case forms are the same address. Storing
// both would split the label and let one form escape the sanctions match.
func TestEVMAddressesAreCaseNormalised(t *testing.T) {
	mixed := "0xA614f803b6FD780986A42c78Ec9c7f77e6DeD13c"
	lower := "0xa614f803b6fd780986a42c78ec9c7f77e6ded13c"

	a, err := canonicalise("ethereum", mixed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := canonicalise("ethereum", lower)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("checksummed and lower-case forms resolved differently: %s vs %s", a, b)
	}
}

// Running against the full published feed when it is available locally. This
// is the check that would catch the feed changing shape — a fixture cannot,
// because a fixture is frozen.
func TestParseOFACFullFeed(t *testing.T) {
	path := os.Getenv("OFAC_SDN_XML")
	if path == "" {
		path = "/tmp/ofac.xml"
	}
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("full SDN feed not present at %s; set OFAC_SDN_XML to run this", path)
	}
	defer f.Close()

	res, out, err := ParseOFAC(context.Background(), f)
	if err != nil {
		t.Fatalf("parse full feed: %v", err)
	}

	t.Logf("entries=%d addresses=%d labels=%d malformed=%d",
		res.EntriesScanned, res.AddressesFound, res.LabelsProduced, len(res.Malformed))

	if res.EntriesScanned < 10000 {
		t.Errorf("only %d entries scanned; the published list is far larger, "+
			"so the parser is probably not matching the document shape", res.EntriesScanned)
	}
	if len(res.Malformed) > 0 {
		// Loud on purpose: an unparseable sanctioned address is a miss, and
		// SPEC.md §9.1 makes a miss build-breaking.
		t.Errorf("%d sanctioned addresses failed to parse: %v", len(res.Malformed), res.Malformed)
	}

	var tronCount int
	for _, l := range out {
		if l.Chain == "tron" {
			tronCount++
		}
	}
	if tronCount == 0 {
		t.Error("no TRON sanctions labels from the full feed")
	}
	t.Logf("tron sanctions addresses: %d", tronCount)
}

// OFAC's "BNB" code covers two different chains: BNB Smart Chain (20-byte hex)
// and Binance Beacon Chain (bech32, "bnb1..."). Filing a Beacon Chain address
// as BSC would put a sanctioned address where nothing could ever match it.
// Found against the full published feed, sdn uid 58258.
func TestBNBBeaconChainIsNotBSC(t *testing.T) {
	if _, ok := chainForAddress("BNB", "bnb136ns6lfw4zs5hg4n85vdthaad7hq5m4gtkgf23"); ok {
		t.Error("a Binance Beacon Chain address must not be filed as BSC")
	}
	got, ok := chainForAddress("BNB", "0xa614f803b6fd780986a42c78ec9c7f77e6ded13c")
	if !ok || got != "bsc" {
		t.Errorf("a hex BNB address should map to bsc, got %q/%v", got, ok)
	}
}
