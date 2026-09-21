package evm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mozer/tether-risk/internal/chain"
)

// transferTopic is keccak256("Transfer(address,address,uint256)"), the first
// topic of every ERC-20 Transfer event.
const transferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

// Adapter implements chain.Adapter over JSON-RPC.
type Adapter struct {
	client  *Client
	chainID string

	// tokens limits which contracts are scanned. ERC-20 Transfer events are
	// emitted by every token ever deployed, most of them worthless or
	// deliberately spammy, so an unfiltered scan returns mostly noise and
	// enormous responses. SPEC.md §3 names USDT the priority asset.
	tokens []string

	// blockChunk is the span per eth_getLogs call, narrowed automatically when
	// an endpoint reports a smaller cap.
	blockChunk uint64
}

// AdapterOptions configures an Adapter.
type AdapterOptions struct {
	// ChainID is "ethereum" or "bsc".
	ChainID string
	// Tokens are the contract addresses to scan. Empty means all contracts,
	// which public endpoints will refuse on any meaningful range.
	Tokens []string
	// BlockChunk is the initial span per request.
	BlockChunk uint64
}

func NewAdapter(c *Client, opts AdapterOptions) *Adapter {
	if opts.BlockChunk == 0 {
		opts.BlockChunk = 2000
	}
	if opts.ChainID == "" {
		opts.ChainID = "ethereum"
	}
	tokens := make([]string, 0, len(opts.Tokens))
	for _, t := range opts.Tokens {
		tokens = append(tokens, strings.ToLower(strings.TrimSpace(t)))
	}
	return &Adapter{
		client:     c,
		chainID:    opts.ChainID,
		tokens:     tokens,
		blockChunk: opts.BlockChunk,
	}
}

func (a *Adapter) Chain() string { return a.chainID }

// Head returns the current head block number.
func (a *Adapter) Head(ctx context.Context) (uint64, error) {
	var hex string
	if err := a.client.call(ctx, "eth_blockNumber", []any{}, &hex); err != nil {
		return 0, fmt.Errorf("%s head: %w", a.chainID, err)
	}
	return hexToUint64(hex)
}

// rpcLog is one log entry from eth_getLogs.
type rpcLog struct {
	Address          string   `json:"address"`
	Topics           []string `json:"topics"`
	Data             string   `json:"data"`
	BlockNumber      string   `json:"blockNumber"`
	TransactionHash  string   `json:"transactionHash"`
	LogIndex         string   `json:"logIndex"`
	Removed          bool     `json:"removed"`
	BlockTimestamp   string   `json:"blockTimestamp"`
	TransactionIndex string   `json:"transactionIndex"`
}

// FetchRange returns ERC-20 transfers in a block range.
//
// The range is walked in chunks, narrowing automatically when an endpoint
// reports a cap. A range the endpoint refuses outright surfaces as a typed
// error rather than an empty slice, because an empty slice means "no
// transfers here" and that is a different and wrong claim.
func (a *Adapter) FetchRange(ctx context.Context, from, to uint64) ([]chain.Transfer, error) {
	if to < from {
		return nil, fmt.Errorf("invalid range %d-%d", from, to)
	}

	var out []chain.Transfer
	chunk := a.blockChunk
	times := newBlockTimeCache(a.client)

	for start := from; start <= to; {
		end := start + chunk - 1
		if end > to {
			end = to
		}

		logs, err := a.getLogs(ctx, start, end)
		if err != nil {
			var rangeErr *ErrRangeTooLarge
			if asRangeTooLarge(err, &rangeErr) && chunk > 1 {
				// Narrow and retry the same span rather than giving up. The
				// endpoint has told us what it will serve; honour it.
				next := chunk / 4
				if rangeErr.MaxBlocks > 0 && rangeErr.MaxBlocks < next {
					next = rangeErr.MaxBlocks
				}
				if next < 1 {
					next = 1
				}
				a.client.log.Debug("narrowing block chunk",
					"chain", a.chainID, "from", chunk, "to", next)
				chunk = next
				continue
			}
			return nil, fmt.Errorf("%s logs %d-%d: %w", a.chainID, start, end, err)
		}

		for _, l := range logs {
			t, ok, err := a.toTransfer(ctx, l, times)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, t)
			}
		}
		start = end + 1
	}
	return out, nil
}

func (a *Adapter) getLogs(ctx context.Context, from, to uint64) ([]rpcLog, error) {
	filter := map[string]any{
		"fromBlock": fmt.Sprintf("0x%x", from),
		"toBlock":   fmt.Sprintf("0x%x", to),
		"topics":    []any{transferTopic},
	}
	if len(a.tokens) == 1 {
		filter["address"] = a.tokens[0]
	} else if len(a.tokens) > 1 {
		filter["address"] = a.tokens
	}

	var logs []rpcLog
	if err := a.client.call(ctx, "eth_getLogs", []any{filter}, &logs); err != nil {
		return nil, err
	}
	return logs, nil
}

