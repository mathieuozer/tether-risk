package labels

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mozer/tether-risk/internal/config"
)

// fakeSampler replays measured profiles, so the thresholds are tested against
// the numbers that actually motivated them.
type fakeSampler struct {
	profiles map[string]Sample
	err      map[string]error
	calls    int
}

func (f *fakeSampler) Sample(_ context.Context, address string, pages, pageSize int) (Sample, error) {
	f.calls++
	if e, ok := f.err[address]; ok {
		return Sample{}, e
	}
	return f.profiles[address], nil
}

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The profiles below are the real measurements taken on 2026-09-21 that the
// thresholds in weights.yaml were calibrated against. If a threshold changes
// so that these outcomes flip, that is a deliberate act and this test is where
// it should be noticed.
func measuredProfiles() map[string]Sample {
	return map[string]Sample{
		// 600 sampled, 514 distinct, still paginating. Unambiguously a service.
		"TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM": {
			Transfers: 600, Counterparties: 514, MorePages: true, PagesFetched: 3,
		},
		// 78 sampled, 52 distinct, history exhausted.
		"TAythDdKTZeNq6VnQ7o9cEvWRQGgRpPiKX": {
			Transfers: 78, Counterparties: 52, MorePages: false, PagesFetched: 1,
		},
		// 39 sampled, 31 distinct. A high distinct-per-transfer ratio (0.795)
		// on a tiny sample — the case that rules out using ratio alone.
		"TNwf8VBNCkg7Y1pgyzbHdWdekkamoqcrmL": {
			Transfers: 39, Counterparties: 31, MorePages: false, PagesFetched: 1,
		},
		// 237 sampled, 20 distinct. Concentrated, not a service.
		"TR5e7yKoXkvodEtKdp5JmzgfKmsFdqAwu4": {
			Transfers: 237, Counterparties: 20, MorePages: false, PagesFetched: 2,
		},
	}
}

func TestDetectServicesAgainstMeasuredProfiles(t *testing.T) {
	profiles := measuredProfiles()
	sampler := &fakeSampler{profiles: profiles}

	addrs := make([]string, 0, len(profiles))
	for a := range profiles {
		addrs = append(addrs, a)
	}

	judged, labels, err := DetectServices(context.Background(), sampler, testCfg(t), "tron", addrs)
	if err != nil {
		t.Fatal(err)
	}
	if len(judged) != len(profiles) {
		t.Fatalf("judged %d candidates, want %d", len(judged), len(profiles))
	}

	accepted := map[string]bool{}
	for _, c := range judged {
		if c.Accepted {
			accepted[c.Address] = true
		}
	}

	if !accepted["TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM"] {
		t.Error("the 514-counterparty address should be detected as a service")
	}
	for _, a := range []string{
		"TAythDdKTZeNq6VnQ7o9cEvWRQGgRpPiKX",
		"TNwf8VBNCkg7Y1pgyzbHdWdekkamoqcrmL",
		"TR5e7yKoXkvodEtKdp5JmzgfKmsFdqAwu4",
	} {
		if accepted[a] {
			t.Errorf("%s was detected as a service; it is an ordinary address", a)
		}
	}

	if len(labels) != 1 {
		t.Fatalf("produced %d labels, want 1", len(labels))
	}
}

// The single most important property. Behaviour proves an address is a
// service; it does not say which. Labelling it `exchange` would move it from
// weight 15 to weight 2 and understate every user's exposure sevenfold.
func TestDetectedServicesAreNeverLabelledExchange(t *testing.T) {
	sampler := &fakeSampler{profiles: measuredProfiles()}
	_, labels, err := DetectServices(context.Background(), sampler, testCfg(t), "tron",
		[]string{"TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM"})
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) == 0 {
		t.Fatal("no labels produced")
	}
	for _, l := range labels {
		if l.Category != "unnamed_service" {
			t.Errorf("category = %q; behavioural detection must never claim to "+
				"identify the operator", l.Category)
		}
		if strings.Contains(strings.ToLower(l.Entity), "binance") ||
			strings.Contains(strings.ToLower(l.Entity), "exchange") {
			t.Errorf("entity %q names or implies an operator the evidence does not identify", l.Entity)
		}
		if l.Source != "derived:service" {
			t.Errorf("source = %q", l.Source)
		}
	}
}

// Every label must carry enough for a reviewer to re-run the measurement
// themselves rather than take it on trust.
func TestServiceEvidenceIsReproducible(t *testing.T) {
	sampler := &fakeSampler{profiles: measuredProfiles()}
	_, labels, err := DetectServices(context.Background(), sampler, testCfg(t), "tron",
		[]string{"TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM"})
	if err != nil {
		t.Fatal(err)
	}

	ev := labels[0].Evidence
	for _, k := range []string{"transfers_sampled", "distinct_counterparties",
		"history_exhausted", "thresholds", "reproduce"} {
		if _, ok := ev[k]; !ok {
			t.Errorf("evidence is missing %q", k)
		}
	}
	if repro, _ := ev["reproduce"].(string); !strings.Contains(repro, "TZ8Ksz21") {
		t.Errorf("reproduce URL does not name the address: %v", ev["reproduce"])
	}
}

// A high distinct-per-transfer ratio on a tiny sample must not qualify.
// Small samples are trivially diverse; this is the case that rules out using
// the ratio on its own.
func TestSmallDiverseWalletIsNotAService(t *testing.T) {
	sampler := &fakeSampler{profiles: map[string]Sample{
		"TTiny": {Transfers: 39, Counterparties: 39, MorePages: false, PagesFetched: 1},
	}}
	judged, labels, err := DetectServices(context.Background(), sampler, testCfg(t), "tron",
		[]string{"TTiny"})
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 0 {
		t.Error("a 39-transfer wallet with a perfect ratio was labelled a service")
	}
	if judged[0].Rejected == "" {
		t.Error("rejection reason not recorded")
	}
}

