// Package evm implements chain.Adapter for Ethereum-compatible chains over
// JSON-RPC.
//
// SPEC.md §5 describes the intended shape: bulk backfill from the BigQuery
// public datasets, then a JSON-RPC adapter for the recent tail, both behind
// one interface. This is the JSON-RPC half.
//
// A note on what public endpoints can actually do, measured on 2026-09-21
// rather than assumed:
//
//	publicnode.com   archive requests rejected outright
//	ankr, drpc       authentication required
//	1rpc, pokt       archive served, but capped at 50 blocks per request
//	bsc-dataseed     eth_getLogs rate-limited below usefulness
//
// A 50-block cap means roughly 520,000 requests to walk Ethereum's history,
// so demand-driven per-address ingestion is not possible without a provider
// key that serves archive data. The adapter therefore reports that condition
// explicitly instead of returning a short result that would read as a
// complete history.
package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// Client is a rate-limited JSON-RPC client with retries.
type Client struct {
	url     string
	http    *http.Client
	limiter *rate.Limiter
	log     *slog.Logger

	maxRetries int
	nextID     int64
}

type Options struct {
	URL               string
	RequestsPerSecond float64
	Burst             int
	Timeout           time.Duration
	MaxRetries        int
	Logger            *slog.Logger
}

func NewClient(opts Options) *Client {
	if opts.RequestsPerSecond <= 0 {
		opts.RequestsPerSecond = 10
	}
	if opts.Burst <= 0 {
		opts.Burst = 4
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 45 * time.Second
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 4
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		url:        opts.URL,
		http:       &http.Client{Timeout: opts.Timeout},
		limiter:    rate.NewLimiter(rate.Limit(opts.RequestsPerSecond), opts.Burst),
		log:        opts.Logger,
		maxRetries: opts.MaxRetries,
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// ErrArchiveRequired signals that the endpoint will not serve the historical
// data the request needs.
//
// Distinguished from a generic failure on purpose: it is not transient, a
// retry will not fix it, and the caller must report a coverage gap rather
// than treat a short result as a complete history.
type ErrArchiveRequired struct{ Detail string }

func (e *ErrArchiveRequired) Error() string {
	return "archive data required but this endpoint will not serve it: " + e.Detail
}

// ErrRangeTooLarge signals that the endpoint caps the block span per request.
type ErrRangeTooLarge struct {
	Detail string
	// MaxBlocks is the cap when the endpoint states one, else 0.
	MaxBlocks uint64
}

func (e *ErrRangeTooLarge) Error() string {
	if e.MaxBlocks > 0 {
		return fmt.Sprintf("block range too large, endpoint allows %d: %s", e.MaxBlocks, e.Detail)
	}
	return "block range too large: " + e.Detail
}

func (c *Client) call(ctx context.Context, method string, params []any, out any) error {
	var lastErr error

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(attempt)):
			}
		}
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}

		c.nextID++
		body, err := json.Marshal(rpcRequest{
			JSONRPC: "2.0", ID: c.nextID, Method: method, Params: params,
		})
		if err != nil {
			return err
		}

		raw, retryable, err := c.once(ctx, body)
		if err == nil {
			if out == nil {
				return nil
			}
			return json.Unmarshal(raw, out)
		}
		lastErr = err
		if !retryable {
			return err
		}
	}
	return fmt.Errorf("%s failed after %d attempts: %w", method, c.maxRetries+1, lastErr)
}

func (c *Client) once(ctx context.Context, body []byte) (json.RawMessage, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("rpc transport: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if err != nil {
		return nil, true, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, true, fmt.Errorf("rate limited (429)")
	}
	if resp.StatusCode >= 500 {
		return nil, true, fmt.Errorf("server error %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(payload, 200))
	}

	var out rpcResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, false, fmt.Errorf("decode rpc response: %w", err)
	}
	if out.Error != nil {
		return nil, retryableRPC(out.Error), classify(out.Error)
	}
	return out.Result, false, nil
}

