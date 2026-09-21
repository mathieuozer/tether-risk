package labels

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mozer/tether-risk/internal/chain/tron"
)

func openAbuseFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "abuse", name))
	if err != nil {
		t.Fatalf("open fixture %s: %v", name, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseScamSniffer(t *testing.T) {
	res, out, err := ParseScamSniffer(context.Background(),
		openAbuseFixture(t, "scamsniffer_sample.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.LabelsProduced == 0 {
		t.Fatal("no labels produced")
	}
	if len(res.Malformed) > 0 {
		t.Errorf("malformed addresses in fixture: %v", res.Malformed)
	}

	for _, l := range out {
		// SPEC.md §6.4: abuse reports are unverified user submissions.
		if l.Confidence != 0.5 {
			t.Errorf("%s: confidence = %v, SPEC.md §6.4 requires 0.5", l.Address, l.Confidence)
		}
		if l.Source != "scamsniffer" {
			t.Errorf("%s: source = %q", l.Address, l.Source)
		}
		if l.Category != "scam" {
			t.Errorf("%s: category = %q, want scam", l.Address, l.Category)
		}
		if l.Address != strings.ToLower(l.Address) && l.Chain != "tron" {
			t.Errorf("%s: EVM address is not lower-cased; the label would split", l.Address)
		}
		if _, ok := l.Evidence["listed_as"]; !ok {
			t.Errorf("%s: evidence does not record what the feed listed", l.Address)
		}
	}
}

// The feed carries no per-address metadata. The ingester must not invent any.
func TestScamSnifferDoesNotInventMetadata(t *testing.T) {
	_, out, err := ParseScamSniffer(context.Background(),
		openAbuseFixture(t, "scamsniffer_sample.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no labels")
	}
	for _, l := range out {
		for _, forbidden := range []string{"subcategory", "reported_url", "description", "reporter"} {
			if _, present := l.Evidence[forbidden]; present {
				t.Errorf("%s: evidence carries %q, which this feed does not provide",
					l.Address, forbidden)
			}
		}
	}
}

func TestParseCryptoScamDB(t *testing.T) {
	res, out, err := ParseCryptoScamDB(context.Background(),
		openAbuseFixture(t, "cryptoscamdb_sample.yaml"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.EntriesScanned == 0 {
		t.Fatal("no entries scanned")
	}
	if len(res.Malformed) > 0 {
		t.Errorf("malformed addresses: %v", res.Malformed)
	}

	var tronCount int
	for _, l := range out {
		if l.Confidence != 0.5 {
			t.Errorf("%s: confidence = %v, want 0.5", l.Address, l.Confidence)
		}
		if l.Source != "cryptoscamdb" {
			t.Errorf("%s: source = %q", l.Address, l.Source)
		}
		if l.Category != "scam" {
			t.Errorf("%s: category = %q, want scam", l.Address, l.Category)
		}
		if l.Chain == "tron" {
			tronCount++
			norm, err := tron.Normalise(l.Address)
			if err != nil || norm != l.Address {
				t.Errorf("tron address %q is not canonical", l.Address)
			}
		}
		// Unlike ScamSniffer, this feed does carry structure, so the label
		// should say why the address was reported.
		if _, ok := l.Evidence["reported_url"]; !ok {
			t.Errorf("%s: evidence does not record the reporting URL", l.Address)
		}
	}

	if tronCount == 0 {
		t.Error("no TRON labels from a fixture built to contain them")
	}

	// Chains we do not ingest must be counted, not silently dropped.
	if res.SkippedOffChain["BTC"] == 0 {
		t.Error("Bitcoin addresses must be counted as skipped")
	}
}

// Out-of-scope asset codes must be skipped rather than guessed at. As with
// OFAC, BNB covers two different chains and the feed does not say which.
func TestChainForAssetCode(t *testing.T) {
	cases := []struct {
		code  string
		chain string
		ok    bool
	}{
		{"ETH", "ethereum", true},
		{"TRX", "tron", true},
		{"eth", "ethereum", true},
		{"BTC", "", false},
		{"XMR", "", false},
		{"BNB", "", false}, // ambiguous between BSC and Beacon Chain
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := chainForAssetCode(tc.code)
		if ok != tc.ok || got != tc.chain {
			t.Errorf("chainForAssetCode(%q) = %q/%v, want %q/%v",
				tc.code, got, ok, tc.chain, tc.ok)
		}
	}
}

func TestCSDBCategoryMapping(t *testing.T) {
	for _, in := range []string{"Phishing", "Scamming", "Malware", "Hacked",
		"'Scamming'", "phishing", "  Phishing  ", "SomethingNew"} {
		if got := csdbCategory(in); got != "scam" {
			t.Errorf("csdbCategory(%q) = %q, want scam", in, got)
		}
	}
}

func TestChainFromShape(t *testing.T) {
	cases := []struct {
		addr  string
		chain string
		ok    bool
	}{
		{"0xa614f803b6fd780986a42c78ec9c7f77e6ded13c", "ethereum", true},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "tron", true},
		{"1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "", false},
		{"0xdeadbeef", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := chainFromShape(tc.addr)
		if ok != tc.ok || got != tc.chain {
			t.Errorf("chainFromShape(%q) = %q/%v, want %q/%v", tc.addr, got, ok, tc.chain, tc.ok)
		}
	}
}

// These feeds' TRON coverage was measured, not assumed, and the measurement is
// what justifies the warning in METHODOLOGY.md. If a future feed update
// changes it substantially, this test is where that should be noticed.
func TestMeasuredTronCoverageIsStillNegligible(t *testing.T) {
	_, ss, err := ParseScamSniffer(context.Background(),
		openAbuseFixture(t, "scamsniffer_sample.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ss {
		if l.Chain == "tron" {
			t.Logf("ScamSniffer now carries TRON addresses (%s); "+
				"docs/METHODOLOGY.md says it carries none and should be revisited", l.Address)
			break
		}
	}
}
