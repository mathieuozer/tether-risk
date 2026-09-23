package tron

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/mozer/tether-risk/pkg/tronindex"
	"hash/fnv"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mozer/tether-risk/internal/chain"
)

// Adapter implements chain.Adapter for TRON via TronGrid.
type Adapter struct {
	client   *Client
	pageSize int
}

func NewAdapter(c *Client) *Adapter {
	return &Adapter{client: c, pageSize: 200} // 200 is TronGrid's documented maximum
}

func (a *Adapter) Chain() string { return "tron" }

// ---------------------------------------------------------------------------
// Response shapes, as returned by the live API
// ---------------------------------------------------------------------------

type meta struct {
	Fingerprint string `json:"fingerprint"`
	PageSize    int    `json:"page_size"`
	Links       struct {
		Next string `json:"next"`
	} `json:"links"`
}

// trc20Item is one TRC-20 transfer.
//
// Note what is absent: no log/event index and no block number. Both matter and
// are handled explicitly below; see docs/DECISIONS.md D11.
type trc20Item struct {
	TransactionID string `json:"transaction_id"`
	BlockTime     int64  `json:"block_timestamp"`
	From          string `json:"from"`
	To            string `json:"to"`
	Type          string `json:"type"`
	Value         string `json:"value"`
	TokenInfo     struct {
		Symbol   string `json:"symbol"`
		Address  string `json:"address"`
		Decimals int    `json:"decimals"`
		Name     string `json:"name"`
	} `json:"token_info"`
}

type trc20Response struct {
	Data    []trc20Item `json:"data"`
	Success bool        `json:"success"`
	Meta    meta        `json:"meta"`
	Error   string      `json:"error"`
}

type nativeTx struct {
	TxID        string `json:"txID"`
	BlockNumber uint64 `json:"blockNumber"`
	BlockTime   int64  `json:"block_timestamp"`
	Ret         []struct {
		ContractRet string `json:"contractRet"`
	} `json:"ret"`
	RawData struct {
		Contract []struct {
			Type      string `json:"type"`
			Parameter struct {
				Value struct {
					Amount       json.Number `json:"amount"`
					OwnerAddress string      `json:"owner_address"`
					ToAddress    string      `json:"to_address"`
				} `json:"value"`
			} `json:"parameter"`
		} `json:"contract"`
	} `json:"raw_data"`
}

type nativeResponse struct {
	Data    []nativeTx `json:"data"`
	Success bool       `json:"success"`
	Meta    meta       `json:"meta"`
	Error   string     `json:"error"`
}

// ---------------------------------------------------------------------------
// FetchAddress
// ---------------------------------------------------------------------------

// FetchAddress returns one page of an address's history.
//
// TRC-20 and native transfers come from two separate endpoints with
// independent pagination, so the cursor encodes both positions. USDT is the
// priority asset (SPEC.md §3), so TRC-20 is drained first and native follows.
func (a *Adapter) FetchAddress(ctx context.Context, address string, cur chain.Cursor) (chain.AddressPage, error) {
	addr, err := Normalise(address)
	if err != nil {
		return chain.AddressPage{}, fmt.Errorf("tron: %w", err)
	}
	if addr == "" {
		return chain.AddressPage{}, fmt.Errorf("tron: empty address")
	}

	state := decodeCursor(cur.Value)

	if !state.TRC20Done {
		page, err := a.fetchTRC20(ctx, addr, state)
		if err != nil {
			return chain.AddressPage{}, err
		}
		return page, nil
	}
	return a.fetchNative(ctx, addr, state)
}