// classify turns a provider's error text into a typed error.
//
// Providers report the same two conditions in prose rather than in codes, and
// the wording differs between them. Matching on text is unpleasant but the
// alternative is treating "we will not serve this" as a generic failure, which
// would let a truncated history look complete.
func classify(e *rpcError) error {
	msg := strings.ToLower(e.Message)

	switch {
	case strings.Contains(msg, "archive"),
		strings.Contains(msg, "missing trie node"),
		strings.Contains(msg, "state is not available"),
		strings.Contains(msg, "older than"):
		return &ErrArchiveRequired{Detail: e.Message}

	case strings.Contains(msg, "block range"),
		strings.Contains(msg, "range too large"),
		strings.Contains(msg, "limit exceeded"),
		strings.Contains(msg, "too many results"),
		strings.Contains(msg, "query returned more than"),
		strings.Contains(msg, "response size exceeded"):
		return &ErrRangeTooLarge{Detail: e.Message, MaxBlocks: parseMaxBlocks(e.Message)}
	}
	return e
}

func retryableRPC(e *rpcError) bool {
	msg := strings.ToLower(e.Message)
	// Neither an archive refusal nor a range cap improves on retry; both are
	// statements about what the endpoint will do, not transient faults.
	if strings.Contains(msg, "archive") || strings.Contains(msg, "block range") ||
		strings.Contains(msg, "limit exceeded") {
		return false
	}
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "busy") ||
		strings.Contains(msg, "try again")
}

// parseMaxBlocks extracts a stated cap, e.g. "maximum allowed is 50 blocks".
func parseMaxBlocks(msg string) uint64 {
	fields := strings.FieldsFunc(msg, func(r rune) bool {
		return r < '0' || r > '9'
	})
	// The largest bare number in such a message is the cap in every phrasing
	// observed; a smaller one is usually an index or a code.
	var best uint64
	for _, f := range fields {
		var n uint64
		if _, err := fmt.Sscanf(f, "%d", &n); err == nil && n > best && n < 1_000_000 {
			best = n
		}
	}
	return best
}

func backoff(attempt int) time.Duration {
	const base = 400 * time.Millisecond
	const max = 20 * time.Second
	d := time.Duration(float64(base) * math.Pow(2, float64(attempt-1)))
	if d > max {
		d = max
	}
	return time.Duration(rand.Int63n(int64(d)) + int64(d)/2)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// hexToUint64 parses a 0x-prefixed quantity.
func hexToUint64(s string) (uint64, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return 0, fmt.Errorf("empty hex quantity")
	}
	n := new(big.Int)
	if _, ok := n.SetString(s, 16); !ok {
		return 0, fmt.Errorf("invalid hex quantity %q", s)
	}
	return n.Uint64(), nil
}

// hexToBig parses a 0x-prefixed 256-bit value.
func hexToBig(s string) (*big.Int, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return big.NewInt(0), nil
	}
	n := new(big.Int)
	if _, ok := n.SetString(s, 16); !ok {
		return nil, fmt.Errorf("invalid hex value %q", s)
	}
	return n, nil
}

// Latest returns the newest block number.
func (c *Client) Latest(ctx context.Context) (uint64, error) {
	var h string
	if err := c.call(ctx, "eth_blockNumber", []any{}, &h); err != nil {
		return 0, err
	}
	return parseHexUint(h)
}

// BlockTime returns a block's timestamp.
func (c *Client) BlockTime(ctx context.Context, n uint64) (time.Time, error) {
	var b struct {
		Timestamp string `json:"timestamp"`
	}
	if err := c.call(ctx, "eth_getBlockByNumber", []any{fmt.Sprintf("0x%x", n), false}, &b); err != nil {
		return time.Time{}, err
	}
	ts, err := parseHexUint(b.Timestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("block %d timestamp: %w", n, err)
	}
	return time.Unix(int64(ts), 0).UTC(), nil
}

// CallAt runs a read-only call against a contract as of a block, and returns
// the hex result. Past blocks need an archive endpoint, which Alchemy is
// (docs/DECISIONS.md D41).
func (c *Client) CallAt(ctx context.Context, to, data string, block uint64) (string, error) {
	var out string
	err := c.call(ctx, "eth_call", []any{map[string]string{"to": to, "data": data}, fmt.Sprintf("0x%x", block)}, &out)
	return out, err
}

func parseHexUint(h string) (uint64, error) {
	return strconv.ParseUint(strings.TrimPrefix(h, "0x"), 16, 64)
}
