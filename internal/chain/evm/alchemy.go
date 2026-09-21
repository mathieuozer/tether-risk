package evm

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/mozer/tether-risk/internal/chain"
)

// Alchemy's alchemy_getAssetTransfers is a per-address transfer history API,
// which is exactly what SPEC.md §5's demand-driven ingestion needs and what
// raw JSON-RPC cannot provide (see the archive discussion in client.go).
//
// Two properties of the response shape drive the design here:
//
//  1. The API filters by `fromAddress` OR `toAddress`, never both at once, so
//     a complete history for one address takes two passes. The cursor encodes
//     both positions, as the TRON adapter does.
//
//  2. The `value` field is a JSON number — a float, already divided by the
//     token's decimals. For a USDT amount in the millions that silently loses
//     precision, and SPEC.md §2 requires a score be reconstructible from
//     stored data. So `rawContract.value` (an exact hex integer) is used
//     wherever present, and `value` only as a last resort with the loss
//     recorded.

// AlchemyAdapter implements chain.Adapter against Alchemy's enhanced API.
type AlchemyAdapter struct {
	client  *Client
	chainID string

	// categories passed to the API. "external" covers native value transfers
	// and "erc20" covers tokens; NFT categories are deliberately excluded
	// because an NFT transfer moves no fungible value and would distort the
	// haircut arithmetic.
	categories []string

	// pageSize is maxCount per request. Alchemy's default is 1000.
	pageSize int64
}

type AlchemyOptions struct {
	ChainID    string
	Categories []string
	PageSize   int64
}

func NewAlchemyAdapter(c *Client, opts AlchemyOptions) *AlchemyAdapter {
	if opts.ChainID == "" {
		opts.ChainID = "ethereum"
	}
	if len(opts.Categories) == 0 {
		opts.Categories = []string{"external", "erc20"}
	}
	if opts.PageSize <= 0 || opts.PageSize > 1000 {
		opts.PageSize = 1000
	}
	return &AlchemyAdapter{
		client:     c,
		chainID:    opts.ChainID,
		categories: opts.Categories,
		pageSize:   opts.PageSize,
	}
}

func (a *AlchemyAdapter) Chain() string { return a.chainID }

// Head returns the current head block.
func (a *AlchemyAdapter) Head(ctx context.Context) (uint64, error) {
	var hex string
	if err := a.client.call(ctx, "eth_blockNumber", []any{}, &hex); err != nil {
		return 0, fmt.Errorf("%s head: %w", a.chainID, err)
	}
	return hexToUint64(hex)
}

// assetTransfer mirrors one entry in the response.
type assetTransfer struct {
	BlockNum string  `json:"blockNum"`
	Hash     string  `json:"hash"`
	From     string  `json:"from"`
	To       string  `json:"to"`
	Value    float64 `json:"value"`
	Asset    string  `json:"asset"`
	Category string  `json:"category"`
	UniqueID string  `json:"uniqueId"`

	RawContract struct {
		Value   string `json:"value"`
		Address string `json:"address"`
		Decimal string `json:"decimal"`
	} `json:"rawContract"`

	Metadata struct {
		BlockTimestamp string `json:"blockTimestamp"`
	} `json:"metadata"`
}

type assetTransfersResult struct {
	PageKey   string          `json:"pageKey"`
	Transfers []assetTransfer `json:"transfers"`
}

// alchemyCursor tracks both directions independently, because the API can
// filter on only one of them per request.
type alchemyCursor struct {
	OutKey  string `json:"out_key,omitempty"`
	OutDone bool   `json:"out_done,omitempty"`
	InKey   string `json:"in_key,omitempty"`
	InDone  bool   `json:"in_done,omitempty"`
}

// FetchAddress returns one page of an address's history.
//
// Outbound is drained first, then inbound. Callers iterate until Next.Done.
func (a *AlchemyAdapter) FetchAddress(ctx context.Context, address string, cur chain.Cursor) (chain.AddressPage, error) {
	addr := strings.ToLower(strings.TrimSpace(address))
	if !isHexAddress(addr) {
		return chain.AddressPage{}, fmt.Errorf("%s: %q is not a 20-byte address", a.chainID, address)
	}

	state := decodeAlchemyCursor(cur.Value)

	if !state.OutDone {
		return a.fetchDirection(ctx, addr, state, true)
	}
	return a.fetchDirection(ctx, addr, state, false)
}

