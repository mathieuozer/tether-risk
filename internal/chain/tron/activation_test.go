package tron

import (
	"context"
	"errors"
	"testing"
)

// THasRe…geRM's first inbound transaction as TronGrid returned it
// (2026-09-22), trimmed to the fields read.
const activationBody = `{"data":[{"ret":[{"contractRet":"SUCCESS","fee":1100000}],"txID":"9400172719558b9dba23401393f1c96ec923ec7f923714caeb6d72e7c5b14b2b","block_timestamp":1733128917000,"raw_data":{"contract":[{"parameter":{"value":{"amount":479203688,"owner_address":"41047384117e5485ecf98288d957e16ee945b252b5","to_address":"4153878389083e159967c9c1c4ab10dc049923ebb2"},"type_url":"type.googleapis.com/protocol.TransferContract"},"type":"TransferContract"}]}}],"success":true}`

func TestActivationOfReadsTheCreatingTransfer(t *testing.T) {
	c := traceClient(t, activationBody)
	a, err := c.ActivationOf(context.Background(), "THasRePgRCvk8ZgAXf9cYUUpTqeAXGgeRM")
	if err != nil {
		t.Fatal(err)
	}
	if !IsValid(a.Activator) || a.Activator == a.Address {
		t.Errorf("activator %q", a.Activator)
	}
	if a.Type != "TransferContract" || a.Amount.String() != "479203688" || a.Time.Unix() != 1733128917 {
		t.Errorf("got %+v", a)
	}
}

// A failed transaction created nothing, and a transfer to someone else is
// not this account's activation.
func TestActivationOfSkipsFailedAndForeignTransactions(t *testing.T) {
	c := traceClient(t, `{"data":[{"ret":[{"contractRet":"REVERT"}],"txID":"aa","block_timestamp":1,"raw_data":{"contract":[{"type":"TransferContract","parameter":{"value":{"amount":1,"owner_address":"41047384117e5485ecf98288d957e16ee945b252b5","to_address":"4153878389083e159967c9c1c4ab10dc049923ebb2"}}}]}}],"success":true}`)
	if _, err := c.ActivationOf(context.Background(), "THasRePgRCvk8ZgAXf9cYUUpTqeAXGgeRM"); !errors.Is(err, ErrNotFound) {
		t.Errorf("failed transaction: err = %v, want ErrNotFound", err)
	}
}
