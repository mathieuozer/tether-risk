package tron

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A real TronGrid events response: the counterfeit-USDT transfer from
// docs/DECISIONS.md D18.
const counterfeitEvents = `{"data":[{"block_number":50403286,"block_timestamp":1681823094000,"caller_contract_address":"THk5qH79SoAaUnUh8JVdRarSESTZpqPjSQ","contract_address":"THk5qH79SoAaUnUh8JVdRarSESTZpqPjSQ","event_index":0,"event_name":"Transfer","result":{"0":"0x602bfbc1bf6788818e14f9108bc189b40e9ba704","1":"0x5714d9a92682a8f3b05a189d2be0a6421882e149","2":"10350355963000000000000","from":"0x602bfbc1bf6788818e14f9108bc189b40e9ba704","to":"0x5714d9a92682a8f3b05a189d2be0a6421882e149","value":"10350355963000000000000"},"result_type":{"from":"address","to":"address","value":"uint256"},"event":"Transfer(address indexed from, address indexed to, uint256 value)","transaction_id":"ce39232e160a507848c4a4a475d5394462f305d8cd74a71cb640e40feb46cc61"}],"success":true,"meta":{"at":1789995575741,"page_size":1}}`

func traceClient(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(Options{BaseURL: srv.URL, RequestsPerSecond: 1000})
}

func TestTransferEventsDecodesAndIdentifiesAsset(t *testing.T) {
	c := traceClient(t, counterfeitEvents)
	got, err := c.TransferEvents(context.Background(),
		"ce39232e160a507848c4a4a475d5394462f305d8cd74a71cb640e40feb46cc61")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d movements, want 1", len(got))
	}
	m := got[0]
	for _, a := range []string{m.From, m.To} {
		if !IsValid(a) || !strings.HasPrefix(a, "T") {
			t.Errorf("address %q is not valid base58", a)
		}
	}
	// The counterfeit must be identified by its contract, never as USDT.
	if m.Asset != "THk5qH79SoAaUnUh8JVdRarSESTZpqPjSQ" || IsCanonicalToken(m.Contract) {
		t.Errorf("asset = %q, canonical = %v; a counterfeit must not pass as USDT",
			m.Asset, IsCanonicalToken(m.Contract))
	}
	if m.Value.String() != "10350355963000000000000" {
		t.Errorf("value = %s", m.Value)
	}
}

// A reverted transaction emits nothing. That must surface as not found, not
// as a successful trace of zero transfers.
func TestTransferEventsReportsEmptyAsNotFound(t *testing.T) {
	c := traceClient(t, `{"data":[],"success":true,"meta":{}}`)
	_, err := c.TransferEvents(context.Background(), strings.Repeat("a", 64))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestTransferEventsRejectsMalformedID(t *testing.T) {
	c := traceClient(t, counterfeitEvents)
	if _, err := c.TransferEvents(context.Background(), "not-a-txid"); err == nil {
		t.Fatal("expected an error for a malformed transaction id")
	}
}

// Only real USDT sent to the payment address counts as a payment. A
// counterfeit token, or a transfer to another address the API returned
// anyway, must never come back from Inbound.
func TestInboundKeepsOnlyRealUSDTToTheAddress(t *testing.T) {
	const pay = "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7"
	item := func(tx, to, contract, value string) string {
		return `{"transaction_id":"` + tx + `","block_timestamp":1790000000000,"from":"TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM",` +
			`"to":"` + to + `","type":"Transfer","value":"` + value + `","token_info":{"address":"` + contract + `","symbol":"USDT","decimals":6}}`
	}
	body := `{"success":true,"meta":{},"data":[` +
		item("real", pay, USDTContract, "10370000") + `,` +
		item("fake", pay, "THk5qH79SoAaUnUh8JVdRarSESTZpqPjSQ", "10370000") + `,` +
		item("elsewhere", "TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM", USDTContract, "10370000") + `]}`

	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(Options{BaseURL: srv.URL, RequestsPerSecond: 1000})

	got, err := c.Inbound(context.Background(), pay, USDTContract, time.UnixMilli(1789999999000))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TxID != "real" || got[0].Value.String() != "10370000" {
		t.Fatalf("got %+v, want only the real USDT transfer", got)
	}
	for _, want := range []string{"only_confirmed=true", "only_to=true", "contract_address=" + USDTContract, "min_timestamp=1789999999000"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q lacks %q", query, want)
		}
	}
}