// An address whose entire history fits inside the sample is not high-volume,
// however diverse it looks.
func TestExhaustedHistoryIsRejected(t *testing.T) {
	sampler := &fakeSampler{profiles: map[string]Sample{
		"TBorderline": {Transfers: 600, Counterparties: 514, MorePages: false, PagesFetched: 3},
	}}
	_, labels, err := DetectServices(context.Background(), sampler, testCfg(t), "tron",
		[]string{"TBorderline"})
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 0 {
		t.Error("an address with an exhausted history was labelled a service")
	}
}

// One unreachable address must not abandon the run; it costs coverage, not
// correctness.
func TestSamplingFailureDoesNotAbortTheRun(t *testing.T) {
	sampler := &fakeSampler{
		profiles: measuredProfiles(),
		err:      map[string]error{"TBroken": fmt.Errorf("rate limited")},
	}
	judged, labels, err := DetectServices(context.Background(), sampler, testCfg(t), "tron",
		[]string{"TBroken", "TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM"})
	if err != nil {
		t.Fatalf("a single sampling failure aborted the run: %v", err)
	}
	if len(labels) != 1 {
		t.Errorf("produced %d labels; the reachable address should still be judged", len(labels))
	}

	var sawFailure bool
	for _, c := range judged {
		if c.Address == "TBroken" && strings.Contains(c.Rejected, "could not sample") {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Error("the sampling failure was not recorded")
	}
}

// SPEC.md §2: identical input produces identical output.
func TestDetectionIsDeterministic(t *testing.T) {
	profiles := measuredProfiles()
	addrs := []string{}
	for a := range profiles {
		addrs = append(addrs, a)
	}

	var first string
	for run := 0; run < 10; run++ {
		// Shuffle the input order between runs.
		shuffled := append([]string(nil), addrs...)
		if run%2 == 1 {
			for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
				shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
			}
		}

		_, labels, err := DetectServices(context.Background(),
			&fakeSampler{profiles: profiles}, testCfg(t), "tron", shuffled)
		if err != nil {
			t.Fatal(err)
		}

		var b strings.Builder
		for _, l := range labels {
			fmt.Fprintf(&b, "%s/%s/%.2f;", l.Address, l.Category, l.Confidence)
		}
		if run == 0 {
			first = b.String()
			continue
		}
		if b.String() != first {
			t.Fatalf("run %d differs: %q vs %q", run, b.String(), first)
		}
	}
}

// A detector with unset thresholds would label every address it saw. That must
// fail loudly rather than quietly flooding the label set.
func TestUnsetThresholdsAreRefused(t *testing.T) {
	cfg := testCfg(t)
	cfg.Weights.DerivedService.MinCounterparties = 0

	_, _, err := DetectServices(context.Background(), &fakeSampler{}, cfg, "tron", []string{"TA"})
	if err == nil {
		t.Fatal("an unbounded detector must be refused")
	}
	if !strings.Contains(err.Error(), "every address") {
		t.Errorf("error should explain the danger: %v", err)
	}
}

func TestDisabledDetectorProducesNothing(t *testing.T) {
	cfg := testCfg(t)
	cfg.Weights.DerivedService.Enabled = false

	sampler := &fakeSampler{profiles: measuredProfiles()}
	_, labels, err := DetectServices(context.Background(), sampler, cfg, "tron",
		[]string{"TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM"})
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 0 {
		t.Error("a disabled detector produced labels")
	}
	if sampler.calls != 0 {
		t.Error("a disabled detector spent API calls")
	}
}

// Live check of the real TronGrid sampler against the address whose measured
// profile the thresholds were calibrated on. Skipped unless TRON_LIVE_SAMPLE
// is set, so the suite stays runnable offline.
//
// A fixture cannot catch TronGrid changing its response shape or an address's
// behaviour changing; this can.
func TestLiveTronSampler(t *testing.T) {
	if os.Getenv("TRON_LIVE_SAMPLE") == "" {
		t.Skip("set TRON_LIVE_SAMPLE=1 to run the live sampler check")
	}

	sampler := NewTronSampler("https://api.trongrid.io", os.Getenv("TRONGRID_API_KEY"), 3)
	cfg := testCfg(t)

	cases := []struct {
		address     string
		wantService bool
		note        string
	}{
		{"TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM", true, "high volume, broad counterparties"},
		{"TNwf8VBNCkg7Y1pgyzbHdWdekkamoqcrmL", false, "39 transfers, receive-only"},
	}

	for _, tc := range cases {
		s, err := sampler.Sample(context.Background(), tc.address,
			cfg.Weights.DerivedService.SamplePages, cfg.Weights.DerivedService.PageSize)
		if err != nil {
			t.Fatalf("%s: sample failed: %v", tc.address, err)
		}
		t.Logf("%s  transfers=%d counterparties=%d more=%v  # %s",
			tc.address, s.Transfers, s.Counterparties, s.MorePages, tc.note)

		judged, labels, err := DetectServices(context.Background(),
			&fixedSampler{s}, cfg, "tron", []string{tc.address})
		if err != nil {
			t.Fatal(err)
		}
		got := len(labels) == 1
		if got != tc.wantService {
			t.Errorf("%s detected as service = %v, want %v (%s)",
				tc.address, got, tc.wantService, judged[0].Rejected)
		}
	}
}

// fixedSampler replays one already-taken sample, so the live check measures
// once and judges without spending a second set of API calls.
type fixedSampler struct{ s Sample }

func (f *fixedSampler) Sample(context.Context, string, int, int) (Sample, error) {
	return f.s, nil
}
