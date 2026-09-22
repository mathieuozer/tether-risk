package labels

import (
	"context"
	"strings"
	"testing"
)

// Rows taken from the published HTX file, including the summary rows and the
// headers that are interleaved with address rows.
const porSample = "\xef\xbb\xbfcoin,snapshot height,balance\n" +
	"TRX(All),-,9384209885.72\n" +
	"coin,address,snapshot height,balance,message,signature\n" +
	"BTC-TRC20,TDToUxX8sH4z6moQpK3ZLAN24eupu2ivA4,85840149,10298.93,huobi,8fa149\n" +
	"TRX,TDToUxX8sH4z6moQpK3ZLAN24eupu2ivA4,85840149,5.00,huobi,8fa149\n" +
	"BTC-TRC20,TK86Qm97uM848dMk8G7xNbJB7zG1uW3h1n,85840149,10.00,-,-\n" +
	"BTC-SOL,Cw32Ny2xcYdpfJmvFd7h2pRhcbcjoJFq8KrrA8YLwZeh,443191405,17.00,-,-\n" +
	"ETH,0x18709e89bd403f470088abdacebe86cc60dda12e,20000000,1.00,huobi,0xab\n" +
	"TRX,T0000000000000000000000000000000xx,85840149,1.00,huobi,0xab\n"

func TestParsePoRCSVKeepsTronAndMergesCoins(t *testing.T) {
	res, got, err := ParsePoRCSV(context.Background(), strings.NewReader(porSample),
		"HTX", "htx_por", "https://example.test/huobi_por.csv", 0.9)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d labels, want 2: %+v", len(got), got)
	}
	if res.Signed != 1 || len(res.Malformed) != 1 {
		t.Errorf("signed = %d, malformed = %v; want 1 signed and the bad address reported", res.Signed, res.Malformed)
	}

	byAddr := map[string]Label{}
	for _, l := range got {
		byAddr[l.Address] = l
	}
	l, ok := byAddr["TDToUxX8sH4z6moQpK3ZLAN24eupu2ivA4"]
	if !ok {
		t.Fatal("signed HTX address missing")
	}
	if coins := l.Evidence["coins"].([]string); len(coins) != 2 {
		t.Errorf("coins = %v; an address listed twice must become one label naming both", coins)
	}
	if l.Evidence["ownership_signed"] != true {
		t.Error("signature not recorded")
	}
	if byAddr["TK86Qm97uM848dMk8G7xNbJB7zG1uW3h1n"].Evidence["ownership_signed"] != false {
		t.Error("an unsigned row must be recorded as unsigned")
	}
}

// A reserve list says who controls an address, not the exchange's KYC
// standard, so it must never assert the exchange category.
func TestParsePoRCSVDoesNotAssertExchangeTier(t *testing.T) {
	_, got, err := ParsePoRCSV(context.Background(), strings.NewReader(porSample),
		"HTX", "htx_por", "u", 0.9)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range got {
		if l.Category != "named_service" {
			t.Errorf("%s category = %s, want named_service", l.Address, l.Category)
		}
		if !strings.HasPrefix(l.Entity, "HTX") {
			t.Errorf("entity %q should name the exchange", l.Entity)
		}
	}
}