func (a *AlchemyAdapter) fetchDirection(ctx context.Context, addr string, state alchemyCursor, outbound bool) (chain.AddressPage, error) {
	params := map[string]any{
		"category":         a.categories,
		"withMetadata":     true, // supplies blockTimestamp, avoiding a header fetch per block
		"excludeZeroValue": true,
		"order":            "asc", // stable ordering, so pagination is deterministic
		"maxCount":         fmt.Sprintf("0x%x", a.pageSize),
		"fromBlock":        "0x0",
		"toBlock":          "latest",
	}

	var pageKey string
	if outbound {
		params["fromAddress"] = addr
		pageKey = state.OutKey
	} else {
		params["toAddress"] = addr
		pageKey = state.InKey
	}
	if pageKey != "" {
		params["pageKey"] = pageKey
	}

	var result assetTransfersResult
	if err := a.client.call(ctx, "alchemy_getAssetTransfers", []any{params}, &result); err != nil {
		dir := "outbound"
		if !outbound {
			dir = "inbound"
		}
		return chain.AddressPage{}, fmt.Errorf("%s %s transfers for %s: %w", a.chainID, dir, addr, err)
	}

	transfers := make([]chain.Transfer, 0, len(result.Transfers))
	for _, t := range result.Transfers {
		converted, ok, err := a.toTransfer(t)
		if err != nil {
			return chain.AddressPage{}, err
		}
		if ok {
			transfers = append(transfers, converted)
		}
	}

	next := state
	if outbound {
		next.OutKey = result.PageKey
		next.OutDone = result.PageKey == ""
	} else {
		next.InKey = result.PageKey
		next.InDone = result.PageKey == ""
	}

	dirLabel := "out"
	if !outbound {
		dirLabel = "in"
	}

	return chain.AddressPage{
		Transfers: transfers,
		Next: chain.Cursor{
			Value: encodeAlchemyCursor(next),
			Done:  next.OutDone && next.InDone,
		},
		PageKey: fmt.Sprintf("alchemy:%s:%s:%s", a.chainID, dirLabel, pageKeyOrStart(pageKey)),
	}, nil
}

// toTransfer converts one entry. The bool reports whether it is a fungible
// value transfer worth storing.
func (a *AlchemyAdapter) toTransfer(t assetTransfer) (chain.Transfer, bool, error) {
	// A null `to` means contract creation; no counterparty received value.
	from := strings.ToLower(strings.TrimSpace(t.From))
	to := strings.ToLower(strings.TrimSpace(t.To))
	if !isHexAddress(from) || !isHexAddress(to) {
		return chain.Transfer{}, false, nil
	}

	// NFT categories move no fungible value; including them would distort the
	// proportional split that the haircut depends on.
	switch t.Category {
	case "erc721", "erc1155", "specialnft":
		return chain.Transfer{}, false, nil
	}

	blockNumber, err := hexToUint64(t.BlockNum)
	if err != nil {
		return chain.Transfer{}, false, fmt.Errorf("bad blockNum %q: %w", t.BlockNum, err)
	}

	blockTime, err := time.Parse(time.RFC3339, t.Metadata.BlockTimestamp)
	if err != nil {
		return chain.Transfer{}, false, fmt.Errorf(
			"transfer %s has an unparseable blockTimestamp %q: %w",
			t.UniqueID, t.Metadata.BlockTimestamp, err)
	}

	raw, basis, err := rawValue(t)
	if err != nil {
		return chain.Transfer{}, false, err
	}
	if raw.Sign() <= 0 {
		return chain.Transfer{}, false, nil
	}

	asset := t.Asset
	if asset == "" {
		asset = strings.ToLower(t.RawContract.Address)
	}
	if asset == "" {
		asset = "ETH"
	}

	return chain.Transfer{
		Chain:       a.chainID,
		TxHash:      strings.ToLower(t.Hash),
		LogIndex:    logIndexFromUniqueID(t.UniqueID),
		BlockNumber: blockNumber,
		BlockTime:   blockTime.UTC(),
		FromAddress: from,
		ToAddress:   to,
		Asset:       asset,
		RawValue:    raw,
		PriceBasis:  basis,
	}, true, nil
}

