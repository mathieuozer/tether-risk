# DECISIONS.md

SPEC.md §11: when a design choice is genuinely ambiguous, ask; when it is not,
decide and note the decision here.

Each entry records what was decided, what it departs from, and why. A decision
that turns out wrong should be superseded by a new entry rather than edited
away — the reasoning that led to it is part of the audit trail.

---

## D1 — `transfers` uses `ReplacingMergeTree`, not `MergeTree`

**Date:** 2026-09-20 · **Status:** active · **Departs from:** SPEC.md §4

SPEC.md §4 asks for `MergeTree` ordered by `(chain, from_address, block_time)`
and, separately, for deduplication on `(chain, tx_hash, log_index)`. A plain
`MergeTree` has no deduplication mechanism, so the two requirements cannot both
be met as written.

**Decision:** `ReplacingMergeTree` ordered by
`(chain, from_address, block_time, tx_hash, log_index)`. The spec's ordering
survives as a prefix, so scans by `(chain, from_address)` are unaffected, and
the natural key is appended to give the uniqueness the spec asks for.

Note that `ReplacingMergeTree` deduplicates only during background merges, so
reads need `FINAL` (or an explicit aggregate) to be exact. That is acceptable
because traversal never reads `transfers` — it reads `edges`.

---

## D2 — deduplication happens before insert, not in the table engine

**Date:** 2026-09-20 · **Status:** active · **Departs from:** SPEC.md §4 (by addition)

This is the most consequential decision in the data model.

SPEC.md §4 specifies `edges` as an `AggregatingMergeTree` materialized view
over `transfers`, and requires that all traversal read `edges`. It separately
requires that ingestion be idempotent and safely re-runnable (§5).

**The trap:** a ClickHouse materialized view fires on the rows of each
`INSERT`, not on the rows that survive deduplication. Re-inserting a transfer
that is already stored adds its value to `edges` a second time — permanently —
even though `ReplacingMergeTree` will later collapse the duplicate in
`transfers` itself. There is no error and no constraint violation. Edge values
simply drift upward every time a fetch is retried, and every score computed
from them is quietly wrong. Because traversal reads only `edges`, nothing
downstream can detect it.

**Decision:** three defences, all of them required.

1. **Deduplicate before insert.** `TransferWriter.WritePage` filters each batch
   against the natural keys already in ClickHouse, and against duplicates
   within the batch itself. Single-writer-per-address job leases make the
   read-then-write safe.
2. **Ingest ledger.** `ingest_batches` in PostgreSQL records what each page
   wrote. A replayed page is a no-op rather than a second insert. The batch is
   recorded only *after* the insert succeeds, so a crash mid-insert leaves the
   page unrecorded and re-fetchable rather than marked done with rows missing.
3. **Deterministic rebuild.** `RebuildEdges` recomputes both edge tables from
   `transfers FINAL`. This is ground truth.

`TestEdgesInflateOnRawReinsert` deliberately reproduces the corruption and
asserts the rebuild repairs it, so the reasoning above stays executable rather
than becoming a stale comment.

---

## D3 — `Adapter` gains an address-oriented method

**Date:** 2026-09-20 · **Status:** active · **Departs from:** SPEC.md §5

SPEC.md §5 defines the adapter as `Head` plus `FetchRange(from, to)`, which is
block-range oriented. The same section specifies that ingestion is
demand-driven per address, not full-chain. A block-range interface cannot
express "fetch this address's history", and TronGrid's address endpoints are
not block-range queryable at all.

**Decision:** add `FetchAddress(ctx, address, cursor) (AddressPage, error)`.
`FetchRange` is retained for optional full-chain backfill, so both access
patterns live behind one interface as the spec intends.

---

## D4 — `labels` is slowly-changing-dimension type 2

**Date:** 2026-09-20 · **Status:** active · **Departs from:** SPEC.md §4

SPEC.md §4 asks for `UNIQUE (chain, address, source)` and for a `snapshot_id`
on every label row; §2 requires that a score be reproducible against a named
snapshot. These conflict: one row per source cannot simultaneously be one row
per snapshot. Materialising a full copy of the table per daily snapshot would
mean roughly 365M rows a year for a 1M label set.

