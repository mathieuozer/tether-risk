package pricing

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/config"
)

type fakeTron struct {
	token  string // hex of the pool's token, as tokenAddress() returns it
	events []tron.ContractEvent
}

func (f fakeTron) ConstantCall(context.Context, string, string) (string, error) { return f.token, nil }

func (f fakeTron) LastEventBefore(_ context.Context, _, _ string, before time.Time) (*tron.ContractEvent, error) {
	var last *tron.ContractEvent
	for i := range f.events {
		if f.events[i].Time.Before(before) {
			last = &f.events[i]
		}
	}
	return last, nil
}

// USDT's contract, TR7NHq…Lj6t, as 20 hex bytes.
const usdtHex = "000000000000000000000000a614f803b6fd780986a42c78ec9c7f77e6ded13c"

func TestTronClosesReadSnapshots(t *testing.T) {
	pool := config.PricePool{Asset: "TRX", Pool: "TPool", Quote: tron.USDTContract}
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	f := fakeTron{token: usdtHex, events: []tron.ContractEvent{
		{Time: day.Add(10 * time.Hour), Result: map[string]string{"trx_balance": "1000000000", "token_balance": "300000000"}},
		// The day's close is its last state: 0.34.
		{Time: day.Add(23 * time.Hour), Result: map[string]string{"trx_balance": "1000000000", "token_balance": "340000000"}},
	}}
	pts, err := TronCloses(context.Background(), f, pool, day, day.AddDate(0, 0, 5))
	if err != nil {
		t.Fatal(err)
	}
	// Day 1 closes at 0.34, day 2 reuses it (a day without trades), and the
	// days after are too stale to price.
	if len(pts) != 2 || pts[0].USD.String() != "0.34" || pts[1].Date != "2026-09-02" {
		t.Fatalf("points = %+v", pts)
	}

	f.token = strings.Repeat("0", 64)
	if _, err := TronCloses(context.Background(), f, pool, day, day.AddDate(0, 0, 1)); err == nil {
		t.Error("a pool trading another token priced anyway")
	}
}

// fakeEVM is a chain with one block every 12 s from the epoch and a pair
// whose reserves move by the day.
type fakeEVM struct {
	token0, token1 string
	reserves       func(day time.Time) (r0, r1 string)
	calls          int
}

func (f *fakeEVM) Latest(context.Context) (uint64, error) {
	return uint64(time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC).Unix() / 12), nil
}

func (f *fakeEVM) BlockTime(_ context.Context, n uint64) (time.Time, error) {
	f.calls++
	return time.Unix(int64(n)*12, 0).UTC(), nil
}

func (f *fakeEVM) CallAt(_ context.Context, to, data string, block uint64) (string, error) {
	pad := func(s string) string { return strings.Repeat("0", 64-len(s)) + s }
	switch data {
	case "0x0dfe1681":
		return "0x" + pad(f.token0[2:]), nil
	case "0xd21220a7":
		return "0x" + pad(f.token1[2:]), nil
	case "0x313ce567":
		if to == f.usdt() {
			return "0x" + pad("6"), nil
		}
		return "0x" + pad("12"), nil // 18
	case "0x0902f1ac":
		bt := time.Unix(int64(block)*12, 0).UTC()
		r0, r1 := f.reserves(bt)
		return "0x" + pad(r0) + pad(r1) + pad(fmt.Sprintf("%x", bt.Unix())), nil
	}
	return "0x", nil
}

func (f *fakeEVM) usdt() string { return "0x00000000000000000000000000000000000000aa" }

func TestEVMClosesReadReservesAtDayEnd(t *testing.T) {
	weth, usdt := "0x00000000000000000000000000000000000000bb", "0x00000000000000000000000000000000000000aa"
	pool := config.PricePool{Asset: "ETH", Pool: "0xpool", Base: weth, Quote: usdt}
	// token0 is USDT here, so the pair must be read the other way round.
	f := &fakeEVM{token0: usdt, token1: weth, reserves: func(bt time.Time) (string, string) {
		// 1 WETH = 1e18; the price is 2000 until Sept 2 23:59, then 2500.
		usd := int64(2000)
		if !bt.Before(time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)) {
			usd = 2500
		}
		return fmt.Sprintf("%x", usd*1_000_000), fmt.Sprintf("%x", int64(1e18))
	}}
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	pts, err := EVMCloses(context.Background(), f, pool, day, day.AddDate(0, 0, 3))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2000", "2000", "2500"}
	if len(pts) != 3 {
		t.Fatalf("points = %+v", pts)
	}
	for i, p := range pts {
		if p.USD.String() != want[i] {
			t.Errorf("%s = %s, want %s", p.Date, p.USD, want[i])
		}
	}

	f.token1 = "0x00000000000000000000000000000000000000cc"
	if _, err := EVMCloses(context.Background(), f, pool, day, day.AddDate(0, 0, 1)); err == nil {
		t.Error("a pair holding another token priced anyway")
	}
}
