package billing

import (
	"path/filepath"
	"testing"
	"time"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

var t0 = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

func day(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func TestConfigLoads(t *testing.T) {
	cfg := testConfig(t)
	basic, ok := cfg.Plan("basic")
	if !ok || basic.Rank != 1 || basic.PriceMicroUSDT != 10_000_000 {
		t.Fatalf("basic plan = %+v", basic)
	}
	pro, _ := cfg.Plan("pro")
	if pro.Rank <= basic.Rank {
		t.Error("pro must outrank basic")
	}
}

func TestConfigRejectsBadPrices(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan Plan
	}{
		{"stars over the Bot API ceiling", Plan{ID: "x", Name: "X", DailyScreens: 1, PriceStars: 10001, PriceUSDT: "1"}},
		{"usdt with cents beyond two decimals", Plan{ID: "x", Name: "X", DailyScreens: 1, PriceStars: 1, PriceUSDT: "9.995"}},
		{"no daily screens", Plan{ID: "x", Name: "X", PriceStars: 1, PriceUSDT: "1"}},
	} {
		c := Config{Plans: []Plan{tc.plan}, PeriodDays: 30}
		c.USDT.InvoiceTTL, c.USDT.Poll = time.Hour, time.Minute
		c.Monitor.Interval, c.Monitor.PerPass = time.Hour, 1
		c.Deepen.Poll, c.Deepen.RoundWait, c.Deepen.MaxRounds, c.Deepen.MaxDuration = time.Second, time.Second, 1, time.Minute
		if err := c.init(); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestEntitlePicksHighestActivePlan(t *testing.T) {
	cfg := testConfig(t)
	subs := []Subscription{
		{Plan: "basic", Source: "trial", StartsAt: t0.Add(-day(1)), ExpiresAt: t0.Add(day(6))},
		{Plan: "pro", Source: "usdt", StartsAt: t0.Add(-day(2)), ExpiresAt: t0.Add(day(3))},
		{Plan: "pro", Source: "grant", StartsAt: t0.Add(-day(60)), ExpiresAt: t0.Add(-day(30))}, // expired
	}
	a := Entitle(cfg, subs, t0)
	if a.Plan == nil || a.Plan.ID != "pro" || !a.Until.Equal(t0.Add(day(3))) {
		t.Fatalf("got %+v", a)
	}
	// When pro ends, basic from the trial remains.
	if a := Entitle(cfg, subs, t0.Add(day(4))); a.Plan == nil || a.Plan.ID != "basic" {
		t.Fatalf("after pro expires: %+v", a)
	}
	if a := Entitle(cfg, subs, t0.Add(day(7))); a.Plan != nil {
		t.Fatalf("after everything expires: %+v", a)
	}
}

func TestEntitleJoinsBackToBackPeriods(t *testing.T) {
	cfg := testConfig(t)
	subs := []Subscription{
		{Plan: "basic", Source: "usdt", StartsAt: t0.Add(-day(10)), ExpiresAt: t0.Add(day(20))},
		{Plan: "basic", Source: "usdt", StartsAt: t0.Add(day(20)), ExpiresAt: t0.Add(day(50))},
	}
	if a := Entitle(cfg, subs, t0); !a.Until.Equal(t0.Add(day(50))) {
		t.Fatalf("until = %v, want the end of the prepaid period", a.Until)
	}
}

func TestEntitleIgnoresRevokedAndReportsRenewal(t *testing.T) {
	cfg := testConfig(t)
	now := t0
	subs := []Subscription{
		{Plan: "pro", Source: "stars", IsRecurring: true, StartsAt: t0, ExpiresAt: t0.Add(day(30)), RevokedAt: &now},
		{Plan: "basic", Source: "stars", IsRecurring: true, StartsAt: t0, ExpiresAt: t0.Add(day(30))},
	}
	a := Entitle(cfg, subs, t0.Add(time.Hour))
	if a.Plan.ID != "basic" || !a.Renews {
		t.Fatalf("got %+v, want renewing basic", a)
	}
	subs[1].RenewalCanceledAt = &now
	if a := Entitle(cfg, subs, t0.Add(time.Hour)); a.Renews {
		t.Error("a cancelled subscription must not report renewal")
	}
}

func TestPaidPeriodStartNeverLosesDays(t *testing.T) {
	cfg := testConfig(t)
	subs := []Subscription{{Plan: "pro", Source: "usdt", StartsAt: t0.Add(-day(20)), ExpiresAt: t0.Add(day(10))}}
	if got := PaidPeriodStart(cfg, subs, "pro", t0); !got.Equal(t0.Add(day(10))) {
		t.Errorf("renewing pro early starts at %v, want the current end", got)
	}
	if got := PaidPeriodStart(cfg, subs, "basic", t0); !got.Equal(t0) {
		t.Errorf("a different plan starts at %v, want now", got)
	}
}

func TestPickAmountAvoidsTakenAmounts(t *testing.T) {
	price := int64(10_000_000)
	a, err := PickAmount(price, nil, 0)
	if err != nil || a != 10_010_000 {
		t.Fatalf("first amount = %d, %v; want 10.01", a, err)
	}
	taken := map[int64]bool{10_010_000: true, 10_020_000: true}
	if a, _ := PickAmount(price, taken, 0); a != 10_030_000 {
		t.Errorf("got %s, want 10.03", FormatUSDT(a))
	}
	all := map[int64]bool{}
	for c := int64(1); c <= 99; c++ {
		all[price+c*10_000] = true
	}
	if _, err := PickAmount(price, all, 7); err != ErrNoAmountFree {
		t.Errorf("all amounts taken: got %v, want ErrNoAmountFree", err)
	}
}

func TestMatchRequiresExactAmountInWindow(t *testing.T) {
	inv := Invoice{ID: 1, Amount: 10_370_000, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}
	open := []Invoice{inv}
	grace := day(1)

	pay := func(amount int64, at time.Time) Payment { return Payment{Tx: "tx", Amount: amount, BlockTime: at} }

	if _, ok := Match(pay(10_370_000, t0.Add(10*time.Minute)), open, grace); !ok {
		t.Error("exact amount in time: not matched")
	}
	if _, ok := Match(pay(10_370_000, t0.Add(20*time.Hour)), open, grace); !ok {
		t.Error("late but within grace: not matched")
	}
	if _, ok := Match(pay(10_370_000, t0.Add(26*time.Hour)), open, grace); ok {
		t.Error("after grace: matched")
	}
	if _, ok := Match(pay(10_360_000, t0.Add(time.Minute)), open, grace); ok {
		t.Error("wrong amount: matched")
	}
	if _, ok := Match(pay(10_370_000, t0.Add(-time.Minute)), open, grace); ok {
		t.Error("paid before the invoice existed: matched")
	}
}

func TestFormatUSDT(t *testing.T) {
	if got := FormatUSDT(10_370_000); got != "10.37" {
		t.Errorf("got %q", got)
	}
}
