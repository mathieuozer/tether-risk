package labels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Abuse-report ingestion.
//
// SPEC.md §6.4: these are unverified user reports, so they carry confidence
// 0.5, and a single report must never alone push an address into a high-risk
// band. That last rule is enforced in the resolver, not here — an ingester's
// job is to record what a source claims, accurately and without embellishment.
//
// Known coverage limits, measured against the live feeds on 2026-09-21 rather
// than assumed:
//
//   - ScamSniffer holds 2,530 addresses, all EVM. **Zero are TRON.**
//   - CryptoScamDB holds roughly 5,300 addresses, of which **19 are TRON**.
//
// TRON is currently the only chain with a live data path, so neither source
// meaningfully improves TRON coverage today. They are built because SPEC.md
// §6.4 requires them and because their EVM coverage becomes useful the moment
// an Ethereum endpoint exists. Recorded here so nobody later mistakes these
// sources for TRON coverage they do not provide.

// AbuseResult reports what an ingestion run found.
type AbuseResult struct {
	EntriesScanned  int
	AddressesFound  int
	LabelsProduced  int
	ByChain         map[string]int
	SkippedOffChain map[string]int
	Malformed       []string
}

func newAbuseResult() *AbuseResult {
	return &AbuseResult{
		ByChain:         map[string]int{},
		SkippedOffChain: map[string]int{},
	}
}

// ---------------------------------------------------------------------------
// ScamSniffer
// ---------------------------------------------------------------------------

// ParseScamSniffer reads blacklist/address.json, a flat array of addresses.
//
// The feed carries no per-address metadata — no category, no chain, no
// description, no report date. Every entry is therefore labelled `scam` with
// the same confidence, and the evidence records only that the address appeared
// on the list. Inventing a richer category from an address string would be
// fabrication.
func ParseScamSniffer(ctx context.Context, r io.Reader) (*AbuseResult, []Label, error) {
	res := newAbuseResult()

	var addresses []string
	if err := json.NewDecoder(r).Decode(&addresses); err != nil {
		return nil, nil, fmt.Errorf("scamsniffer: decode address list: %w", err)
	}
	res.EntriesScanned = len(addresses)

	var out []Label
	seen := map[string]bool{}

	for _, raw := range addresses {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}

		addr := strings.TrimSpace(raw)
		if addr == "" {
			continue
		}
		res.AddressesFound++

		chainID, ok := chainFromShape(addr)
		if !ok {
			res.SkippedOffChain["unrecognised"]++
			continue
		}

		canonical, err := canonicalise(chainID, addr)
		if err != nil {
			res.Malformed = append(res.Malformed, fmt.Sprintf("%s: %v", addr, err))
			continue
		}

		key := chainID + "/" + canonical
		if seen[key] {
			continue
		}
		seen[key] = true

		out = append(out, Label{
			Chain:      chainID,
			Address:    canonical,
			Entity:     "Reported to ScamSniffer",
			Category:   "scam",
			Confidence: 0.5,
			Source:     "scamsniffer",
			Evidence: map[string]any{
				"source_list": "scamsniffer/scam-database blacklist/address.json",
				"listed_as":   addr,
				"note": "Unverified community report. The feed carries no category, " +
					"date or description for individual addresses.",
			},
		})
		res.LabelsProduced++
		res.ByChain[chainID]++
	}

	return res, out, nil
}

// ---------------------------------------------------------------------------
// CryptoScamDB
// ---------------------------------------------------------------------------

// csdbEntry is one record in data/urls.yaml or data/uris.yaml.
type csdbEntry struct {
	Name        string              `yaml:"name"`
	URL         string              `yaml:"url"`
	Category    string              `yaml:"category"`
	Subcategory string              `yaml:"subcategory"`
	Description string              `yaml:"description"`
	Reporter    string              `yaml:"reporter"`
	Addresses   map[string][]string `yaml:"addresses"`
}

