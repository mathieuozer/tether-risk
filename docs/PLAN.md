# PLAN.md — implementation plan for the AML risk screening engine

Derived from `SPEC.md`. This file records *how* the spec will be built and
every place where building it honestly requires deviating from it.

Status key: `[ ]` not started, `[~]` in progress, `[x]` done and gated.

---

## 0. Confirmed operating assumptions

| Question | Answer | Consequence |
|---|---|---|
| Chain data access | TronGrid free tier only | TRON is the only chain with real data in v1. ETH/BSC adapters are built behind the interface and exercised by fixtures. See F1. |
| Build cadence | Run Phases 0–3 continuously, report at the Phase 3 gate | Phase 1 and 2 acceptance criteria are still evaluated and recorded, just not stopped at. |
| Repository | `git init`, Java scaffold removed, module `github.com/mozer/tether-risk` | — |

---

## 1. Honest flags — where open data cannot deliver what the spec asks

SPEC.md §11 requires these be raised rather than papered over. Each one gets a
matching entry in `docs/DECISIONS.md` and a `status:` field in `sources.yaml`.

**F1 — ETH and BSC have no live data path.** *(Updated 2026-09-21: the
adapter is now built and verified against a live endpoint — it parsed 563 real
USDT transfers from five blocks. The blocker is narrower than originally
stated: recent blocks work on public endpoints, but archive access does not,
so per-address history remains impossible without a provider key. See
docs/METHODOLOGY.md §2.)*
The spec's bulk backfill is BigQuery (`bigquery-public-data.crypto_ethereum`),
which needs a GCP billing account, and the recent tail needs a JSON-RPC
endpoint. Neither is available. Both adapters will be written to the same
`Adapter` interface and covered by golden fixtures, so they are complete and
testable code — but no ETH or BSC address can be scored against real chain
data until credentials exist. The API will return an explicit
`chain_unavailable` error for those chains rather than a misleading empty
result.

**F2 — Block-explorer labels are very likely un-ingestible.**
SPEC.md §6.2 asks for Etherscan, BscScan and Tronscan public labels at
confidence 0.9, and explicitly instructs: record each site's terms of use, and
*stop rather than work around* anything prohibiting redistribution. Etherscan
does not expose address nametags/labels through its API at all; the labels
exist only on HTML pages, and its Terms prohibit scraping and redistribution.
So the instruction resolves to: **do not ingest them.** `sources.yaml` will
carry `status: blocked` with the ToS clause quoted and the date checked. No
scraper will be written. The lost coverage is absorbed by Dune spellbook
(open-licensed), OFAC, the curated mixer list, and the derived-deposit
heuristic — and the shortfall shows up honestly in the coverage figure rather
than being hidden.

**F3 — USD valuation for native assets is approximate.**
Dust classification ($1 threshold) and haircut splitting both need
`usd_value`, but no price feed credential exists. Plan: stablecoins pinned to
1.0 via config; native assets (TRX, ETH, BNB) valued from a daily close series
loaded from a free public source into a `prices` table. Daily granularity on a
volatile asset introduces real error on individual transfers. This is recorded
in METHODOLOGY.md as a known limitation, and `usd_value` carries a
`price_confidence` so a score driven by imprecisely-valued native flows can be
distinguished from one driven by stablecoins.

**F4 — "External comparison" (§9.5) cannot be fully automated.**
Obtaining public scores for 100 addresses from an existing service means
either a paid API or manual collection. The harness will be built to consume a
checked-in CSV of externally-obtained scores and emit the divergence table;
populating that CSV is a manual step, documented as such.

---

## 2. Design decisions that deviate from the literal spec

Each becomes an entry in `docs/DECISIONS.md`.

**D1 — `transfers` uses `ReplacingMergeTree`, not `MergeTree`.**
The spec asks for `MergeTree ORDER BY (chain, from_address, block_time)` *and*
deduplication on `(chain, tx_hash, log_index)`. A plain MergeTree cannot
deduplicate. Resolution:
`ReplacingMergeTree ORDER BY (chain, from_address, block_time, tx_hash, log_index)`
— keeps the spec's ordering as a prefix, adds the uniqueness the spec requires.

**D2 — the `edges` materialized view double-counts on re-ingestion. This is the
single most dangerous trap in the data model.**
A ClickHouse MV fires per `INSERT`, not per surviving row. Re-running an
ingestion job re-inserts rows that `ReplacingMergeTree` will later collapse in
`transfers` — but the `AggregatingMergeTree` behind `edges` has *already*
summed them. Every value in `edges` would silently inflate, and `edges` is
what all traversal reads. Mitigation, all three:
1. **Dedupe before insert.** Ingestion filters each batch against existing
   `(chain, tx_hash, log_index)` keys before writing. Single-writer-per-address
   job leases in Postgres make the read-then-write safe.
2. **Ingest ledger.** `ingest_batches` in Postgres records what each job wrote,
   so a re-run is a no-op rather than a re-insert.
3. **Deterministic rebuild.** `cmd/ingest rebuild-edges` recomputes `edges`
   from deduplicated `transfers`. This is ground truth; a test asserts the
   incremental MV and the rebuild agree.

**D3 — the `Adapter` interface needs an address-oriented method.**
§5 defines `Head` + `FetchRange(from, to)` (block-range), but ingestion is
specified as demand-driven per address. Adding:
```go
FetchAddress(ctx, addr string, cur Cursor) (AddressPage, error)
```
`FetchRange` is retained for optional full-chain backfill.