**Decision:** each row carries `valid_from_snapshot` and `valid_to_snapshot`.
Resolution at snapshot *S* selects rows where
`valid_from_snapshot <= S AND (valid_to_snapshot IS NULL OR S < valid_to_snapshot)`.

The spec's uniqueness is preserved as a partial unique index over currently-open
rows, which is what it was actually protecting. History is append-only and
compact.

---

## D5 — value arithmetic uses `shopspring/decimal`, not `float64`

**Date:** 2026-09-20 · **Status:** active

SPEC.md §2 requires identical input to produce an identical score, and every
score to be reconstructible from stored data. The haircut formula multiplies a
chain of fractional shares together and accumulates across many paths, so with
binary floating point the order of accumulation becomes observable in the
result, and a reviewer recomputing a score by hand from the stored path set
would not get the same number back.

**Decision:** decimal arithmetic for all value and share mathematics. Floats
appear only at the presentation boundary. The cost is speed, which SPEC.md §11
explicitly subordinates to inspectability.

---

## D6 — total ordering everywhere traversal makes a choice

**Date:** 2026-09-20 · **Status:** active

Determinism (SPEC.md §2) is not achieved by the scoring formula alone. Any
place the engine picks "some" neighbours rather than all of them — the fan-out
cap especially — turns iteration order into score differences.

**Decision:** neighbours are ordered by value descending, ties broken by
address ascending. Go map iteration order must never reach output. No
wall-clock value participates in scoring. `Config.Categories()` returns a
sorted slice for this reason.

---

## D7 — reverse-direction access uses separate tables, not projections

**Date:** 2026-09-20 · **Status:** active · **Departs from:** SPEC.md §4 (by refinement)

SPEC.md §4 asks for a second ordering by `(chain, to_address, block_time)` so
both traversal directions are fast. A ClickHouse projection is the obvious
implementation and was tried first.

ClickHouse rejects it outright: *"Projection is fully supported in
ReplacingMergeTree with deduplicate_merge_projection_mode = throw."*
Projections are not deduplicated when parts merge, so a projection over a
deduplicating or aggregating engine keeps counting rows the base table has
already collapsed — the same failure as D2, in a second place.

**Decision:** `transfers_by_to` and `edges_by_to` are separate tables, each fed
by its own materialized view from `transfers` and protected by the same
pre-insert deduplication. This is also what SPEC.md §4 literally asks for
("add a materialized view"). The cost is storage; the benefit is that there is
exactly one deduplication story in the system rather than two.

---

## D8 — reads go through `*_current` views, never the raw aggregate tables

**Date:** 2026-09-20 · **Status:** active

`AggregatingMergeTree` merges parts in the background, so an unaggregated read
can return several partial rows for a single edge. Traversal that summed only
one of them would understate an edge's value and therefore understate risk —
failing in the direction that matters most.

**Decision:** `edges_current` and `edges_by_to_current` apply the aggregation
explicitly. All traversal reads these.

---

## D9 — local PostgreSQL binds port 5433

**Date:** 2026-09-20 · **Status:** active

The development machine already runs other PostgreSQL containers on 5432.
Binding it left our container stuck in `Created` while the client connected to
a *different* project's database and failed authentication — a far more
confusing failure than a port clash.

**Decision:** `POSTGRES_PORT` defaults to 5433 in both `docker-compose.yml` and
the client. Fully overridable by environment.

---

## D10 — the banned-terminology gate excludes SPEC.md and itself

**Date:** 2026-09-20 · **Status:** active

SPEC.md §2 bans a particular word for trial deployments. `make
check-terminology` enforces it across the repository, but the rule creates an
obvious problem: SPEC.md must quote the word to state the rule, and the gate
must contain it to search for it.

**Decision:** the gate assembles the search term from fragments so it does not
match itself, and excludes `SPEC.md` as the authority that defines the rule.
Everything else in the repository is checked.

---

## Open — coverage semantics for truncated traversals

**Date raised:** 2026-09-20 · **Status:** open, to be settled at the Phase 3 gate

When traversal stops because it hit the hop limit or the fan-out cap, the value
beyond that point is neither attributed to a category nor provably
unattributable. Counting it as unattributed lowers coverage; excluding it from
the denominator raises coverage by hiding the truncation.

**Current position:** count it as unattributed, on the principle that SPEC.md
§7 would rather understate confidence than overstate it, and that hiding
unknown exposure is explicitly called out as the thing not to do. This
materially affects reported coverage and is worth confirming against real
numbers before it is locked in.

