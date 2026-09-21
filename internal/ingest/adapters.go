package ingest

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/mozer/tether-risk/internal/chain"
	"github.com/mozer/tether-risk/internal/chain/evm"
	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/config"
	"github.com/mozer/tether-risk/internal/pricing"
	"github.com/mozer/tether-risk/internal/store"
)

// NewAdapter selects the adapter for a chain.
//
// EVM chains prefer Alchemy's per-address transfers API when a key is
// configured, because raw JSON-RPC has no per-address history call and the
// archive scan that would substitute for one is not served by any public
// endpoint (see internal/chain/evm/client.go). Without a key the chain is
// declared unavailable in sources.yaml and never reaches here.
func NewAdapter(chainID string, cfg config.ChainSource, log *slog.Logger) (chain.Adapter, error) {
	switch chainID {
	case "tron":
		return tron.NewAdapter(tron.NewClient(tron.Options{
			BaseURL:           cfg.URL,
			APIKey:            os.Getenv("TRONGRID_API_KEY"), // optional; no secrets in the repo
			RequestsPerSecond: float64(cfg.RateLimitPerSec),
			Logger:            log,
		})), nil

	case "ethereum", "bsc":
		envVar := "ETH_RPC_URL"
		if chainID == "bsc" {
			envVar = "BSC_RPC_URL"
		}
		url := os.Getenv(envVar)
		if url == "" {
			url = cfg.URL
		}
		if url == "" {
			return nil, fmt.Errorf("no endpoint for %s; set %s", chainID, envVar)
		}

		client := evm.NewClient(evm.Options{
			URL:               url,
			RequestsPerSecond: float64(cfg.RateLimitPerSec),
			Logger:            log,
		})

		if strings.Contains(url, "alchemy.com") {
			log.Info("using the alchemy per-address transfers API", "chain", chainID)
			return evm.NewAlchemyAdapter(client, evm.AlchemyOptions{ChainID: chainID}), nil
		}

		// A non-Alchemy endpoint still gives a working block-range adapter,
		// but demand-driven per-address ingestion will fail with a typed
		// archive error rather than returning a misleadingly empty history.
		log.Warn("endpoint is not Alchemy; per-address ingestion will report "+
			"that archive access is required", "chain", chainID, "url_host", hostOf(url))
		return evm.NewAdapter(client, evm.AdapterOptions{ChainID: chainID}), nil

	default:
		return nil, fmt.Errorf("no adapter for chain %q", chainID)
	}
}

func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return "unknown"
}

// NewPrefetcher builds a worker for fetching single addresses on demand, as
// screening does before it scores. It shares the queue and the TTL cache
// with the background worker, so an address fetched by either is fresh for
// both.
func NewPrefetcher(chainID string, cfg *config.Config, ch, pg *sql.DB, log *slog.Logger) (*Worker, error) {
	chainCfg, ok := cfg.Chain(chainID)
	if !ok {
		return nil, fmt.Errorf("chain %s is not declared in sources.yaml", chainID)
	}
	if !chainCfg.Available() {
		return nil, fmt.Errorf("chain %s is unavailable: %s", chainID, chainCfg.UnavailableReason)
	}
	adapter, err := NewAdapter(chainID, chainCfg, log)
	if err != nil {
		return nil, err
	}
	return NewWorker("screen", adapter, store.NewJobs(pg), store.NewTransferWriter(ch, pg),
		Options{Logger: log, Pricer: pricing.New(cfg, pg)}), nil
}
