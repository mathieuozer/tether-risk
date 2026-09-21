# SPEC.md — On-Chain AML Risk Screening Engine

You are building a self-hosted, open-data crypto AML risk screening engine.
Read this file fully before writing any code. Build in the phase order given.
Do not skip ahead. At the end of each phase, stop and report against the
acceptance criteria before continuing.

---

## 1. Product definition

Given a blockchain address, return:

- a breakdown of that address's exposure by counterparty category
  (exchange, mixer, gambling, sanctions, scam, ...) as percentages of value,
  computed separately for **inbound** (where funds came from) and
  **outbound** (where funds went),
- a composite risk score 0–100,
- a **coverage figure**: what share of traced value we were actually able to
  attribute to a known entity,
- a human-readable explanation naming the specific paths that drove the score.

This is a **triage / pre-screening** tool. It is explicitly not a regulated
AML determination. Every API response and every report must carry that framing.

### Non-goals (do not build these)

- No UTXO clustering. No Bitcoin in v1.
- No machine learning. The scoring must be deterministic and auditable.
- No commercial data feeds (Chainalysis / TRM / Elliptic). Zero licence spend.
- No continuous monitoring or alerting in v1. Single-address query only.
- No user accounts / billing in v1.

### Chains in scope for v1

TRON, Ethereum, BSC. Native transfers plus ERC-20/TRC-20 token transfers,
with USDT as the priority asset.

---

## 2. Hard constraints

- **Language:** Go for all services. Python only for one-off ingestion and
  backfill scripts under `scripts/`.
- **Store:** ClickHouse for chain data. PostgreSQL for the label database and
  for run metadata. No graph database — the aggregated-edge table plus
  recursive SQL is the design.
- **Determinism:** identical input plus identical label snapshot must produce
  an identical score. Version the label snapshot and stamp its ID on every
  result.
- **Auditability:** every score must be reconstructible from stored
  intermediate data. Persist the traced paths, not just the final number.
- **Configuration over code:** category weights, hop limits, decay factors and
  thresholds all live in a single versioned YAML file, never inline in Go.
- **No secrets in the repo.** Config via environment variables.
- **Terminology:** never use the word "pilot" anywhere in code, comments, docs
  or output. First deployments are "production installations".

---

## 3. Repository layout

```
/cmd
  /api            HTTP API server
  /ingest         chain ingestion workers
  /labeler        label ingestion + derivation jobs
  /bot            Telegram bot
/internal
  /chain          per-chain adapters (tron, evm)
  /store          ClickHouse + Postgres access
  /graph          traversal
  /scoring        haircut propagation + scoring
  /labels         label resolution, confidence handling
  /report         explanation and PDF rendering
/config
  weights.yaml
  sources.yaml
/scripts          backfill and one-off ingestion
/testdata         golden fixtures
/docs
  METHODOLOGY.md
```

---

## 4. Data model

### ClickHouse: `transfers`

Raw normalised transfer events, one row per value movement.

```
chain          LowCardinality(String)
tx_hash        String
log_index      UInt32
block_number   UInt64
block_time     DateTime
from_address   String
to_address     String
asset          LowCardinality(String)   -- 'TRX','ETH','BNB','USDT',...
raw_value      UInt256
usd_value      Decimal(38,6)            -- nullable until Phase 3
```

Engine `MergeTree`, ordered by `(chain, from_address, block_time)`.
Add a materialized view ordered by `(chain, to_address, block_time)` — you
need both traversal directions to be fast.

Deduplicate on `(chain, tx_hash, log_index)`; ingestion must be idempotent
and safely re-runnable.

### ClickHouse: `edges`

Aggregated address-to-address flow. **All traversal reads this table, never
`transfers`.** This is the single most important performance decision in the
system.

```
chain, from_address, to_address, asset
total_value        Decimal(38,6)
transfer_count     UInt64
first_seen, last_seen  DateTime
```

Maintain as an `AggregatingMergeTree` materialized view over `transfers`.

