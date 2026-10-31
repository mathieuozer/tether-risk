package screen

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/mozer/tether-risk/internal/pricing"
	"github.com/mozer/tether-risk/internal/scoring"
	"github.com/shopspring/decimal"
)

// nativeAssets are the chains' own coins. Their balance moves through fees,
// staking and resource rental as well as transfers, so a net of transfers
// is not a balance and is never shown as one.
var nativeAssets = map[string]bool{"TRX": true, "ETH": true, "BNB": true}

// activity summarises the address's own stored history.
//
// Each direction reads the table ordered by that direction's address:
// `transfers` for outbound, `transfers_by_to` for inbound (docs/DECISIONS.md
// D7), so both are range reads rather than scans.
func activity(ctx context.Context, ch *sql.DB, chainID, address string) (*scoring.Activity, error) {
	out := &scoring.Activity{InUSD: decimal.Zero, OutUSD: decimal.Zero}
	assets := map[string]*scoring.AssetFlow{}
	unpricedTokens := map[string]bool{}
	rawIn, rawOut := map[string]*big.Int{}, map[string]*big.Int{}

	for _, dir := range []struct {
		inbound  bool
		byAsset  string
		distinct string
	}{
		{
			inbound: false,
			byAsset: `SELECT asset, count(), countIf(usd_value IS NULL), sum(usd_value),
			                 min(block_time), max(block_time), sum(raw_value)
			          FROM transfers FINAL
			          WHERE chain = ? AND from_address = ? GROUP BY asset`,
			distinct: `SELECT uniqExact(to_address) FROM transfers FINAL
			           WHERE chain = ? AND from_address = ?`,
		},
		{
			inbound: true,
			byAsset: `SELECT asset, count(), countIf(usd_value IS NULL), sum(usd_value),
			                 min(block_time), max(block_time), sum(raw_value)
			          FROM transfers_by_to FINAL
			          WHERE chain = ? AND to_address = ? GROUP BY asset`,
			distinct: `SELECT uniqExact(from_address) FROM transfers_by_to FINAL
			           WHERE chain = ? AND to_address = ?`,
		},
	} {
		rows, err := ch.QueryContext(ctx, dir.byAsset, chainID, address)
		if err != nil {
			return nil, fmt.Errorf("activity: %w", err)
		}
		for rows.Next() {
			var (
				asset       string
				n, unpriced uint64
				usd         *decimal.Decimal
				first, last time.Time
				raw         big.Int
			)
			if err := rows.Scan(&asset, &n, &unpriced, &usd, &first, &last, &raw); err != nil {
				rows.Close()
				return nil, fmt.Errorf("activity: %w", err)
			}

			if out.FirstSeen.IsZero() || first.Before(out.FirstSeen) {
				out.FirstSeen = first
			}
			if last.After(out.LastSeen) {
				out.LastSeen = last
			}

			if unpriced == n && isContractAsset(asset) {
				// An unrecognised token: the TRON adapter names those by
				// their contract (docs/DECISIONS.md D18). A recognised asset
				// with a pricing gap stays listed, valued at what is known.
				out.UnpricedTransfers += n
				unpricedTokens[asset] = true
				continue
			}

			a, ok := assets[asset]
			if !ok {
				a = &scoring.AssetFlow{Asset: asset, InUSD: decimal.Zero, OutUSD: decimal.Zero}
				assets[asset] = a
			}
			a.Transfers += n
			v := decimal.Zero
			if usd != nil {
				v = *usd
			}
			if dir.inbound {
				out.InTransfers += n
				out.InUSD = out.InUSD.Add(v)
				a.InUSD = a.InUSD.Add(v)
				rawIn[asset] = new(big.Int).Set(&raw)
			} else {
				out.OutTransfers += n
				out.OutUSD = out.OutUSD.Add(v)
				a.OutUSD = a.OutUSD.Add(v)
				rawOut[asset] = new(big.Int).Set(&raw)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()

		// Counted once across all assets: a counterparty trading two tokens
		// is still one counterparty.
		var counterparties uint64
		if err := ch.QueryRowContext(ctx, dir.distinct, chainID, address).Scan(&counterparties); err != nil {
			return nil, fmt.Errorf("activity: %w", err)
		}
		if dir.inbound {
			out.InCounterparties = counterparties
		} else {
			out.OutCounterparties = counterparties
		}
	}

	for _, a := range assets {
		out.Assets = append(out.Assets, *a)
	}
	sort.Slice(out.Assets, func(i, j int) bool {
		ti := out.Assets[i].InUSD.Add(out.Assets[i].OutUSD)
		tj := out.Assets[j].InUSD.Add(out.Assets[j].OutUSD)
		if !ti.Equal(tj) {
			return ti.GreaterThan(tj)
		}
		return out.Assets[i].Asset < out.Assets[j].Asset
	})
	out.UnpricedTokens = uint64(len(unpricedTokens))
	out.Held = held(rawIn, rawOut)
	return out, nil
}

// held nets each token's stored inflow against its outflow. A token whose
// net is negative has inflow missing from the store, so nothing is claimed
// for it; the caller drops the whole list when history is known to be
// incomplete.
func held(rawIn, rawOut map[string]*big.Int) []scoring.AssetHeld {
	var out []scoring.AssetHeld
	for asset, in := range rawIn {
		dec, ok := pricing.Decimals(asset)
		if !ok || nativeAssets[asset] {
			continue
		}
		net := new(big.Int).Set(in)
		if o := rawOut[asset]; o != nil {
			net.Sub(net, o)
		}
		if net.Sign() < 0 {
			continue
		}
		out = append(out, scoring.AssetHeld{Asset: asset, Amount: decimal.NewFromBigInt(net, -dec)})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Amount.Equal(out[j].Amount) {
			return out[i].Amount.GreaterThan(out[j].Amount)
		}
		return out[i].Asset < out[j].Asset
	})
	return out
}

// isContractAsset reports whether an asset is named by its contract address,
// which is how the TRON adapter names every token it does not recognise.
func isContractAsset(asset string) bool {
	return len(asset) == 34 && asset[0] == 'T'
}