func (a *Adapter) fetchTRC20(ctx context.Context, addr string, state cursorState) (chain.AddressPage, error) {
	u := state.TRC20Next
	if u == "" {
		u = fmt.Sprintf("%s/v1/accounts/%s/transactions/trc20?limit=%d&only_confirmed=true",
			a.client.baseURL, url.PathEscape(addr), a.pageSize)
	}

	var resp trc20Response
	if err := a.client.get(ctx, u, &resp); err != nil {
		return chain.AddressPage{}, fmt.Errorf("tron trc20 %s: %w", addr, err)
	}
	if !resp.Success && resp.Error != "" {
		return chain.AddressPage{}, fmt.Errorf("tron trc20 %s: %s", addr, resp.Error)
	}

	transfers := make([]chain.Transfer, 0, len(resp.Data))
	for _, it := range resp.Data {
		t, ok, err := a.toTransfer(it)
		if err != nil {
			return chain.AddressPage{}, err
		}
		if ok {
			transfers = append(transfers, t)
		}
	}

	next := state
	next.TRC20Next = resp.Meta.Links.Next
	if next.TRC20Next == "" {
		next.TRC20Done = true
	}

	return chain.AddressPage{
		Transfers: transfers,
		Next: chain.Cursor{
			Value: encodeCursor(next),
			Done:  next.TRC20Done && next.NativeDone,
		},
		PageKey: pageKey("trc20", addr, state.TRC20Next, resp.Meta.Fingerprint+"|"+chain.ContentKey(transfers)),
	}, nil
}

func (a *Adapter) fetchNative(ctx context.Context, addr string, state cursorState) (chain.AddressPage, error) {
	u := state.NativeNext
	if u == "" {
		u = fmt.Sprintf("%s/v1/accounts/%s/transactions?limit=%d&only_confirmed=true",
			a.client.baseURL, url.PathEscape(addr), a.pageSize)
	}

	var resp nativeResponse
	if err := a.client.get(ctx, u, &resp); err != nil {
		return chain.AddressPage{}, fmt.Errorf("tron native %s: %w", addr, err)
	}
	if !resp.Success && resp.Error != "" {
		return chain.AddressPage{}, fmt.Errorf("tron native %s: %s", addr, resp.Error)
	}

	var transfers []chain.Transfer
	for _, tx := range resp.Data {
		ts, err := a.nativeTransfers(tx)
		if err != nil {
			return chain.AddressPage{}, err
		}
		transfers = append(transfers, ts...)
	}

	next := state
	next.NativeNext = resp.Meta.Links.Next
	if next.NativeNext == "" {
		next.NativeDone = true
	}

	return chain.AddressPage{
		Transfers: transfers,
		Next: chain.Cursor{
			Value: encodeCursor(next),
			Done:  next.TRC20Done && next.NativeDone,
		},
		PageKey: pageKey("native", addr, state.NativeNext, resp.Meta.Fingerprint+"|"+chain.ContentKey(transfers)),
	}, nil
}

// toTransfer converts a TRC-20 item. The bool reports whether the item is a
// value transfer worth storing; approvals and other event types are not.
func (a *Adapter) toTransfer(it trc20Item) (chain.Transfer, bool, error) {
	if !strings.EqualFold(it.Type, "Transfer") {
		return chain.Transfer{}, false, nil
	}

	from, err := Normalise(it.From)
	if err != nil {
		return chain.Transfer{}, false, fmt.Errorf("trc20 %s from: %w", it.TransactionID, err)
	}
	to, err := Normalise(it.To)
	if err != nil {
		return chain.Transfer{}, false, fmt.Errorf("trc20 %s to: %w", it.TransactionID, err)
	}
	if from == "" || to == "" {
		return chain.Transfer{}, false, nil
	}

	value, ok := new(big.Int).SetString(it.Value, 10)
	if !ok {
		return chain.Transfer{}, false, fmt.Errorf("trc20 %s: value %q is not an integer", it.TransactionID, it.Value)
	}

	// TronGrid returns an empty token_info for a token it cannot resolve,
	// seen on 2021 transfers of value 1. With no contract the asset cannot
	// be identified, priced or told apart from any other token, so the item
	// is skipped. Failing on it instead failed the whole address, on every
	// retry, and left it permanently unfetched.
	if strings.TrimSpace(it.TokenInfo.Address) == "" {
		return chain.Transfer{}, false, nil
	}
	contract, err := Normalise(it.TokenInfo.Address)
	if err != nil || contract == "" {
		return chain.Transfer{}, false, fmt.Errorf("trc20 %s: invalid token contract %q", it.TransactionID, it.TokenInfo.Address)
	}
	asset := assetForContract(contract)

	return chain.Transfer{
		Chain:    "tron",
		TxHash:   it.TransactionID,
		LogIndex: syntheticLogIndex(it),
		// TronGrid's TRC-20 endpoint does not return a block number. Zero
		// means unknown. Nothing depends on it: the schema orders by
		// block_time, which this endpoint does provide.
		BlockNumber: 0,
		BlockTime:   time.UnixMilli(it.BlockTime).UTC(),
		FromAddress: from,
		ToAddress:   to,
		Asset:       asset,
		RawValue:    value,
		PriceBasis:  "unpriced", // Phase 3 prices this
	}, true, nil
}

