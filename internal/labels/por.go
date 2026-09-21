package labels

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Exchange proof-of-reserves address lists.
//
// Some exchanges publish the addresses behind their reserve attestations,
// each with a signed message proving control. Self-published and
// authoritative about the exchange's own wallets, these are the cleanest
// open source of named exchange addresses (docs/DECISIONS.md D19).
//
// They are labelled unnamed_service, not exchange. Whether an exchange has
// functioning KYC is what separates `exchange` (weight 2) from
// `high_risk_exchange` (weight 40), and a reserve list says nothing about it.
// The exchange's name is recorded as the entity, so the address is identified
// even though its risk tier is not asserted (the same rule as D15).

// PoRResult summarises one proof-of-reserves list.
type PoRResult struct {
	RowsScanned int
	TronRows    int
	Addresses   int
	Signed      int
	Malformed   []string
}

// ParsePoRCSV reads a proof-of-reserves CSV in the layout HTX and Poloniex
// publish: per-address rows of coin, address, snapshot height, balance,
// signed message and signature, interleaved with three-column summary rows.
//
// Only TRON addresses are kept. An address listed under several coins becomes
// one label whose evidence names every coin.
func ParsePoRCSV(ctx context.Context, r io.Reader, exchange, sourceID, sourceURL string, confidence float64) (*PoRResult, []Label, error) {
	res := &PoRResult{}

	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // summary and address rows differ in width
	cr.LazyQuotes = true

	type entry struct {
		coins  []string
		signed bool
		height string
	}
	byAddress := map[string]*entry{}

	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}

		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: read csv: %w", sourceID, err)
		}
		res.RowsScanned++
		if len(rec) < 6 {
			continue // a summary row or a header
		}

		coin := strings.TrimPrefix(strings.TrimSpace(rec[0]), "\xef\xbb\xbf")
		raw := strings.TrimSpace(rec[1])
		if coin == "coin" || !strings.HasPrefix(raw, "T") || len(raw) != 34 {
			continue
		}
		res.TronRows++

		addr, err := canonicalise("tron", raw)
		if err != nil {
			res.Malformed = append(res.Malformed, fmt.Sprintf("%s: %v", raw, err))
			continue
		}

		e, ok := byAddress[addr]
		if !ok {
			e = &entry{height: strings.TrimSpace(rec[2])}
			byAddress[addr] = e
		}
		e.coins = append(e.coins, coin)
		sig := strings.TrimSpace(rec[5])
		if sig != "" && sig != "-" {
			e.signed = true
		}
	}

	addrs := make([]string, 0, len(byAddress))
	for a := range byAddress {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs) // deterministic output (docs/DECISIONS.md D6)

	out := make([]Label, 0, len(addrs))
	for _, a := range addrs {
		e := byAddress[a]
		sort.Strings(e.coins)
		if e.signed {
			res.Signed++
		}
		out = append(out, Label{
			Chain:      "tron",
			Address:    a,
			Entity:     exchange + " (proof-of-reserves wallet)",
			Category:   "unnamed_service",
			Confidence: confidence,
			Source:     sourceID,
			Evidence: map[string]any{
				"source_list":      sourceURL,
				"coins":            e.coins,
				"snapshot_height":  e.height,
				"ownership_signed": e.signed,
				"note": "Published by the exchange as part of its own reserve attestation. " +
					"Labelled unnamed_service because the list establishes who controls the " +
					"address, not the exchange's KYC standard (docs/DECISIONS.md D19).",
			},
		})
	}
	res.Addresses = len(out)
	return res, out, nil
}