// ParseCryptoScamDB reads the YAML blacklist.
//
// Unlike ScamSniffer this feed does carry structure — a category, a
// subcategory, a description and the reporting URL — so the label can say
// something specific about why an address was reported.
func ParseCryptoScamDB(ctx context.Context, r io.Reader) (*AbuseResult, []Label, error) {
	res := newAbuseResult()

	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, fmt.Errorf("cryptoscamdb: read: %w", err)
	}

	var entries []csdbEntry
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		return nil, nil, fmt.Errorf("cryptoscamdb: parse yaml: %w", err)
	}
	res.EntriesScanned = len(entries)

	var out []Label
	seen := map[string]bool{}

	for _, e := range entries {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}

		category := csdbCategory(e.Category)

		for assetCode, addrs := range e.Addresses {
			chainID, inScope := chainForAssetCode(assetCode)

			for _, raw := range addrs {
				addr := strings.TrimSpace(raw)
				if addr == "" {
					continue
				}
				res.AddressesFound++

				if !inScope {
					res.SkippedOffChain[assetCode]++
					continue
				}

				canonical, err := canonicalise(chainID, addr)
				if err != nil {
					res.Malformed = append(res.Malformed,
						fmt.Sprintf("%s (%s, %q): %v", addr, assetCode, e.Name, err))
					continue
				}

				key := chainID + "/" + canonical
				if seen[key] {
					// The same address can be reported under several URLs.
					// One row per address per source; the first wins, and the
					// ordering of the file is stable, so this is deterministic.
					continue
				}
				seen[key] = true

				entity := strings.TrimSpace(e.Name)
				if entity == "" {
					entity = "Reported to CryptoScamDB"
				}

				out = append(out, Label{
					Chain:      chainID,
					Address:    canonical,
					Entity:     entity,
					Category:   category,
					Confidence: 0.5,
					Source:     "cryptoscamdb",
					Evidence: map[string]any{
						"source_list":  "CryptoScamDB/blacklist",
						"listed_as":    addr,
						"asset_code":   assetCode,
						"reported_url": e.URL,
						"category":     e.Category,
						"subcategory":  e.Subcategory,
						"description":  e.Description,
						"reporter":     e.Reporter,
						"note":         "Unverified community report.",
					},
				})
				res.LabelsProduced++
				res.ByChain[chainID]++
			}
		}
	}

	return res, out, nil
}

// csdbCategory maps the feed's vocabulary onto ours.
//
// Its categories are Phishing, Scamming, Malware and Hacked. The first three
// are all fraud against a victim and map to `scam`. "Hacked" is a compromised
// site rather than a claim about the address, and also maps to `scam` because
// funds sent to it were taken by fraud — the mechanism differs, the meaning
// for a screening result does not.
func csdbCategory(in string) string {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(in), "'\"")) {
	case "phishing", "scamming", "malware", "hacked":
		return "scam"
	default:
		// An unrecognised category is still a report of abuse. Defaulting to
		// `scam` keeps it visible; silently dropping it would lose a report,
		// and inventing a new category would break the controlled vocabulary.
		return "scam"
	}
}

// chainFromShape infers a chain from an address's format, for feeds that do
// not say which chain they mean.
func chainFromShape(addr string) (string, bool) {
	switch {
	case strings.HasPrefix(addr, "0x") && len(addr) == 42:
		// Ambiguous between EVM chains. Ethereum is the convention for an
		// unqualified 0x address in these feeds.
		return "ethereum", true
	case strings.HasPrefix(addr, "T") && len(addr) == 34:
		return "tron", true
	default:
		return "", false
	}
}

// chainForAssetCode maps CryptoScamDB's asset codes to chains.
func chainForAssetCode(code string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "ETH":
		return "ethereum", true
	case "TRX":
		return "tron", true
	case "BNB":
		// As with OFAC, BNB covers two chains and the feed does not say which.
		// Out of scope rather than guessed.
		return "", false
	default:
		// BTC, XRP, LTC, BCH, DOGE, NEO, ADA, DOT, XMR and the rest.
		return "", false
	}
}