// canonicalTokens maps the TRC-20 contracts we recognise to asset names.
//
// The asset must never come from token_info.symbol. Anyone who deploys a
// contract chooses its symbol, and "USDT" is the most counterfeited symbol on
// TRON. The pricer pins USDT to $1 and applies USDT's decimals by name, so a
// counterfeit named "USDT" gets real Tether's price, and at 18 declared
// decimals read as 6 its amounts are inflated by 10^12. That inflates the
// denominator of every exposure ratio and turns real mixer or sanctions exposure
// into a rounding error (docs/DECISIONS.md D18).
//
// Each entry was checked against Tronscan on 2026-09-21. Retired contracts
// stay listed because historical transfers still reference them.
var canonicalTokens = map[string]string{
	"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t": "USDT", // Tether USD
	"TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8": "USDC", // USD Coin, retired on TRON
	"TUpMhErZL2fhh4sVNULAbNKLokS4GjC1F4": "TUSD", // TrueUSD
	"TXDk8mbtRbXeYuMNS83CfKPaYYT8XWv9Hz": "USDD", // Decentralized USD
	"TPYmHEhy5n8TCEfYGqW2rPxsghSfzghPDn": "USDD", // Decentralized USD, first contract
}

// AssetForContract names the asset a TRC-20 contract carries, as the
// adapter does; the indexer names its rows the same way.
func AssetForContract(contract string) string { return assetForContract(contract) }

// CanonicalTokens lists the TRC-20 contracts we recognise, by contract.
func CanonicalTokens() map[string]string {
	out := make(map[string]string, len(canonicalTokens))
	for k, v := range canonicalTokens {
		out[k] = v
	}
	return out
}

// assetForContract names the asset a TRC-20 contract carries. Any
// contract not in canonicalTokens is identified by its own address, which
// the pricer does not recognise, so it stays unpriced. That is the same rule
// the EVM adapter follows.
func assetForContract(contract string) string {
	if asset, ok := canonicalTokens[contract]; ok {
		return asset
	}
	return contract
}

// syntheticLogIndex derives a stable index for a TRC-20 transfer.
//
// SPEC.md §4 deduplicates on (chain, tx_hash, log_index), but TronGrid's
// TRC-20 endpoint returns no event index. Using the position within the
// response page would not work: pagination can split a transaction's transfers
// across two pages, so the same transfer would get different indices on
// different fetches and be inserted twice — inflating `edges` exactly as
// docs/DECISIONS.md D2 describes.
//
// Hashing the transfer's identifying fields instead gives an index that is the
// same every time the same transfer is seen, from any page, in any order.
//
// Known limitation: two transfers in one transaction with identical sender,
// recipient, token and value hash to the same index and collapse into one.
// That is rare, and collapsing is the safer direction — it understates rather
// than invents flow — but it is a real undercount and is recorded in
// docs/METHODOLOGY.md.
func syntheticLogIndex(it trc20Item) uint32 {
	// One formula for rows from TronGrid and rows from our own index
	// (docs/INDEXER_PLAN.md), or the two would count a transfer twice.
	return tronindex.TronGridIndex(it.From, it.To, it.TokenInfo.Address, it.Value)
}

