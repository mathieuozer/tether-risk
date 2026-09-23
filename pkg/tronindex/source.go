package tronindex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// Source reads blocks. NodeClient implements it; tests use their own.
type Source interface {
	// Head is the newest block the source will serve.
	Head(ctx context.Context) (uint64, error)
	Block(ctx context.Context, n uint64) (RawBlock, error)
	TxInfos(ctx context.Context, n uint64) ([]RawTxInfo, error)
}

// NodeOptions configures a NodeClient.
type NodeOptions struct {
	// BaseURL is the node's HTTP API: http://127.0.0.1:8090 for a local
	// java-tron, or a hosted endpoint.
	BaseURL string
	// Header is sent with every request, for a hosted endpoint's key, for
	// example {"TRON-PRO-API-KEY": "..."}.
	Header map[string]string
	// Solidified reads /walletsolidity: only blocks no reorganisation can
	// undo, about a minute behind the head. Recommended for an index.
	Solidified        bool
	RequestsPerSecond float64 // 0 means unlimited, for a local node
	Timeout           time.Duration
	MaxRetries        int
}

// NodeClient reads blocks over java-tron's HTTP API, which local nodes and
// most hosted TRON endpoints serve.
type NodeClient struct {
	opts    NodeOptions
	http    *http.Client
	limiter *rate.Limiter
}

func NewNodeClient(opts NodeOptions) *NodeClient {
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 8
	}
	lim := rate.NewLimiter(rate.Inf, 1)
	if opts.RequestsPerSecond > 0 {
		lim = rate.NewLimiter(rate.Limit(opts.RequestsPerSecond), 1)
	}
	return &NodeClient{opts: opts, http: &http.Client{Timeout: opts.Timeout}, limiter: lim}
}

func (c *NodeClient) path(method string) string {
	prefix := "/wallet/"
	if c.opts.Solidified {
		prefix = "/walletsolidity/"
	}
	return strings.TrimRight(c.opts.BaseURL, "/") + prefix + method
}

func (c *NodeClient) Head(ctx context.Context) (uint64, error) {
	var b RawBlock
	if err := c.post(ctx, c.path("getnowblock"), map[string]any{}, &b); err != nil {
		return 0, err
	}
	return b.BlockHeader.RawData.Number, nil
}

func (c *NodeClient) Block(ctx context.Context, n uint64) (RawBlock, error) {
	var b RawBlock
	err := c.post(ctx, c.path("getblockbynum"), map[string]any{"num": n, "visible": true}, &b)
	if err == nil && b.BlockHeader.RawData.Number != n {
		err = fmt.Errorf("asked for block %d, got %d", n, b.BlockHeader.RawData.Number)
	}
	return b, err
}

func (c *NodeClient) TxInfos(ctx context.Context, n uint64) ([]RawTxInfo, error) {
	var infos []RawTxInfo
	// An empty block answers {} rather than [], so an object is not an error.
	var raw json.RawMessage
	if err := c.post(ctx, c.path("gettransactioninfobyblocknum"), map[string]any{"num": n}, &raw); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &infos); err != nil {
		return nil, fmt.Errorf("block %d infos: %w", n, err)
	}
	return infos, nil
}

// post sends a JSON request, retrying transient failures with backoff.
func (c *NodeClient) post(ctx context.Context, url string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt <= c.opts.MaxRetries; attempt++ {
		if attempt > 0 {
			d := time.Duration(1<<min(attempt, 5)) * 250 * time.Millisecond
			d = d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
			}
		}
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range c.opts.Header {
			req.Header.Set(k, v)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			last = err
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
		resp.Body.Close()
		switch {
		case rerr != nil:
			last = rerr
			continue
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			last = fmt.Errorf("%s: status %d: %s", url, resp.StatusCode, truncate(data, 200))
			continue
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("%s: status %d: %s", url, resp.StatusCode, truncate(data, 200))
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s: decode: %w", url, err)
		}
		return nil
	}
	return fmt.Errorf("%s failed after %d attempts: %w", url, c.opts.MaxRetries+1, last)
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
