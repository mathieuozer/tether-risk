package tron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

// Client is a rate-limited, retrying TronGrid HTTP client.
//
// SPEC.md §5: respect rate limits, exponential backoff, resumable cursor. The
// free tier is the only access available (docs/PLAN.md), so staying inside its
// limits is not optional politeness — exceeding them gets the whole ingestion
// path throttled and the Phase 1 timing gate becomes unmeetable.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
	limiter *rate.Limiter
	log     *slog.Logger

	maxRetries int
}

// Options configures a Client. The zero value of each field selects a
// sensible default.
type Options struct {
	BaseURL string
	APIKey  string // optional; raises the rate limit when present

	// RequestsPerSecond is the sustained rate. TronGrid's documented free-tier
	// allowance is higher, but a margin is kept: the limit is enforced per IP
	// and other processes on this host may share it.
	RequestsPerSecond float64
	Burst             int
	Timeout           time.Duration
	MaxRetries        int
	Logger            *slog.Logger
}

func NewClient(opts Options) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.trongrid.io"
	}
	if opts.RequestsPerSecond <= 0 {
		opts.RequestsPerSecond = 12
	}
	if opts.Burst <= 0 {
		// Burst 1, not 4.
		//
		// Measured against TronGrid on 2026-09-21: a serial stream at a 3/s
		// target completed 10 of 10 requests untroubled, while concurrent
		// workers sharing a 3/s limiter with burst 4 were still rejected. The
		// service penalises concurrency, not just sustained rate.
		//
		// Bursting also buys nothing here. Each request costs about a second
		// of latency, so a single stream achieves roughly 1/s whatever the
		// limiter permits — the throughput ceiling is the network, and every
		// extra concurrent request only moves us closer to a 429 and the
		// backoff that follows it.
		opts.Burst = 1
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 5
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	return &Client{
		baseURL:    opts.BaseURL,
		apiKey:     opts.APIKey,
		http:       &http.Client{Timeout: opts.Timeout},
		limiter:    rate.NewLimiter(rate.Limit(opts.RequestsPerSecond), opts.Burst),
		log:        opts.Logger,
		maxRetries: opts.MaxRetries,
	}
}

// ErrNotFound is returned for a 404 from the upstream API.
var ErrNotFound = errors.New("tron: not found")

// get performs a rate-limited GET with retries, decoding JSON into out.
//
// url must be a fully-formed absolute URL. TronGrid's pagination returns an
// opaque `links.next`, and following it verbatim is more robust than
// reconstructing query parameters ourselves.
func (c *Client) get(ctx context.Context, url string, out any) error {
	var lastErr error

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := c.backoff(attempt)
			c.log.Debug("retrying trongrid request",
				"attempt", attempt, "delay", delay, "error", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		// Wait for a rate-limit token before every attempt, retries included:
		// a retry that ignored the limiter would make throttling worse
		// precisely when the server is already asking us to slow down.
		if err := c.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("rate limiter: %w", err)
		}

		body, retryable, err := c.doOnce(ctx, url)
		if err == nil {
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("decode trongrid response: %w", err)
			}
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
	}
	return fmt.Errorf("trongrid request failed after %d attempts: %w", c.maxRetries+1, lastErr)
}

// doOnce performs a single request. The second return value reports whether
// the error is worth retrying.
func (c *Client) doOnce(ctx context.Context, url string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Transport errors are transient far more often than not: a dropped
		// connection or a DNS blip should not abandon an ingestion job.
		return nil, true, fmt.Errorf("trongrid request: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if readErr != nil {
		return nil, true, fmt.Errorf("read trongrid response: %w", readErr)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		return body, false, nil

	case resp.StatusCode == http.StatusNotFound:
		return nil, false, ErrNotFound

	case resp.StatusCode == http.StatusTooManyRequests:
		// Honour Retry-After when the server sends it. Guessing shorter than
		// instructed is how a temporary throttle becomes a sustained one.
		if wait := parseRetryAfter(resp.Header.Get("Retry-After")); wait > 0 {
			c.log.Warn("trongrid rate limited", "retry_after", wait)
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-time.After(wait):
			}
		}
		return nil, true, fmt.Errorf("trongrid rate limited (429)")

	case resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("trongrid server error %d: %s", resp.StatusCode, truncate(body, 200))

	case resp.StatusCode >= 400:
		// Client errors are our fault and will not fix themselves.
		return nil, false, fmt.Errorf("trongrid client error %d: %s", resp.StatusCode, truncate(body, 200))

	default:
		return nil, false, fmt.Errorf("trongrid unexpected status %d", resp.StatusCode)
	}
}

// backoff returns an exponentially increasing delay with full jitter.
//
// Jitter matters more than the exponent here: several workers throttled at the
// same moment would otherwise retry in lockstep and re-trigger the limit
// together.
func (c *Client) backoff(attempt int) time.Duration {
	const (
		base = 500 * time.Millisecond
		max  = 30 * time.Second
	)
	d := time.Duration(float64(base) * math.Pow(2, float64(attempt-1)))
	if d > max {
		d = max
	}
	return time.Duration(rand.Int63n(int64(d)) + int64(d)/2)
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(h); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