// nativeTransfers extracts TRX transfers from a native transaction. The index
// within raw_data.contract is a genuine log index, so no synthesis is needed.
func (a *Adapter) nativeTransfers(tx nativeTx) ([]chain.Transfer, error) {
	// A reverted transaction moved no value. Storing it would attribute
	// exposure to a transfer that never happened.
	if len(tx.Ret) > 0 && tx.Ret[0].ContractRet != "" && tx.Ret[0].ContractRet != "SUCCESS" {
		return nil, nil
	}

	var out []chain.Transfer
	for i, c := range tx.RawData.Contract {
		if c.Type != "TransferContract" {
			continue // TriggerSmartContract etc. surface via the TRC-20 endpoint
		}
		v := c.Parameter.Value

		amount, err := v.Amount.Int64()
		if err != nil || amount <= 0 {
			continue
		}
		from, err := Normalise(v.OwnerAddress)
		if err != nil {
			return nil, fmt.Errorf("native %s owner: %w", tx.TxID, err)
		}
		to, err := Normalise(v.ToAddress)
		if err != nil {
			return nil, fmt.Errorf("native %s to: %w", tx.TxID, err)
		}
		if from == "" || to == "" {
			continue
		}

		out = append(out, chain.Transfer{
			Chain:       "tron",
			TxHash:      tx.TxID,
			LogIndex:    uint32(i),
			BlockNumber: tx.BlockNumber,
			BlockTime:   time.UnixMilli(tx.BlockTime).UTC(),
			FromAddress: from,
			ToAddress:   to,
			Asset:       "TRX",
			RawValue:    big.NewInt(amount),
			PriceBasis:  "unpriced",
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Cursor
// ---------------------------------------------------------------------------

// cursorState tracks both endpoints' pagination independently.
type cursorState struct {
	TRC20Next  string `json:"trc20_next,omitempty"`
	TRC20Done  bool   `json:"trc20_done,omitempty"`
	NativeNext string `json:"native_next,omitempty"`
	NativeDone bool   `json:"native_done,omitempty"`
}

func encodeCursor(s cursorState) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeCursor(v string) cursorState {
	var s cursorState
	if v == "" {
		return s
	}
	// A malformed cursor restarts the address rather than failing the job.
	// Re-fetching is safe because ingestion deduplicates; giving up is not.
	_ = json.Unmarshal([]byte(v), &s)
	return s
}

// pageKey identifies a page of upstream data for the ingest ledger. It must be
// stable for the same page across retries, which is why it is built from the
// request position rather than the response contents.
func pageKey(endpoint, addr, next, fingerprint string) string {
	pos := next
	if pos == "" {
		pos = "start"
	}
	if fingerprint != "" {
		pos += "|" + fingerprint
	}
	h := fnv.New64a()
	h.Write([]byte(pos))
	return endpoint + ":" + addr + ":" + strconv.FormatUint(h.Sum64(), 16)
}

// ---------------------------------------------------------------------------
// Block-range access (SPEC.md §5), retained for optional backfill
// ---------------------------------------------------------------------------

// Head returns the current chain head block number.
func (a *Adapter) Head(ctx context.Context) (uint64, error) {
	var resp struct {
		BlockHeader struct {
			RawData struct {
				Number uint64 `json:"number"`
			} `json:"raw_data"`
		} `json:"block_header"`
	}
	if err := a.client.get(ctx, a.client.baseURL+"/wallet/getnowblock", &resp); err != nil {
		return 0, fmt.Errorf("tron head: %w", err)
	}
	return resp.BlockHeader.RawData.Number, nil
}

// FetchRange is part of chain.Adapter for full-chain backfill.
//
// TronGrid exposes no endpoint that returns all transfers in a block range;
// its public API is address- and contract-scoped. Implementing this would mean
// walking every block and every transaction in it, which the free tier cannot
// sustain. Ingestion is demand-driven (SPEC.md §5) and never calls this, so it
// returns an explicit error rather than silently returning nothing — an empty
// result would read as "this range had no transfers".
func (a *Adapter) FetchRange(ctx context.Context, from, to uint64) ([]chain.Transfer, error) {
	return nil, fmt.Errorf(
		"tron: block-range backfill is not supported on the TronGrid free tier "+
			"(requested %d-%d); ingestion is demand-driven per address", from, to)
}

// USDTWindowCursor starts a drain of address's USDT transfers between from
// and to only, newest first. A measurement that needs a few weeks of a large
// wallet's history reads one or two pages instead of the whole of it
// (docs/DECISIONS.md D34). The history is partial, so a caller must not mark
// the address fetched.
func (a *Adapter) USDTWindowCursor(address string, from, to time.Time) chain.Cursor {
	u := fmt.Sprintf("%s/v1/accounts/%s/transactions/trc20?limit=%d&only_confirmed=true&contract_address=%s&min_timestamp=%d&max_timestamp=%d",
		a.client.baseURL, url.PathEscape(address), a.pageSize, USDTContract, from.UnixMilli(), to.UnixMilli())
	return chain.Cursor{Value: encodeCursor(cursorState{TRC20Next: u, NativeDone: true})}
}
