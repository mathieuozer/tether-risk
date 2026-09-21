# Enabling Ethereum and BSC with Alchemy

Everything below is built and tested. The only missing piece is a key.

## 1. Get a key

Create an app at <https://dashboard.alchemy.com> for **Ethereum Mainnet**, and
a second one for **BNB Smart Chain** if you want BSC. Copy the HTTPS URL, which
looks like:

```
https://eth-mainnet.g.alchemy.com/v2/YOUR_KEY
```

## 2. Point the engine at it

SPEC.md §2 forbids secrets in the repository, so the key lives only in the
environment:

```bash
export ETH_RPC_URL="https://eth-mainnet.g.alchemy.com/v2/YOUR_KEY"
export BSC_RPC_URL="https://bnb-mainnet.g.alchemy.com/v2/YOUR_KEY"
```

The ingester detects an Alchemy host and selects the per-address transfers
adapter automatically. Any other endpoint falls back to the `eth_getLogs`
adapter, which works for block ranges but cannot serve per-address history.

## 3. Flip the chain on

In `config/sources.yaml`, change `status: unavailable` to `status: allowed`
for `ethereum` (and `bsc`), and delete its `unavailable_reason`. The config
loader requires a reason for every unavailable chain, so leaving both would
fail validation.

## 4. Verify before trusting it

```bash
ALCHEMY_RPC_URL="$ETH_RPC_URL" go test ./internal/chain/evm/ -run Live -v
```

This fetches a real address's history and checks every transfer validates. It
also reports how many amounts came from the lossy float field rather than the
exact hex one — see below.

## 5. Ingest and screen

```bash
go run ./cmd/ingest -chain ethereum -depth 1 fetch 0xSOMEADDRESS
go run ./cmd/price backfill
go run ./cmd/screen -chain ethereum 0xSOMEADDRESS
```

---

## What this unlocks

The label set already holds **5,621 Ethereum addresses** that are currently
inert because nothing can be traced against them:

| Source | Ethereum labels |
|---|---|
| CryptoScamDB | 2,967 |
| ScamSniffer | 2,530 |
| OFAC | 124 |

Coverage on Ethereum addresses should be meaningfully non-zero from the first
query, which is more than can currently be said for TRON.

## Two things to know before reading the numbers

**Amounts come from `rawContract.value`, not `value`.** The API's top-level
`value` is a JSON float already divided by the token's decimals, so for a large
USDT amount it has lost precision before it reaches us. The adapter always
prefers the exact hex integer. Where only the float exists — some native
transfers — the transfer is marked `unpriced_approx_amount` so the imprecision
travels with it rather than being forgotten. The live test reports how often
that happens.

**A complete history takes two passes.** The API filters on `fromAddress` *or*
`toAddress`, never both, so the adapter drains outbound then inbound and the
cursor tracks each independently. An implementation that queried one direction
would silently return half the history, which for a receive-only address would
mean no history at all.

## Cost

`alchemy_getAssetTransfers` costs 120 compute units per call, and the adapter
requests 1,000 transfers per call. Alchemy's free tier is generous but finite;
`-depth` on the ingester controls how far neighbour expansion goes and is the
main lever on how many calls a single screening makes. Start at `-depth 1`.