### PostgreSQL: `labels`

```
id, chain, address
entity            -- 'Binance', 'Tornado Cash', 'OFAC SDN #1234'
category          -- controlled vocabulary, see weights.yaml
confidence        -- 0.0-1.0
source            -- 'ofac','etherscan','dune','chainabuse','derived:deposit'
evidence          jsonb
first_seen, last_updated
snapshot_id
```

Unique on `(chain, address, source)`. An address may carry several labels from
different sources; resolution rules are in §6.

---

## 5. Ingestion (Phase 1)

Each chain adapter implements:

```go
type Adapter interface {
    Head(ctx) (uint64, error)
    FetchRange(ctx, from, to uint64) ([]Transfer, error)
}
```

- **TRON:** TronGrid public API. Respect rate limits, exponential backoff,
  resumable cursor persisted in Postgres.
- **ETH / BSC:** start from the BigQuery public datasets
  (`bigquery-public-data.crypto_ethereum`) for bulk backfill, then switch to a
  JSON-RPC adapter for the recent tail. Keep the two behind the same interface.

Ingestion is **demand-driven**, not full-chain. When an address is queried and
we lack its history, enqueue a fetch job for that address's transfers and its
neighbours to the configured hop depth. Cache with a TTL. Full-chain backfill
is optional and must not be a prerequisite for a working query.

**Acceptance (Phase 1):** given any TRON address, the system persists its
complete transfer history and populates `edges`. A cold query for a
thousand-transfer address completes in under 30 seconds; a warm one in under 2.

---

## 6. Label database (Phase 2)

Label ingestion jobs, each idempotent, each writing its own `source`:

1. **OFAC SDN** — parse the published XML, extract crypto addresses. Also
   ingest UN and EU consolidated lists. Run daily. `confidence = 1.0`.
2. **Block explorer public labels** — Etherscan, BscScan, Tronscan.
   `confidence = 0.9`. **Check and record each site's terms of use in
   `sources.yaml` before ingesting; flag anything that prohibits redistribution
   and stop rather than working around it.**
3. **Dune spellbook labels** — clone the open repo, ingest the labels schema.
   `confidence = 0.8`.
4. **Abuse reports** — chainabuse, CryptoScamDB, ScamSniffer. `confidence = 0.5`,
   because these are unverified user reports. Never let a single abuse report
   alone push an address into a high-risk band.
5. **Mixer and known-protocol contracts** — curated list checked into the repo.

### Derived labels — the deposit-wallet heuristic

This is what closes most of the gap against commercial feeds. An address that
repeatedly forwards funds to a known exchange hot wallet, with little other
activity, is a deposit wallet of that exchange.

Implement it conservatively and record the reasoning in `evidence`:

- at least N transfers to the same known hot wallet (start N = 3),
- above a share threshold of the address's total outbound value,
- little or no outbound activity to anything else.

Emit with `source = 'derived:deposit'` and `confidence = 0.6`. Make N and the
thresholds config, not constants. Measure the precision of this heuristic
against a hand-labelled sample and record the result in `METHODOLOGY.md`.

### Resolution rules

When an address carries several labels: highest confidence wins; ties broken by
source priority order in config. Sanctions labels always win regardless of
confidence. Never silently merge conflicting categories — record the conflict.

**Acceptance (Phase 2):** label counts per source are reported; every OFAC
address resolves correctly; the derived-deposit heuristic is measured, with its
precision written down.

---

## 7. Traversal and scoring (Phase 3)

### Traversal

Breadth-first over `edges`, one direction at a time.

- Max hops from config (default 5).
- Stop expanding at any labelled address — a known exchange is a terminal node,
  not a waypoint. Without this rule everything reaches Binance in six hops and
  every score becomes noise.
- Guard against fan-out: cap neighbours per node (default 5,000, highest value
  first) and record when the cap was hit, so the result is honest about being
  truncated.
- Cycle detection on visited set per traversal.

### Haircut propagation

Value is split proportionally at each node.