**D4 — `labels` becomes slowly-changing-dimension type 2.**
The spec wants `UNIQUE (chain, address, source)` *and* a `snapshot_id` on every
row *and* determinism against a named snapshot. Those conflict: one row per
source cannot also be one row per snapshot. Copying the whole table per daily
snapshot is ~365M rows/year. Resolution: keep one row per
`(chain, address, source, valid_from_snapshot)` with a `valid_to_snapshot`.
Resolution at snapshot *S* selects rows where
`valid_from <= S < valid_to`. Compact, append-only, fully auditable.

**D5 — value arithmetic uses `shopspring/decimal`, not `float64`.**
Determinism and auditability outrank speed in the haircut accumulation. Floats
only at the presentation boundary.

**D6 — determinism requires total ordering everywhere.**
Neighbours sorted by value descending, ties broken by address ascending. No
map iteration order may reach output. No wall-clock in scoring. A test re-runs
the same address against the same snapshot and asserts byte-identical output.

---

## 3. Phase 0 — foundations

- [x] `go.mod`, Go-appropriate `.gitignore`, `Makefile` (`up`, `down`,
      `migrate`, `test`, `lint`, `check-terminology`)
- [x] `docker-compose.yml`: ClickHouse + PostgreSQL, pinned versions, healthchecks
- [x] Migrations: numbered SQL, Postgres runner + ClickHouse DDL applier
- [x] `internal/config`: loads `weights.yaml` + `sources.yaml`, validates the
      category vocabulary both ways, exposes `ConfigVersion` = SHA-256 of both
      files, stamped on every result
- [x] `config/weights.yaml` with the spec's starting weights, bands, hop limit,
      decay, dust threshold, fan-out cap, deposit-heuristic thresholds
- [x] `config/sources.yaml` with per-source licence, ToS status and check date
- [x] `make check-terminology`: fails the build if the SPEC.md §2 banned term appears anywhere
      (SPEC.md §2)
- [x] `docs/DECISIONS.md` and `docs/METHODOLOGY.md` started, written as we go

**Libraries:** `clickhouse-go/v2`, `pgx/v5`, `yaml.v3`, `shopspring/decimal`,
stdlib `net/http` + `log/slog`. No web framework — §11 asks for boring,
inspectable code.

## 4. Phase 1 — ingestion

- [x] `internal/chain`: `Transfer`, `Cursor`, `Adapter` (per D3)
- [x] `internal/chain/tron`: TronGrid adapter — native + TRC-20 endpoints,
      fingerprint pagination, token-bucket rate limiting, exponential backoff
      with jitter, cursor persisted in Postgres
- [x] `internal/chain/evm`: JSON-RPC and BigQuery adapters behind the same
      interface, fixture-driven (F1)
- [x] `internal/store/clickhouse`: batched idempotent writer (D2), edges rebuild
- [x] `internal/store/postgres`: cursors, job queue, ingest ledger, run metadata
- [x] `cmd/ingest`: worker pool, demand-driven address jobs with hop-depth
      neighbour enqueue, TTL cache, `rebuild-edges` subcommand

**Gate:** any TRON address → full history persisted → `edges` populated.
Cold query for a 1,000-transfer address < 30 s, warm < 2 s. Measured and recorded.

## 5. Phase 2 — labels

- [x] Schema per D4 + `label_snapshots`
- [x] OFAC SDN XML parser (digital currency address fields), UN and EU lists,
      `confidence 1.0`, daily
- [x] Block-explorer sources: **blocked, documented, not built** (F2)
- [ ] Dune spellbook ingester, `confidence 0.8`
- [ ] Abuse feeds, `confidence 0.5`, with the hard rule that a lone abuse report
      can never alone push an address into a high band
- [x] Curated mixer/protocol list checked into the repo
- [x] Derived deposit-wallet heuristic, config-driven thresholds, reasoning
      written to `evidence`
      *(2026-09-21: the command fed it an empty label map, so it never ran
      end to end until then. See DECISIONS.md D20.)*
- [ ] Precision measurement against a hand-labelled sample → METHODOLOGY.md
- [x] Resolution rules: highest confidence wins, ties by source priority,
      sanctions always win, conflicts recorded never silently merged

**Gate:** per-source label counts reported; every OFAC address resolves;
derived-deposit precision measured and written down.

## 6. Phase 3 — traversal and scoring

- [x] `internal/graph`: BFS over `edges`, one direction at a time, terminal at
      labelled nodes, fan-out cap with truncation flag, cycle detection
- [x] `internal/scoring`: haircut propagation, decay, dust handling, category
      accumulation, score, bands, sanctions override
- [x] Coverage = attributed / total traced, reported always; < 40% marks the
      result low-confidence
- [x] Path persistence so any score reconstructs from stored rows
- [x] Golden fixtures in `/testdata`; determinism and weight-change tests

**Gate:** deterministic across runs; every score carries a reconstructing path
set; changing a YAML weight changes output with no code change.

## 7. Phases 4–5 — after the Phase 3 report

API, Telegram bot, PDF report; then the `cmd/validate` harness with sanctions
recall, false-positive, dust-resistance, stability and external-comparison
checks.

---

## 8. Open question deferred to the Phase 3 gate

Coverage semantics when traversal is truncated: a fan-out cap or hop-limit stop
leaves value neither attributed nor provably unattributable. The plan treats it
as **unattributed** (lowering coverage) on the principle that the spec would
rather understate confidence than overstate it — but this materially affects
reported coverage and is worth confirming with real numbers in front of us.
