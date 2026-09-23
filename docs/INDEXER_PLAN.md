# Plan: our own TRON index

Status: 2026-09-23. The indexer (pkg/tronindex, cmd/indexer) is built and tested; the node and the server are not.

## Why

Today every screen fetches an address's history from TronGrid on demand.
That has three costs.

1. **Quota.** The free key's 100,000 requests a day ran out on
   2026-09-22, and the Tether blacklist refresh failed for four hours (D35).
   TronGrid sells higher tiers only as unpriced custom plans.
2. **Speed.** A cold screen spends 1 to 12 seconds fetching (D38).
3. **Accuracy, the largest cost.** Tracing stops at every counterparty
   whose history was never fetched. Those dead ends count as unknown, and
   they are why coverage on a typical wallet sits between 10% and 80%. The
   service, deposit, hot-wallet and operator heuristics (D15, D25, D28, D31)
   have only ever seen the few hundred thousand addresses near what someone
   screened, never the whole chain.

With every USDT and TRX transfer on TRON in our own ClickHouse, the first two
disappear. The third becomes a matter of running the heuristics over the
whole graph.

## What we are not building

A general TronGrid replacement. We need two things only:

- every USDT and TRX transfer (plus the other stablecoins), queryable by
  sender and by recipient: exactly `transfers` and `transfers_by_to`;
- the events of a few contracts: Tether's blacklist and the JustSwap pool.

Both are tables we already have, or small additions to them. Nothing outside
this service reads the index.

## Architecture

```
java-tron FullNode ──HTTP (solidified blocks only)──▶ tron-indexer ──▶ ClickHouse `tron_index` db
      (local)                                         │                   transfers / transfers_by_to
                                                      │                   edges (MV) / contract_events
                                                      └─ cursor + ledger in PostgreSQL
screen / labeler ──read──▶ `tron_index` (after cutover)     TronGrid kept as fallback
```

- **Node:** java-tron FullNode, started from an official snapshot (about
  3.3 TB on 21–22 September 2026) rather than synced from genesis, which
  takes weeks to months. It serves HTTP on localhost only.
- **Indexer:** a new command, `cmd/indexer`, with two modes:
  - `tail`: follows the solidified head and writes each block's transfers;
  - `backfill -from N -to M`: walks a block range. It runs in parallel over
    disjoint ranges, each resumable from its own cursor.
- **Reading a block:** `/walletsolidity/getblockbynum` gives the
  transactions, whose `TransferContract` entries are the native TRX
  transfers. `/walletsolidity/gettransactioninfobyblocknum` gives the logs,
  whose TRC-20 `Transfer` events (topic `ddf252ad…`) are the token
  transfers, and a contract's other events.
- **A separate database, `tron_index`.** The backfill writes there, not into
  the live `tether_risk` tables. Live screens are untouched until cutover,
  cutover is a configuration switch, and rollback is switching back.

## The one thing that must not go wrong: identical keys

Rows already stored (23 million on TRON) came from TronGrid. TronGrid's TRC-20
endpoint returns no log index, so D11 derives one by hashing
`(from, to, token contract, value)`. The indexer must produce, for the same
transfer:

- the same `tx_hash`;
- the same synthetic `log_index`: the same four strings (base58 addresses,
  token contract in base58, the value as a decimal string) hashed by the same
  function;
- the same `asset` name, since D18 maps contracts to names;
- the same `block_time`.

Otherwise, after cutover, a transfer already stored from TronGrid would be
stored again from the index, and the edges would count it twice (D2, D37).
The parsing lives in one shared function (`tron.TransferFromLog`), which the
adapter and the indexer both call. A golden test takes real transactions and
checks that their TronGrid rows and their node rows are byte-identical.

Native TRX already uses the genuine position in `raw_data.contract` (D11), so
it matches by construction.

Two improvements the node makes possible, kept out of the first cut so the
keys stay comparable:

- TRC-20 rows could carry their real block number (D12 stores 0);
- internal TRX transfers made by contracts are visible in the transaction
  info. The adapter has never stored these.

## Scope of the first cut

- **Assets:** TRX (native `TransferContract`), USDT, and the other priced
  stablecoins (USDC, TUSD, USDD).