---

## D11 — TRC-20 transfers get a synthetic log index

**Date:** 2026-09-20 · **Status:** active · **Departs from:** SPEC.md §4 (forced by the data)

SPEC.md §4 deduplicates transfers on `(chain, tx_hash, log_index)`. TronGrid's
TRC-20 endpoint — the source of USDT flow, which §3 names the priority asset —
returns no event or log index at all. The fields available are
`transaction_id`, `from`, `to`, `value`, `token_info` and `block_timestamp`.
This was confirmed against the live API, not inferred from documentation.

Using the item's position within the response page is not viable: pagination
can split one transaction's transfers across two pages, so the same transfer
would be assigned different indices on different fetches and inserted twice —
inflating `edges` exactly as D2 describes.

**Decision:** derive the index by hashing the transfer's identifying fields
(`from`, `to`, token contract, `value`) with FNV-1a. The result is stable for
the same transfer seen from any page in any order, which is the property
deduplication needs.

**Known limitation:** two transfers within a single transaction that share
sender, recipient, token *and* value hash identically and collapse into one.
This undercounts, which is the safer direction — it understates flow rather
than inventing it — but it is a real loss and is recorded in
docs/METHODOLOGY.md rather than left implicit.

The native TRX endpoint needs none of this: the index within
`raw_data.contract` is a genuine log index and is used directly.

---

## D12 — TRC-20 transfers carry no block number

**Date:** 2026-09-20 · **Status:** active · **Departs from:** SPEC.md §4

The same endpoint returns no `blockNumber`. The native endpoint does.

**Decision:** store `block_number = 0` for TRC-20 transfers, meaning unknown.
Nothing load-bearing depends on it: `transfers` is ordered by `block_time`,
which the endpoint does provide, and traversal reads `edges`, which carries no
block number at all. Zero is used rather than null because the column is not
nullable and the distinction has no consumer.

If a future requirement needs exact block numbers for TRC-20 flow, it will
need a block-timestamp index or a different data source, and that is a larger
change than back-filling a column.

---

## D13 — the controlled vocabulary stays at SPEC.md §7's twelve categories

**Date:** 2026-09-20 · **Status:** active

A competitor's output for a sampled Tron address reports roughly 23
categories against our 12, including Token contract, Enforcement action,
Custodial wallet, Bridge, Payment Service Provider, Lending, Smart contract,
P2P exchange, High-Risk Jurisdiction, ATM and Mining Pool.

**Decision:** keep the twelve categories SPEC.md §7 specifies, and map external
vocabularies onto them in `config/comparison_mapping.yaml` for the §9.5
divergence table only.

Reasoning:

- Every weight in the published table needs a justification in
  METHODOLOGY.md. Adding eleven categories means eleven more numbers to defend,
  for distinctions our open-data label sources mostly cannot draw. A category
  we cannot reliably populate is worse than one we do not have: it produces a
  row that is always near zero and implies a precision we do not possess.
- SPEC.md §9.5 says explicitly not to tune toward external services. Adopting a
  competitor's taxonomy is a soft form of exactly that.
- The mapping file records where it is lossy, so the divergence table can
  report "we cannot express this" honestly instead of silently folding it into
  a neighbouring category.

The mapping deliberately does not contribute to `config_version`: it affects no
score, and including it would make scores appear to change whenever a
comparison mapping was corrected.

Revisit if a label source is adopted that can actually distinguish these
categories at usable confidence.

---

## D14 — the Dune Spellbook cannot supply ingestible labels

**Date:** 2026-09-21 · **Status:** active · **Departs from:** SPEC.md §6.3

SPEC.md §6.3 says to clone the open spellbook repository and ingest the labels
schema at confidence 0.8. That was the planned route to exchange labels, which
are the single biggest coverage gap.

It does not work, and the reason is structural rather than a matter of effort.

Checked against the repository on 2026-09-21:

- The label models are **dbt SQL**, not data. `labels_cex_ethereum.sql` reads
  `FROM {{ source('cex','addresses') }}` — a table inside Dune's own warehouse.
  The file contains no addresses at all, and neither do its siblings.
