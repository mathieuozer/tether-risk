package main

import (
	"regexp"
	"strings"

	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/config"
)

// chainInfo is a chain as customers see it.
type chainInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// knownChains is every chain the engine has an adapter for, in display
// order. Whether each is enabled comes from sources.yaml.
var knownChains = []chainInfo{
	{ID: "tron", Name: "Tron"},
	{ID: "ethereum", Name: "Ethereum"},
	{ID: "bsc", Name: "BNB Smart Chain"},
}

func loadChains(cfg *config.Config) []chainInfo {
	out := make([]chainInfo, 0, len(knownChains))
	for _, c := range knownChains {
		if src, ok := cfg.Chain(c.ID); ok {
			c.Enabled = src.Available()
		}
		out = append(out, c)
	}
	return out
}

func (b *bot) chain(id string) (chainInfo, bool) {
	for _, c := range b.chains {
		if c.ID == id {
			return c, true
		}
	}
	return chainInfo{}, false
}

var evmAddress = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// chainAliases lets a customer name the chain before an EVM address, which
// is otherwise ambiguous: "bsc 0x…".
var chainAliases = map[string]string{
	"tron": "tron", "trx": "tron",
	"eth": "ethereum", "ethereum": "ethereum", "erc20": "ethereum",
	"bsc": "bsc", "bnb": "bsc", "bep20": "bsc",
}

// errBadAddress and errChainUnavailable are what parseTarget reports.
type targetError struct {
	code  string // bad_address or chain_unavailable
	chain string
}

func (e *targetError) Error() string { return e.code }

// parseTarget reads an address, optionally preceded by a chain name, and
// returns the chain and the address in canonical form. An explicit chain
// (from the app) wins over one in the text. An EVM address with no chain
// named is Ethereum.
func (b *bot) parseTarget(text, explicit string) (string, string, error) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return "", "", &targetError{code: "bad_address"}
	}
	chain := chainAliases[strings.ToLower(explicit)]
	addr := fields[0]
	if len(fields) > 1 {
		if c, ok := chainAliases[strings.ToLower(fields[0])]; ok {
			if chain == "" {
				chain = c
			}
			addr = fields[1]
		}
	}
	addr = strings.Trim(addr, " ,;\"'")

	switch {
	case tron.IsValid(addr):
		if chain != "" && chain != "tron" {
			return "", "", &targetError{code: "bad_address"}
		}
		chain = "tron"
	case evmAddress.MatchString(addr):
		if chain == "" {
			chain = "ethereum"
		}
		if chain == "tron" {
			return "", "", &targetError{code: "bad_address"}
		}
		addr = strings.ToLower(addr)
	default:
		return "", "", &targetError{code: "bad_address"}
	}
	if c, ok := b.chain(chain); !ok || !c.Enabled {
		return chain, addr, &targetError{code: "chain_unavailable", chain: chain}
	}
	return chain, addr, nil
}