- **Not indexed:** other TRC-20 tokens, most of them spam. The adapter stores
  them today, unpriced, and the report counts them as "unrecognised tokens"
  (D16). After cutover that count reads zero. It is noted in METHODOLOGY
  rather than hidden. Address-poisoning dust arrives in USDT and TRX, so it
  is kept.
- **Contract events:** Tether's `AddedBlackList`, `RemovedBlackList` and
  `DestroyedBlackFunds`, and the JustSwap pool's `Snapshot`. With these, the
  blacklist refresh (D34) and the TRX price (D41) come from our own node too.

## Measurement (2026-09-23, 300 blocks through Alchemy)

`indexer -source alchemy -sample 300 measure`: 300 blocks spread evenly over
blocks 8,000,000 to 86,499,810 (USDT on TRON begins at about 8 M). A first
20-block run through TronGrid gave the same picture (9.71 billion, 3.09 TB).

| Year | tx/block | kept/block | TRX | USDT | other stablecoins |
|---|---|---|---|---|---|
| 2019 | 71 | 4.8 | 4.7 | 0.1 | 0 |
| 2020 | 47 | 7.1 | 6.8 | 0.3 | 0 |
| 2021 | 125 | 59.9 | 33.1 | 26.7 | 0.1 |
| 2022 | 204 | 161.2 | 110.9 | 49.6 | 0.5 |
| 2023 | 230 | 176.0 | 108.7 | 66.7 | 0.6 |
| 2024 | 203 | 148.8 | 81.6 | 67.0 | 0.1 |
| 2025 | 299 | 206.8 | 132.0 | 74.8 | 0 |
| 2026 | 406 | 243.7 | 165.2 | 78.4 | 0 |

- **About 9.8 billion transfers** in scope, not "several billion". TRX is
  about 64% of them (6.3 billion); USDT and the other stablecoins about 3.6
  billion.
- **About 3.1 TB in ClickHouse**, at the live database's own 318 bytes a
  transfer (transfers and edge tables together), not 0.5–1.5 TB. With the
  node's 3.3 TB, that is about 7–8 TB. Stablecoins alone: about 1.15 TB.
- **Backfilling through a hosted endpoint is impractical**: 1.19 seconds a
  block (two requests), so about 270 days with 4 streams. It is also about
  157 million requests, far beyond Alchemy's free monthly allowance. It
  needs the local node, where a block should take milliseconds; phase 1
  measures that.
- Enabling TRON on an Alchemy app takes a few minutes to reach every
  server: requests alternate between 200 and 403 until it has.

Consequences for scope. USDT and the other stablecoins alone are about 36%
of the rows, about 1.15 TB, and USDT is the product's subject. TRX could
follow later, or be kept above a threshold. Most TRX transfers are tiny, but
address-poisoning dust (D32) is made of them, so a threshold costs that
signal on TRX; the owner decides (see below).

## Size

| Item | Estimate | Measured in |
|---|---|---|
| Blocks | ~86.5 M; USDT exists from about block 8 M (2019) | phase 0 |
| Transfers in scope | 9.8 billion (3.6 billion stablecoins) | measured, 300 blocks |
| ClickHouse on disk | 3.1 TB with edges (1.15 TB stablecoins only) | measured, at today's 318 bytes a transfer |
| Node | 3.3 TB, growing | snapshot size |
| Backfill speed | unknown; needs to reach about 100 blocks/s across workers to finish in about 10 days | phase 0 |

The server has to hold the node and the index: 16 or more cores, 128 GB RAM,
and, after the first measurement, about 5 TB for USDT and stablecoins or
about 8 TB with TRX: 2 × 7.68 TB NVMe at least. A Hetzner AX102 class machine is about €250–350 a
month (unconfirmed). Split into a node server and a database server if one
machine cannot keep up.

## Phases

Each phase ends with a check that must pass before the next starts.

### Phase 0: measurement (2 days)
- Read 100,000 blocks spread across history through a hosted TRON RPC, such
  as Alchemy's, parse them with the shared function, and write them to a
  scratch database.
- Measure rows per block by era, bytes per row, and blocks per second per
  worker.
- **Check:** projected disk use, backfill duration, and the choice of one
  server or two, written into this document.

### Phase 1: the node (2–3 days, most of it waiting)
- Order the server. Install java-tron. Download and verify the snapshot.
  Start it, bound to localhost only.
