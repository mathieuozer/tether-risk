package tron

import (
	"context"
	"fmt"
	"math/big"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Tracing a single controlled transfer.
//
// The way to establish an exchange hot wallet from first-hand evidence is a
// test transfer: withdraw from exchange A to your own deposit address at
// exchange B. The withdrawal's sender is A's hot wallet. B then sweeps the
// deposit address into its own collection wallet, usually after topping it up
// with TRX to pay for the sweep. Every one of those addresses is established
// by a transaction the tester made themselves, with no third-party list
// involved (docs/DECISIONS.md D19).

// Movement is one value transfer observed while tracing.
type Movement struct {
	TxID     string
	Time     time.Time
	From     string
	To       string
	Contract string // empty for native TRX
	Asset    string // canonical asset name, or the contract address
	Value    *big.Int
}

// Native reports whether the movement is a native TRX transfer.
func (m Movement) Native() bool { return m.Contract == "" }

type eventsResponse struct {
	Data []struct {
		BlockTime       int64  `json:"block_timestamp"`
		ContractAddress string `json:"contract_address"`
		EventName       string `json:"event_name"`
		Result          struct {
			From  string `json:"from"`
			To    string `json:"to"`
			Value string `json:"value"`
		} `json:"result"`
	} `json:"data"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// TransferEvents returns the TRC-20 transfers a transaction emitted.
//
// A reverted transaction emits no events, so an empty result means nothing
// moved, which is reported as ErrNotFound rather than as an empty success.
func (c *Client) TransferEvents(ctx context.Context, txID string) ([]Movement, error) {
	txID = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(txID)), "0x")
	if len(txID) != 64 {
		return nil, fmt.Errorf("tron: %q is not a 64-character transaction id", txID)
	}

	var resp eventsResponse
	u := fmt.Sprintf("%s/v1/transactions/%s/events?only_confirmed=true", c.baseURL, txID)
	if err := c.get(ctx, u, &resp); err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("tron: events for %s: %s", txID, resp.Error)
	}

	var out []Movement
	for _, e := range resp.Data {
		if e.EventName != "Transfer" {
			continue
		}
		contract, err := Normalise(e.ContractAddress)
		if err != nil {
			return nil, fmt.Errorf("tron: event contract: %w", err)
		}
		from, err := eventAddress(e.Result.From)
		if err != nil {
			return nil, err
		}
		to, err := eventAddress(e.Result.To)
		if err != nil {
			return nil, err
		}
		v, ok := new(big.Int).SetString(e.Result.Value, 10)
		if !ok {
			return nil, fmt.Errorf("tron: event value %q is not an integer", e.Result.Value)
		}
		out = append(out, Movement{
			TxID: txID, Time: time.UnixMilli(e.BlockTime).UTC(),
			From: from, To: to, Contract: contract, Asset: assetForContract(contract), Value: v,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: transaction %s emitted no confirmed TRC-20 transfer", ErrNotFound, txID)
	}
	return out, nil
}

// Outbound returns an address's TRC-20 transfers out, at or after since.
// These are the sweep candidates for a deposit address.
func (c *Client) Outbound(ctx context.Context, address string, since time.Time) ([]Movement, error) {
	var resp trc20Response
	u := fmt.Sprintf("%s/v1/accounts/%s/transactions/trc20?only_confirmed=true&only_from=true&limit=200&order_by=block_timestamp,asc&min_timestamp=%d",
		c.baseURL, url.PathEscape(address), since.UnixMilli())
	if err := c.get(ctx, u, &resp); err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("tron: trc20 history for %s: %s", address, resp.Error)
	}

	var out []Movement
	for _, it := range resp.Data {
		if !strings.EqualFold(it.Type, "Transfer") {
			continue
		}
		contract, err := Normalise(it.TokenInfo.Address)
		if err != nil {
			continue
		}
		v, ok := new(big.Int).SetString(it.Value, 10)
		if !ok {
			continue
		}
		out = append(out, Movement{
			TxID: it.TransactionID, Time: time.UnixMilli(it.BlockTime).UTC(),
			From: it.From, To: it.To, Contract: contract, Asset: assetForContract(contract), Value: v,
		})
	}
	sortMovements(out)
	return out, nil
}

// NativeInbound returns successful TRX transfers into an address at or after
// since. For a deposit address these are the exchange's fee top-ups.
func (c *Client) NativeInbound(ctx context.Context, address string, since time.Time) ([]Movement, error) {
	var resp nativeResponse
	u := fmt.Sprintf("%s/v1/accounts/%s/transactions?only_confirmed=true&only_to=true&limit=200&order_by=block_timestamp,asc&min_timestamp=%d",
		c.baseURL, url.PathEscape(address), since.UnixMilli())
	if err := c.get(ctx, u, &resp); err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("tron: history for %s: %s", address, resp.Error)
	}

	var out []Movement
	for _, tx := range resp.Data {
		if len(tx.Ret) > 0 && tx.Ret[0].ContractRet != "" && tx.Ret[0].ContractRet != "SUCCESS" {
			continue
		}
		for _, ct := range tx.RawData.Contract {
			if ct.Type != "TransferContract" {
				continue
			}
			from, err := Normalise(ct.Parameter.Value.OwnerAddress)
			if err != nil {
				continue
			}
			to, err := Normalise(ct.Parameter.Value.ToAddress)
			if err != nil || to != address {
				continue
			}
			v, ok := new(big.Int).SetString(ct.Parameter.Value.Amount.String(), 10)
			if !ok {
				continue
			}
			out = append(out, Movement{
				TxID: tx.TxID, Time: time.UnixMilli(tx.BlockTime).UTC(),
				From: from, To: to, Asset: "TRX", Value: v,
			})
		}
	}
	sortMovements(out)
	return out, nil
}

// eventAddress converts an event log address to base58. Event results carry
// the 20-byte address without TRON's 0x41 prefix.
func eventAddress(h string) (string, error) {
	h = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(h)), "0x")
	if len(h) == 40 {
		h = "41" + h
	}
	addr, err := HexToBase58(h)
	if err != nil {
		return "", fmt.Errorf("tron: event address: %w", err)
	}
	return addr, nil
}

// IsCanonicalToken reports whether a contract is one of the recognised
// stablecoins rather than an arbitrary or counterfeit token.
func IsCanonicalToken(contract string) bool {
	_, ok := canonicalTokens[contract]
	return ok
}

func sortMovements(m []Movement) {
	sort.SliceStable(m, func(i, j int) bool {
		if !m[i].Time.Equal(m[j].Time) {
			return m[i].Time.Before(m[j].Time)
		}
		return m[i].TxID < m[j].TxID
	})
}