// toTransfer converts a Transfer log. The bool reports whether it is a
// well-formed transfer worth storing.
func (a *Adapter) toTransfer(ctx context.Context, l rpcLog, times *blockTimeCache) (chain.Transfer, bool, error) {
	// A removed log belonged to a reorganised block and describes value that
	// never moved.
	if l.Removed {
		return chain.Transfer{}, false, nil
	}
	// Topics: [signature, from, to]. A Transfer with fewer is malformed or a
	// non-standard token; either way we cannot say who paid whom.
	if len(l.Topics) < 3 {
		return chain.Transfer{}, false, nil
	}

	from, err := topicToAddress(l.Topics[1])
	if err != nil {
		return chain.Transfer{}, false, nil
	}
	to, err := topicToAddress(l.Topics[2])
	if err != nil {
		return chain.Transfer{}, false, nil
	}

	value, err := hexToBig(l.Data)
	if err != nil {
		return chain.Transfer{}, false, nil
	}

	blockNumber, err := hexToUint64(l.BlockNumber)
	if err != nil {
		return chain.Transfer{}, false, fmt.Errorf("bad block number %q: %w", l.BlockNumber, err)
	}
	logIndex, err := hexToUint64(l.LogIndex)
	if err != nil {
		return chain.Transfer{}, false, fmt.Errorf("bad log index %q: %w", l.LogIndex, err)
	}

	// Some endpoints include the block timestamp on the log; most do not.
	// Falling back to a cached per-block lookup keeps the request count
	// proportional to blocks rather than to transfers.
	var blockTime time.Time
	if l.BlockTimestamp != "" {
		if ts, err := hexToUint64(l.BlockTimestamp); err == nil {
			blockTime = time.Unix(int64(ts), 0).UTC()
		}
	}
	if blockTime.IsZero() {
		ts, err := times.get(ctx, blockNumber)
		if err != nil {
			return chain.Transfer{}, false, err
		}
		blockTime = ts
	}

	return chain.Transfer{
		Chain:       a.chainID,
		TxHash:      strings.ToLower(l.TransactionHash),
		LogIndex:    uint32(logIndex),
		BlockNumber: blockNumber,
		BlockTime:   blockTime,
		FromAddress: from,
		ToAddress:   to,
		// The contract address identifies the asset. A symbol would need an
		// extra call per token and is not unique; the contract is.
		Asset:      strings.ToLower(l.Address),
		RawValue:   value,
		PriceBasis: "unpriced",
	}, true, nil
}

// topicToAddress extracts a 20-byte address from a 32-byte topic.
func topicToAddress(topic string) (string, error) {
	t := strings.TrimPrefix(strings.ToLower(topic), "0x")
	if len(t) != 64 {
		return "", fmt.Errorf("topic is not 32 bytes: %q", topic)
	}
	// The address occupies the low 20 bytes; the high 12 must be zero, and are
	// not for a topic that is not an address.
	if strings.Trim(t[:24], "0") != "" {
		return "", fmt.Errorf("topic is not an address: %q", topic)
	}
	return "0x" + t[24:], nil
}

// FetchAddress is part of chain.Adapter (docs/DECISIONS.md D3).
//
// Raw JSON-RPC has no per-address history call. Building one means scanning
// every block since the address first appeared with a topic filter, which
// needs archive access — and measured against public endpoints on 2026-09-21,
// archive access is either refused outright or capped at 50 blocks per
// request, which is roughly 520,000 requests to cover Ethereum.
//
// So this returns a typed error rather than a short result. A partial history
// presented as complete would understate exposure, and understating exposure
// is the failure direction that matters.
func (a *Adapter) FetchAddress(ctx context.Context, address string, cur chain.Cursor) (chain.AddressPage, error) {
	return chain.AddressPage{}, &ErrArchiveRequired{
		Detail: fmt.Sprintf(
			"per-address history on %s requires scanning historical logs. "+
				"Configure an endpoint with archive access, or use the BigQuery "+
				"backfill path (SPEC.md §5). Address %s was not fetched.",
			a.chainID, address),
	}
}

// ---------------------------------------------------------------------------
// Block timestamps
// ---------------------------------------------------------------------------

// blockTimeCache avoids one header fetch per transfer. Many transfers share a
// block, so this turns a per-transfer cost into a per-block one.
type blockTimeCache struct {
	client *Client
	byNum  map[uint64]time.Time
}

func newBlockTimeCache(c *Client) *blockTimeCache {
	return &blockTimeCache{client: c, byNum: map[uint64]time.Time{}}
}

func (b *blockTimeCache) get(ctx context.Context, number uint64) (time.Time, error) {
	if t, ok := b.byNum[number]; ok {
		return t, nil
	}

	var header struct {
		Timestamp string `json:"timestamp"`
	}
	// false: headers only, not the full transaction list.
	err := b.client.call(ctx, "eth_getBlockByNumber",
		[]any{fmt.Sprintf("0x%x", number), false}, &header)
	if err != nil {
		return time.Time{}, fmt.Errorf("block %d header: %w", number, err)
	}
	if header.Timestamp == "" {
		return time.Time{}, fmt.Errorf("block %d has no timestamp", number)
	}

	ts, err := hexToUint64(header.Timestamp)
	if err != nil {
		return time.Time{}, err
	}
	t := time.Unix(int64(ts), 0).UTC()
	b.byNum[number] = t
	return t, nil
}

// asRangeTooLarge unwraps a range-cap error.
func asRangeTooLarge(err error, target **ErrRangeTooLarge) bool {
	for err != nil {
		if e, ok := err.(*ErrRangeTooLarge); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// decodeLogs is used by tests to load fixtures.
func decodeLogs(raw []byte) ([]rpcLog, error) {
	var logs []rpcLog
	if err := json.Unmarshal(raw, &logs); err != nil {
		return nil, err
	}
	return logs, nil
}
