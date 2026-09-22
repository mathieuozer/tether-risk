package tron

import (
	"context"
	"fmt"
	"math/big"
	"net/url"
	"time"
)

// Activation is the transaction that created an account on chain.
//
// A TRON account does not exist until something creates it: a TRX transfer
// to a new address, a TRC-10 transfer, or an explicit AccountCreateContract.
// Whoever did that paid for it, and operators create their own wallets from
// a small number of operations accounts, so wallets sharing an activator are
// often one operator's (docs/DECISIONS.md D31). It is first-party chain data:
// no list, no third party.
type Activation struct {
	Address   string
	Activator string
	TxID      string
	Type      string // TransferContract, TransferAssetContract, AccountCreateContract
	Amount    *big.Int
	Time      time.Time
}

type activationTx struct {
	TxID      string `json:"txID"`
	BlockTime int64  `json:"block_timestamp"`
	Ret       []struct {
		ContractRet string `json:"contractRet"`
	} `json:"ret"`
	RawData struct {
		Contract []struct {
			Type      string `json:"type"`
			Parameter struct {
				Value struct {
					Amount         *big.Int `json:"amount"`
					OwnerAddress   string   `json:"owner_address"`
					ToAddress      string   `json:"to_address"`
					AccountAddress string   `json:"account_address"`
				} `json:"value"`
			} `json:"parameter"`
		} `json:"contract"`
	} `json:"raw_data"`
}

// ActivationOf returns the transaction that created an account. It returns
// ErrNotFound when the account has no inbound transaction that could have
// created it, which is the case for an address that was never activated.
func (c *Client) ActivationOf(ctx context.Context, address string) (Activation, error) {
	addr, err := Normalise(address)
	if err != nil {
		return Activation{}, err
	}
	var resp struct {
		Data    []activationTx `json:"data"`
		Success bool           `json:"success"`
		Error   string         `json:"error"`
	}
	// Oldest first: the account's first inbound transaction created it.
	u := fmt.Sprintf("%s/v1/accounts/%s/transactions?only_to=true&only_confirmed=true&order_by=block_timestamp,asc&limit=5",
		c.baseURL, url.PathEscape(addr))
	if err := c.get(ctx, u, &resp); err != nil {
		return Activation{}, err
	}
	if !resp.Success {
		return Activation{}, fmt.Errorf("tron: activation of %s: %s", addr, resp.Error)
	}
	for _, tx := range resp.Data {
		if len(tx.Ret) > 0 && tx.Ret[0].ContractRet != "" && tx.Ret[0].ContractRet != "SUCCESS" {
			continue
		}
		for _, ct := range tx.RawData.Contract {
			v := ct.Parameter.Value
			target := v.ToAddress
			switch ct.Type {
			case "TransferContract", "TransferAssetContract":
			case "AccountCreateContract":
				target = v.AccountAddress
			default:
				continue
			}
			to, err := Normalise(target)
			if err != nil || to != addr {
				continue
			}
			from, err := Normalise(v.OwnerAddress)
			if err != nil {
				continue
			}
			amount := v.Amount
			if amount == nil {
				amount = new(big.Int)
			}
			return Activation{Address: addr, Activator: from, TxID: tx.TxID, Type: ct.Type,
				Amount: amount, Time: time.UnixMilli(tx.BlockTime).UTC()}, nil
		}
	}
	return Activation{}, ErrNotFound
}