- There is **no TRON CEX label model**. The per-chain files cover arbitrum,
  avalanche_c, bitcoin, bnb, ethereum, fantom, optimism and polygon.
- Of 14,947 paths in the repository, 65 mention TRON and every one is a fees or
  transfers metric, not a label.

So cloning the repository yields queries we cannot run, against data we do not
have, for chains that in TRON's case are not covered anyway. The Apache-2.0
licence permits redistribution of the SQL; there is simply nothing behind it.

**Decision:** no Dune ingester will be written. `sources.yaml` records the
source as unavailable with this reason rather than leaving it listed as
"permitted but not yet implemented", which implied work outstanding rather than
a dead end.

Running the queries would require a Dune API subscription, which SPEC.md §1
rules out ("no commercial data feeds. Zero licence spend").

**What this leaves for exchange labels**, which remain the coverage bottleneck:

1. Exchanges' own published proof-of-reserves address lists. Self-published,
   freely redistributable, and authoritative about the exchange's own wallets.
   The most promising automatable route.
2. Human verification against a block explorer. Reading a page is not
   scraping it, so this does not conflict with D-F2, but it does not scale.
3. The derived-deposit heuristic — but that anchors on known hot wallets, so it
   amplifies an existing seed rather than creating one.

None of these is a drop-in replacement, and the coverage figure will keep
reporting the gap honestly until one is done.

---

## D15 — behavioural service detection labels `unnamed_service`, never `exchange`

**Date:** 2026-09-21 · **Status:** active

Exchange labels are the coverage bottleneck, and after D14 removed the Dune
route there is no citable source for named TRON exchange hot wallets: block
explorers are blocked by their terms (F2), and exchanges' proof-of-reserves
pages gate their address lists behind JavaScript. Writing addresses from
recall into a file carrying 0.95 confidence that terminates traversal is
exactly the failure this system exists to avoid.

What remains is behaviour. An address transacting with hundreds of distinct
counterparties while still paginating after a deep sample is a service.
Nothing else produces that shape.

**Decision:** detect services behaviourally and label them `unnamed_service`,
never `exchange`.

The distinction is the whole point. SPEC.md §7 weights `exchange` at 2 because
reaching a regulated exchange is close to reassuring, and `unnamed_service` at
15 because an unidentified service might be a no-KYC swapper. Behaviour proves
*that* an address is a service; it says nothing about *which*. Claiming
otherwise would understate every user's exposure sevenfold.

The consequence runs the other way too: a real exchange detected this way is
labelled at weight 15 and its users look slightly riskier than they are. That
is the conservative direction, and a verified name from `curated` outranks
this source and should replace it whenever one is available.

**Thresholds**, calibrated against measured samples rather than guessed:

| Address | Sampled | Counterparties | More pages | Verdict |
|---|---|---|---|---|
| TZ8Ksz21… | 600 | 514 | yes | service |
| TAythDdK… | 78 | 52 | no | not a service |
| TNwf8VB… | 39 | 31 | no | not a service |
| TR5e7yK… | 237 | 20 | no | not a service |

The distinct-per-transfer ratio alone does not work: the 39-transfer wallet
scores 0.795, higher than some genuine services, because small samples are
trivially diverse. Volume and breadth together are what separate.

Every label records the measurement and a URL that reproduces it, so a
reviewer can re-run the judgement rather than trust it.

---

## D16 — unpriced data is reported, not rendered as silence

**Date:** 2026-09-21 · **Status:** active

An edge whose transfers carry no USD price contributes nothing to the
proportional split, so an address with real history but no prices loaded
traced to nothing and reported "no traced value" — which reads as "this
address never moved funds".

Found in practice: an address with 96 outbound edges and 494 transfers
reported as though it were inactive, because the worker had ingested it after
the last pricing backfill.

**Decision:** traversal counts unpriced transfers and the result distinguishes
a pricing gap from an inactive address, naming the transfer count and the
command that fixes it. This is the same class of failure as the dead-end bug
in D2's neighbourhood: an empty result that looks like a clean answer.

---

## D17 — TRON ingestion runs one serial worker, not a pool

**Date:** 2026-09-21 · **Status:** active · **Departs from:** the obvious default

Ingestion originally ran four workers sharing a rate limiter configured at
12 requests per second with a burst of 4. It abandoned addresses.

Measured on 2026-09-21, with nothing else competing for the API:

