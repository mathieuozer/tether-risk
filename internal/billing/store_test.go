package billing

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/mozer/tether-risk/internal/store/storetest"
)

func testStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	pg := storetest.Postgres(t, "tether_risk_billing_test")
	for _, tbl := range []string{"usage_daily", "subscriptions", "usdt_invoices", "usdt_unmatched", "usdt_watch", "bot_users"} {
		if _, err := pg.Exec("TRUNCATE TABLE " + tbl + " CASCADE"); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
	return NewStore(pg, testConfig(t)), pg
}

func TestTrialIsGrantedOnce(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.Touch(ctx, User{ID: 1, Username: "alice"}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	granted := make(chan bool, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.StartTrial(ctx, 1, t0)
			if err != nil {
				t.Error(err)
			}
			granted <- ok
		}()
	}
	wg.Wait()
	close(granted)
	n := 0
	for ok := range granted {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("trial granted %d times, want once", n)
	}
	a, _ := s.Access(ctx, 1, t0.Add(time.Hour))
	if a.Plan == nil || a.Plan.ID != "basic" || a.Source != "trial" || !a.Until.Equal(t0.Add(day(7))) {
		t.Fatalf("access = %+v", a)
	}
}

func TestDailyLimitHoldsUnderConcurrency(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	s.Touch(ctx, User{ID: 2})

	var wg sync.WaitGroup
	allowed := make(chan bool, 30)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := s.Consume(ctx, 2, t0, 20)
			if err != nil {
				t.Error(err)
			}
			allowed <- ok
		}()
	}
	wg.Wait()
	close(allowed)
	n := 0
	for ok := range allowed {
		if ok {
			n++
		}
	}
	if n != 20 {
		t.Fatalf("%d screens allowed, want exactly 20", n)
	}
	if err := s.Refund(ctx, 2, t0); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Consume(ctx, 2, t0, 20); !ok {
		t.Error("a refunded screen should be usable again")
	}
	if _, ok, _ := s.Consume(ctx, 2, t0.Add(day(1)), 20); !ok {
		t.Error("a new day should reset the limit")
	}
}

func TestStarsPaymentIsRecordedOnce(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	s.Touch(ctx, User{ID: 3})

	p := StarsPayment{UserID: 3, Plan: "pro", Amount: 3800, ChargeID: "ch-1", Recurring: true,
		ExpiresAt: t0.Add(day(30)), ReceivedAt: t0}
	if ok, err := s.RecordStars(ctx, p); !ok || err != nil {
		t.Fatalf("first delivery: %v %v", ok, err)
	}
	if ok, err := s.RecordStars(ctx, p); ok || err != nil {
		t.Fatalf("duplicate delivery: recorded=%v err=%v", ok, err)
	}
	a, _ := s.Access(ctx, 3, t0.Add(time.Hour))
	if a.Plan.ID != "pro" || !a.Renews {
		t.Fatalf("access = %+v", a)
	}

	// A refund ends access at once.
	if uid, err := s.RevokeCharge(ctx, "ch-1", "refunded", t0.Add(2*time.Hour)); uid != 3 || err != nil {
		t.Fatalf("revoke: %d %v", uid, err)
	}
	if a, _ := s.Access(ctx, 3, t0.Add(3*time.Hour)); a.Plan != nil {
		t.Fatalf("after refund: %+v", a)
	}
}

func TestUSDTInvoiceSettlesOnceAndExtends(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	s.Touch(ctx, User{ID: 4})
	s.Touch(ctx, User{ID: 5})
	const addr = "TPayHere"

	inv, err := s.CreateInvoice(ctx, 4, "basic", addr, t0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Asking again returns the same invoice.
	again, _ := s.CreateInvoice(ctx, 4, "basic", addr, t0.Add(time.Minute), 0)
	if again.ID != inv.ID {
		t.Errorf("second request issued invoice %d, want %d again", again.ID, inv.ID)
	}
	// Another customer never gets the same amount.
	other, _ := s.CreateInvoice(ctx, 5, "basic", addr, t0.Add(time.Minute), 0)
	if other.Amount == inv.Amount {
		t.Fatalf("two open invoices share amount %s", FormatUSDT(inv.Amount))
	}

	open, _ := s.OpenInvoices(ctx, addr, t0.Add(5*time.Minute))
	pay := Payment{Tx: "tx-1", From: "TCustomer", Amount: inv.Amount, BlockTime: t0.Add(5 * time.Minute)}
	m, ok := Match(pay, open, s.cfg.USDT.Grace)
	if !ok || m.ID != inv.ID {
		t.Fatalf("match = %+v %v", m, ok)
	}
	until, ok, err := s.Settle(ctx, m, pay, t0.Add(6*time.Minute))
	if !ok || err != nil {
		t.Fatalf("settle: %v %v", ok, err)
	}
	if want := t0.Add(6 * time.Minute).Add(day(30)); !until.Equal(want) {
		t.Errorf("until = %v, want %v", until, want)
	}
	if _, ok, _ := s.Settle(ctx, m, pay, t0.Add(7*time.Minute)); ok {
		t.Fatal("the same payment settled twice")
	}
	if known, _ := s.KnownTx(ctx, "tx-1"); !known {
		t.Error("settled transaction not known")
	}

	// Paying again before expiry continues from the current end.
	inv2, _ := s.CreateInvoice(ctx, 4, "basic", addr, t0.Add(day(1)), 0)
	pay2 := Payment{Tx: "tx-2", Amount: inv2.Amount, BlockTime: t0.Add(day(1))}
	until2, ok, _ := s.Settle(ctx, inv2, pay2, t0.Add(day(1)))
	if !ok || !until2.Equal(until.Add(day(30))) {
		t.Fatalf("renewal until = %v, want %v", until2, until.Add(day(30)))
	}
}

func TestGrantAndRevoke(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if err := s.EnsureUser(ctx, 6); err != nil {
		t.Fatal(err)
	}
	until, err := s.Grant(ctx, 6, "pro", 14, "tester", t0)
	if err != nil || !until.Equal(t0.Add(day(14))) {
		t.Fatalf("grant: %v %v", until, err)
	}
	if n, err := s.Revoke(ctx, 6, "done", t0.Add(day(1))); n != 1 || err != nil {
		t.Fatalf("revoke: %d %v", n, err)
	}
	if a, _ := s.Access(ctx, 6, t0.Add(day(2))); a.Plan != nil {
		t.Fatalf("after revoke: %+v", a)
	}
}

func TestFindUserByNameOrID(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	s.Touch(ctx, User{ID: 77, Username: "Bob"})
	for _, ref := range []string{"77", "@bob", "Bob"} {
		if id, err := s.FindUser(ctx, ref); id != 77 || err != nil {
			t.Errorf("FindUser(%q) = %d, %v", ref, id, err)
		}
	}
	if _, err := s.FindUser(ctx, "@nobody"); err == nil {
		t.Error("unknown user found")
	}
}