- Add node health to the admin alerts: solidified head lag, and disk left.
- **Check:** the solidified head is within a minute of TronScan; the HTTP
  endpoints answer locally.

### Phase 2: parser and tail (4–5 days)
- Build `tron.TransferFromLog` and the native parser, both shared with the
  adapter.
- Golden test: 200 real transactions of mixed types (USDT, TRX, USDC,
  failed, multi-transfer, poisoning dust) produce byte-identical rows from
  TronGrid and from the node.
- `cmd/indexer tail` writes each solidified block into `tron_index`, with a
  block cursor in PostgreSQL and an idempotent write per block: the block
  number is the ledger key, so a replayed block is skipped.
- **Check:** after 24 hours of tailing, every transfer TronGrid lists for 50
  active addresses in that window is in the index, and nothing else is.

### Phase 3: backfill (4–6 days of code, then days of running)
- `cmd/indexer backfill` splits history into ranges, one worker per range,
  each with its own cursor, and bulk-inserts in blocks of about 100k rows.
- The edge views stay on during bulk insert. Every block is written exactly
  once, so they stay correct. `audit-edges` (D37) checks this at the end.
- Order: newest ranges first, so recent months are complete earliest.
- **Check:** every range reaches its end, and no block number is missing
  (checked by counting the block ledger).

### Phase 4: validation (2–3 days)
- `validate index`: for 1,000 random addresses (active, dormant, services,
  poisoned), compare USDT and TRX transfer counts and sums from TronGrid
  with the index. Every difference is explained or fixed.
- Rescreen the verdict benchmark (listed, exposed, deposit) against the
  index and compare it with today's answers.
- **Check:** at least 99.9% of sampled addresses identical; the benchmark
  has no new misses.

### Phase 5: cutover (2–3 days)
- A configuration switch points screening, the labeler and the price loader
  at `tron_index`.
- The first-ring and follow-up fetching (D23, D31, D38) becomes unnecessary
  on TRON: every counterparty is already there. Screens stop fetching.
  TronGrid stays as a fallback when the index lags by more than 5 minutes.
- The blacklist refresh and the TRX price read the node's events.
- The old `tether_risk` TRON tables stay, read-only, for a month for
  rollback.
- **Check:** a day of normal screening with no TronGrid calls, and no
  latency or error regressions.

### Phase 6: whole-chain labelling (1–2 weeks, afterwards)
- Run service, deposit, hot-wallet, operator (D31) and poisoning (D32)
  detection over the whole graph, not only the neighbourhood of screened
  addresses. Retune their thresholds, which were set on partial data.
- Measure coverage and the verdict benchmark before and after. This is where
  the accuracy gain shows, and it should be measured, not assumed.

**Total to cutover:** about 3–4 weeks of engineering, with the node sync and
the backfill running in the background.

## Operations after cutover

- **Node upgrades:** follow java-tron releases. Mandatory upgrades at hard
  forks cannot wait.
- **Disk:** the node and the index both grow. Alert at 80% full.
- **Backups:** the index can be rebuilt from the node and needs no backup.
  PostgreSQL (labels, customers, billing) is backed up daily off the
  server.
- **Monitoring:** alerts to admins for index lag, node lag, disk, and a
  sampled hourly comparison of 20 addresses against TronGrid (the canary).

## Risks

| Risk | Mitigation |
|---|---|
| Keys differ from the TronGrid rows, and transfers are counted twice | Shared parser, golden test, `validate index`, `audit-edges` |
| Backfill slower than estimated | Phase 0 measures first; newest-first order; parallel ranges; a second server if needed |
| A silent gap: a block or a transfer missed | Block ledger count, canary comparison, the lesson of D42 |
| The node falls behind or stops | Solidified-head alert; TronGrid fallback over 5 minutes of lag |
| Disk fills | Size from phase 0; 80% alert; scope limited to priced assets |
| Chain reorganisation | Read solidified blocks only |

## Decisions for the owner

1. When to start: before or after go-live. The recommendation is after,
   unless TronGrid quotes more than about $500 a month.
2. Buying the server, about €250–350 a month (unconfirmed).
3. The asset scope, in the light of the first measurement: USDT and
   stablecoins first (about 1.15 TB, recommended), with TRX later or above a
   threshold; or everything priced (about 3.1 TB).
4. Whether TronGrid stays as a paid fallback after cutover, or the free
   tier is enough.