// rawValue extracts the exact on-chain amount.
//
// `rawContract.value` is an exact hex integer and is always preferred. The
// top-level `value` is a JSON float already scaled by the token's decimals,
// so for a large USDT amount it has silently lost precision by the time it
// reaches us — and SPEC.md §2 requires a score be reconstructible from stored
// data, which a rounded amount is not.
//
// Falling back to `value` is therefore a last resort, and when it happens the
// transfer is marked so the imprecision travels with it rather than being
// forgotten.
func rawValue(t assetTransfer) (*big.Int, string, error) {
	if v := strings.TrimSpace(t.RawContract.Value); v != "" && v != "0x" {
		n, err := hexToBig(v)
		if err != nil {
			return nil, "", fmt.Errorf("transfer %s has an invalid rawContract.value %q: %w",
				t.UniqueID, v, err)
		}
		return n, "unpriced", nil
	}

	if t.Value <= 0 {
		return big.NewInt(0), "unpriced", nil
	}

	decimals := int64(18) // native assets on every chain we support
	if d := strings.TrimSpace(t.RawContract.Decimal); d != "" && d != "0x" {
		if n, err := hexToUint64(d); err == nil {
			decimals = int64(n)
		}
	}

	// float64 carries about 15-16 significant digits, so this reconstruction
	// is approximate for large amounts. Recorded as such.
	scale := new(big.Float).SetFloat64(t.Value)
	scale.Mul(scale, new(big.Float).SetInt(pow10(decimals)))
	n, _ := scale.Int(nil)
	return n, "unpriced_approx_amount", nil
}

func pow10(n int64) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil)
}

// logIndexFromUniqueID derives the dedup index from Alchemy's uniqueId.
//
// The identifier looks like "0x<hash>:log:12" for token transfers and
// "0x<hash>:external" for native ones. The log index is taken from it where
// present, giving the same natural key the raw-RPC path produces, so the two
// ingestion routes deduplicate against each other rather than double-counting
// the same transfer (docs/DECISIONS.md D2).
func logIndexFromUniqueID(uniqueID string) uint32 {
	parts := strings.Split(uniqueID, ":")
	if len(parts) >= 3 && parts[len(parts)-2] == "log" {
		var n uint64
		if _, err := fmt.Sscanf(parts[len(parts)-1], "%d", &n); err == nil {
			return uint32(n)
		}
	}
	// An external transfer has no log index; zero is correct rather than
	// synthesised, because a transaction has at most one external transfer.
	return 0
}

// FetchRange satisfies chain.Adapter for backfill.
//
// alchemy_getAssetTransfers is address-oriented, so a whole-range scan would
// mean enumerating addresses first — which is what the raw eth_getLogs adapter
// in this package already does properly. Pointing the caller there beats
// implementing a worse version of it here.
func (a *AlchemyAdapter) FetchRange(ctx context.Context, from, to uint64) ([]chain.Transfer, error) {
	return nil, fmt.Errorf(
		"alchemy adapter is address-oriented (requested blocks %d-%d); "+
			"use the eth_getLogs adapter in this package for block-range backfill", from, to)
}

func encodeAlchemyCursor(c alchemyCursor) string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeAlchemyCursor(v string) alchemyCursor {
	var c alchemyCursor
	if v == "" {
		return c
	}
	// A malformed cursor restarts the address. Re-fetching is safe because
	// ingestion deduplicates; giving up is not.
	_ = json.Unmarshal([]byte(v), &c)
	return c
}

func pageKeyOrStart(k string) string {
	if k == "" {
		return "start"
	}
	return k
}

func isHexAddress(s string) bool {
	if !strings.HasPrefix(s, "0x") || len(s) != 42 {
		return false
	}
	for _, r := range s[2:] {
		isHexDigit := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHexDigit {
			return false
		}
	}
	return true
}
