package labels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"time"
)

// TronSampler implements Sampler against TronGrid.
//
// Deliberately separate from the ingestion adapter in internal/chain/tron.
// That adapter persists everything it sees; this one reads a bounded sample
// and keeps nothing. Sharing the code would mean one caller's page limits and
// cursor semantics constrained the other's.
type TronSampler struct {
	baseURL string
	apiKey  string
	http    *http.Client

	// minInterval paces requests. Sampling runs as a batch job over many
	// candidates, so it must not consume the rate-limit budget that
	// demand-driven screening depends on.
	minInterval time.Duration
	last        time.Time
}

func NewTronSampler(baseURL, apiKey string) *TronSampler {
	if baseURL == "" {
		baseURL = "https://api.trongrid.io"
	}
	return &TronSampler{
		baseURL:     baseURL,
		apiKey:      apiKey,
		http:        &http.Client{Timeout: 30 * time.Second},
		minInterval: 120 * time.Millisecond,
	}
}

type samplerResponse struct {
	Data []struct {
		From string `json:"from"`
		To   string `json:"to"`
		Type string `json:"type"`
	} `json:"data"`
	Meta struct {
		Links struct {
			Next string `json:"next"`
		} `json:"links"`
	} `json:"meta"`
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// Sample reads up to `pages` pages of an address's TRC-20 history and reports
// the breadth of its counterparties.
//
// Only TRC-20 is sampled. Native TRX transfers are dominated by dusting on
// TRON, and counting dusters as counterparties would make any dusted wallet
// look like a service — precisely the false positive this detector must not
// produce.
func (s *TronSampler) Sample(ctx context.Context, address string, pages, pageSize int) (Sample, error) {
	if pages <= 0 {
		pages = 3
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 200 // TronGrid's documented maximum
	}

	next := fmt.Sprintf("%s/v1/accounts/%s/transactions/trc20?limit=%d&only_confirmed=true",
		s.baseURL, url.PathEscape(address), pageSize)

	counterparties := make(map[string]struct{})
	var out Sample

	for page := 0; page < pages && next != ""; page++ {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		default:
		}

		if err := s.pace(ctx); err != nil {
			return out, err
		}

		body, err := s.get(ctx, next)
		if err != nil {
			return out, err
		}

		var resp samplerResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return out, fmt.Errorf("decode sample page: %w", err)
		}
		if !resp.Success && resp.Error != "" {
			return out, fmt.Errorf("trongrid: %s", resp.Error)
		}

		for _, it := range resp.Data {
			out.Transfers++
			for _, cp := range []string{it.From, it.To} {
				if cp != "" && cp != address {
					counterparties[cp] = struct{}{}
				}
			}
		}

		out.PagesFetched++
		next = resp.Meta.Links.Next
	}

	out.Counterparties = len(counterparties)
	// The sample stopped early only because the page budget ran out, so more
	// history remains. This is the signal that separates a high-volume service
	// from a small wallet that merely looks diverse.
	out.MorePages = next != ""
	return out, nil
}

func (s *TronSampler) pace(ctx context.Context) error {
	wait := s.minInterval - time.Since(s.last)
	if wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	s.last = time.Now()
	return nil
}

// get performs one request with backoff.
//
// Sampling runs as a batch over many candidates and will meet the rate limit;
// without backoff the first 429 abandons that candidate and it goes unjudged,
// which silently costs coverage. Retrying is not politeness here, it is the
// difference between a detector that works on a large candidate set and one
// that only works on a small one.
func (s *TronSampler) get(ctx context.Context, u string) ([]byte, error) {
	const maxAttempts = 5
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Exponential with jitter, so concurrent samplers throttled at the
			// same moment do not retry in lockstep.
			base := time.Duration(1<<uint(attempt-1)) * time.Second
			delay := base/2 + time.Duration(rand.Int63n(int64(base)))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if s.apiKey != "" {
			req.Header.Set("TRON-PRO-API-KEY", s.apiKey)
		}

		resp, err := s.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("sample request: %w", err)
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
			lastErr = fmt.Errorf("sample request returned %d", resp.StatusCode)
			continue
		default:
			// A 4xx other than 429 is our fault and will not fix itself.
			return nil, fmt.Errorf("sample request returned %d", resp.StatusCode)
		}
	}
	return nil, fmt.Errorf("sampling failed after %d attempts: %w", maxAttempts, lastErr)
}