```
contribution(path) = value_share(hop1) * value_share(hop2) * ... * decay^hops
```

Default `decay = 0.5` per hop, from config. Accumulate contributions by
category. Normalise to percentages of total traced value.

### Dust handling

Small inbound transfers from unknown sources are dusting, not exposure. Anyone
can send an unsolicited transfer to any address, so inbound dust must never
meaningfully move the score. Classify inbound transfers below a USD threshold
(config, default $1) into a `dust` category with near-zero weight, and exclude
them from traversal expansion entirely.

### Score

```
score = sum(category_pct * category_weight) / 100
```

Weights live in `config/weights.yaml`. Starting values:

```
sanctions: 100       terrorist_financing: 100
darknet: 90          stolen_funds: 85
mixer: 70            scam: 70
high_risk_exchange: 40   gambling: 25
unnamed_service: 15  dust: 5
exchange: 2          dex: 5
```

Bands: Low 0–30, Medium 30–60, High 60–100. Any direct sanctions hit is
reported as High regardless of computed score.

### Coverage — mandatory

```
coverage = attributed_value / total_traced_value
```

Report it in every response. Under 40%, the API must return the score marked
as low-confidence and the report must say so prominently. Do not hide unknown
exposure — showing it honestly is a deliberate design choice and a
differentiator, not a weakness to paper over.

**Acceptance (Phase 3):** scoring is deterministic across runs; every score
carries a stored path set that reconstructs it; changing a weight in YAML
changes output with no code change.

---

## 8. Interfaces (Phase 4)

### API

```
POST /v1/screen      { chain, address, direction? }  -> full result
GET  /v1/address/:chain/:address   -> cached result
GET  /v1/health
```

Responses include: score, band, inbound and outbound category breakdowns,
coverage, label snapshot ID, config version, traversal stats, and the top
contributing paths in plain language.

### Telegram bot

Address in, formatted breakdown out. Mirror the API exactly; the bot holds no
logic of its own.

### PDF report

One page: address, score, band, both breakdowns, coverage, the top paths,
methodology reference, timestamp, snapshot ID, and the triage-tool disclaimer.
Compliance teams buy the report, not the number — treat it as a first-class
deliverable rather than an afterthought.

---

## 9. Validation (Phase 5 — not optional)

Build `cmd/validate` as a real harness, not ad-hoc scripts.

1. **Sanctions recall:** every OFAC address must land in the High band. Any
   miss is a build-breaking bug.
2. **False-positive check:** a hand-picked set of ordinary exchange deposit
   addresses and long-lived personal wallets must land Low. Investigate every
   one that does not.
3. **Dust resistance:** synthesise dust transfers into a clean address and
   assert the score barely moves.
4. **Stability:** re-run the same address set against the same snapshot; assert
   identical output.
5. **External comparison:** take 100 addresses, obtain public scores from an
   existing service, and produce a divergence table with category-level
   breakdown. Do not tune weights to match them — record and explain
   divergence. This table is the strongest evidence of seriousness the project
   will have.

Golden fixtures in `/testdata` so traversal and scoring are testable without
network access.

---

## 10. `docs/METHODOLOGY.md`

Write this as you go, not at the end. It must cover: data sources and their
licences, label confidence model, the derived-deposit heuristic and its
measured precision, traversal rules and hop limits, the haircut formula, the
full weight table with justification, coverage definition, known limitations,
and the explicit statement that this is a triage tool built on open data rather
than a regulated AML determination.

---

## 11. How to work

- Build phase by phase. Stop at each acceptance gate and report.
- When a design choice is genuinely ambiguous, ask rather than assume. When it
  is not ambiguous, decide and note the decision in `docs/DECISIONS.md`.
- Write tests alongside each phase, not after.
- Prefer boring, inspectable code. Anyone reading a score must be able to trace
  it back to rows in a table.
- Flag honestly whenever open data cannot support something this spec asks for,
  instead of producing a number that looks authoritative but is not.