| Target rate | Requests OK | Rate limited | Achieved |
|---|---|---|---|
| 3/s serial | 10 of 10 | 0 | 0.9/s |
| 5/s serial | 4 of 10 | 6 | 1.1/s |
| 8/s serial | 9 of 10 | 1 | 1.1/s |

The achieved rate is about 1 request per second regardless of the target,
because each request costs roughly a second of round-trip latency. **The
configured rate was never buying throughput.** Raising it only produced 429s,
and each 429 triggered backoff of up to 30 seconds.

Comparing the two configurations over the same queue:

| | 4 workers, burst 4 | 1 worker, burst 1 |
|---|---|---|
| Addresses completed per minute | ~3 | **6** |
| Rate-limit errors | continuous | **0** |
| Addresses abandoned | 3 | **0** |

**Decision:** one worker, burst 1, 3 requests per second. Serial ingestion is
twice as fast as the pool and abandons nothing.

The counter-intuitive part is worth stating plainly, because the instinct to
add workers is strong: concurrency here was not merely useless, it was the
cause of the slowness. The pool spent most of its time in backoff it had
triggered itself, and the three addresses it abandoned were dropped for
reasons that had nothing to do with those addresses.

Revisit only with a `TRONGRID_API_KEY`, which raises the ceiling. Until then
the bottleneck is network latency, and no amount of parallelism moves it.

---

## D18 — a TRC-20 asset is identified by its contract, never its symbol

**Date:** 2026-09-21 · **Status:** active · **Supersedes:** symbol-based naming in the TRON adapter

The TRON adapter named each TRC-20 asset by `token_info.symbol`. The deployer
of a contract picks that symbol, and anyone can deploy a contract. The pricer
then applied decimals and the $1 peg by name.

Found in practice: `THk5qH79SoAaUnUh8JVdRarSESTZpqPjSQ` calls itself "USDT"
("Tether USD") and declares 18 decimals. Real Tether is
`TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t`, with 6. Reading 18-decimal amounts at 6
inflates them by 10^12, so one transfer of 10,350 counterfeit tokens was stored
at $10.35 quadrillion. Across the 64,674 stored "USDT" rows, six transfers
account for $49.9 quadrillion of the $49.9 quadrillion total. The rest sum to
$673 million.

This is worse than noise, because it runs in the direction that matters. A
worthless token sent to an address inflates the denominator of every
proportional exposure figure. Real mixer or sanctions exposure then becomes a
rounding error. That is an evasion technique, not only a data quality problem.

A fake `TRX` has the same flaw: it would have been priced at the native daily
close.

**Decision:** TRC-20 assets are named from a fixed table of contract addresses
(`canonicalTokens` in `internal/chain/tron/adapter.go`), each checked against
Tronscan. Any other contract is named by its own address, which the pricer
does not recognise, so it stays unpriced. The EVM adapter already worked this
way. A transfer with no parseable contract is rejected.

Filtering by value would not be enough. A counterfeit that declares 6 decimals
produces plausible amounts, and only the contract separates it from real
Tether.

**Stored data:** `transfers` has no contract column, so existing rows cannot be
reclassified in place. Re-ingesting does not fix them either: the writer skips
transfers already stored, and pages already in `ingest_batches`. Repairing
the data means deleting the TRON TRC-20 rows and their ledger entries,
re-ingesting, repricing and rebuilding the edges.

Repaired on 2026-09-21 in exactly that way. All 64,776 previously stored token
rows came back under their correct assets, plus 9,046 transfers the earlier
runs never reached. Five counterfeit "USDT" contracts accounted for 10 rows:
`THk5qH79SoAaUnUh8JVdRarSESTZpqPjSQ` (5 rows) and
`TTmQYPPZ3N3AfSFx4NKm4or2zwDn8dvKRE` (1 row) carried all $49.9 quadrillion.
`TCGST91DVQ4XGM5kEbupUWAE8CBJDJq9Fs`, `TTPnLa9d3hnUdjYNRsAqyw76TTDuodq5xw` and
`TLMRyoRTCetaz8HK1gLqq4N1feGHoqLriQ` carried $10 to $4,117, amounts that no
value filter would have caught. After repricing, stored TRON USDT totals $753
million across 73,710 transfers. The largest single transfer is $8.5 million.
