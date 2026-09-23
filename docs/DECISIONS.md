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

---

## D19 — exchange addresses come from exchanges' own lists and our own test transfers

**Date:** 2026-09-21 · **Status:** active · **Follows:** D14, D15

Exchange labels are the largest coverage gap. On a sampled address, a
commercial screener attributed 61% of flow to exchanges, and we attributed
none, because we held no exchange labels. D14 left exchanges' own
proof-of-reserves lists as the most promising open route. They were tried on
2026-09-21:

| Exchange | Where | TRON addresses | Terms |
|---|---|---|---|
| Binance | PoR endpoint on binance.com | 25 | **prohibited** (below) |
| HTX | exchange's own GitHub, signed per address | 15 | MIT |
| Poloniex | exchange's own GitHub, signed per address | 7 | MIT |
| OKX | tool on GitHub; address file only on okx.com | – | not yet read |
| Gate, KuCoin, Bitget | web endpoints | – | refused, or totals only |

**Binance is blocked.** Its Terms of Use prohibit "web crawlers, bots, spiders
or other automatic devices, programs, scripts... or any similar or equivalent
manual processes to access, obtain, copy or monitor any part of the
Platform". Under SPEC.md §6.2 we stop rather than work around this, and the
"equivalent manual processes" wording excludes copying by hand too. The list
downloaded while checking was deleted and nothing from it was stored.

That list did validate D15's detector. Every Binance wallet present in stored
data (10 of 25) was already among our 25 behaviourally detected unnamed
services, so the detector finds real exchange hot wallets unassisted. It
cannot name them.

**HTX and Poloniex are ingested** (`htx_por`, `poloniex_por`) at confidence
0.9 and category `unnamed_service`, with the exchange named as entity. A
reserve list proves who controls an address. It says nothing about KYC
standards, and KYC standards are what separate `exchange` (weight 2) from
`high_risk_exchange` (weight 40). This is D15's rule again: identify, but do
not assert a risk tier without a basis. None of the 22 addresses appears in
stored data yet, so today this changes no score.

**The route that scales is a controlled test transfer.** Withdraw from
exchange A to your own deposit address at exchange B. The withdrawal's sender
is A's hot wallet. B then sweeps the deposit, usually after topping it up with
TRX, into its collection wallet. Each address is proven by a transaction the
maintainer made, with no third-party list involved. `labeler trace-tx <txid>`
follows a transfer and prints evidence links and a `curated_labels.yaml`
snippet for review. It writes nothing itself, because a curated label ends
traversal at 0.95 and will be believed. These entries are `exchange`, because
testing an exchange means holding an account there, and holding one means
going through its KYC.

Privacy: the repository is public. Evidence links reveal the maintainer's
deposit addresses, so notes stay generic, and test deposit addresses should
not be reused for real funds.

---

## D20 — two pipeline stages were silently doing nothing

**Date:** 2026-09-21 · **Status:** fixed

Both were found while wiring the daily schedule. Both reported success.

**The deposit-wallet heuristic could never fire.** `labeler derive` passed an
empty label map to `DeriveDeposits`, with a comment deferring the lookup to a
caller that did not exist. So however many hot wallets were curated, the
heuristic would report "no exchange labels". PLAN.md Phase 2 had it ticked.
It now loads the snapshot's `exchange` and `high_risk_exchange` labels.
Labels the heuristic produced itself are excluded as anchors. Otherwise each
run would treat the previous run's deposit wallets as hot wallets and label
their senders, pushing the error one hop further every day. When nothing
anchors, the run says so and exits cleanly, so the nightly schedule does not
fail every night until the first hot wallet exists.

**The OFAC refresh parsed nothing.** `sources.yaml` pointed at
`SDN_ENHANCED.XML`, whose schema the parser does not read. Every run parsed
zero entries and upserted zero labels without an error. The 466 stored OFAC
labels came from an earlier run against the classic `SDN.XML`, so no
designation added since would ever have arrived. Measured on the live files,
the classic list gives 19,393 entries and 1,059 digital currency addresses,
of which 466 are on supported chains. The source now points at the classic
file, and a parse with no entries or no digital currency addresses fails the
run. SPEC.md §9.1 treats a sanctions miss as build-breaking, and a silent
empty success is the same miss arrived at quietly.

The common failure: an empty result reported as success. It is the pattern D2,
D16 and D18 each found in a different stage, and the reason the daily run
fails loudly and posts a desktop notification rather than only writing a log.

**Found, not fixed:** the service sampler paces itself at 120 ms between
requests, about 8 per second. D17 measured TronGrid's usable ceiling at about
3 per second without a key, which is the likely reason `derive-services`
takes around 30 minutes and hits rate limits.

---

## D21 — no `token_contract` category: measured at one transfer

**Date:** 2026-09-21 · **Status:** active · **Reaffirms:** D13

The commercial screener's breakdown includes "Token contract" at 0.7%. Before
revisiting D13 for it, the stored data was measured. The 58 token contracts
we know (every contract seen as a transfer's asset, plus the canonical
stablecoins) are a counterparty in **1 of 279,497 TRON transfers**, worth $6.

That is structural. A TRC-20 transfer moves value from A to B, and the
contract only names the token. A contract becomes a counterparty only when
someone sends tokens to the contract address itself, almost always by
mistake. The competitor's figure more likely counts spam and airdrop token
transfers as "connections". We deliberately leave those unpriced since D18,
and pricing them would reopen that hole.

**Decision:** no new category. A category that is always about zero is what
D13 warns against. Smart contracts in general (DEX routers, bridges, lending
pools) are real counterparties and may be worth a category. That is to be
measured first, against the largest counterparties.

---

## D22 — screening fetches before it scores, and ingestion prices as it writes

**Date:** 2026-09-21 · **Status:** active

The use this serves is one step: give an address, get the answer. Three gaps
stood in the way. None of them raised an error.

**The API never fetched.** `/v1/screen` scored whatever was already stored.
An address nobody had ingested came back as "no traced value", which reads
as "never moved funds". Every screen that looked right had been preceded by
a manual `ingest fetch`. Screening now fetches the address first (depth 1),
which queues its counterparties for the worker. The TTL cache makes this free
for anything fetched in the last day.

The fetch has a 45-second budget. At the public rate limit a large address
takes minutes, longer than the API's two-minute write timeout or a chat
client will wait. When the budget runs out, the pages already written are
kept, the cursor is saved, the address is handed to the worker, and the
result says the history is still being fetched. Measured on a never-seen
address: 39 seconds, partial result, job queued.

Each result reports its depth: how many of the queued counterparties are
traced, whether the address's own history hit the per-address page limit,
and whether it is still being fetched. A shallow answer never reads as
final.

**Fetched history had no price.** Workers wrote transfers as `unpriced`, and
only `price backfill` valued them. Traversal skips unpriced edges, so an
address fetched during the day scored as if it had no history until the
nightly run. The first end-to-end screen of a new address showed "$0
received in 3,222 transfers", with 100% coverage resting on a single older
edge. Workers now price each transfer before writing it, and edges are
correct on insert. The one-off backfill then valued 24,943 transfers left
unpriced that day.

**The worker lived in a terminal.** Queued counterparties are only traced
while a worker runs. It now runs under launchd with KeepAlive
(`make worker-install`), and restarts on its own after a crash or once
Docker is back.

**Sampler pacing: fixed, but it was not the bottleneck.** The service
sampler paced itself at about 8 requests per second against a per-IP budget
it shares with the worker and with screening. It now uses half the configured
rate and counts its requests, retries and 429s, which were silent before.
Measured interleaved on the same candidates on 2026-09-21, with the worker
running and then stopped, both paces cost about 8 seconds per address. In
every run 60–80% of requests were throttled. Without an API key, TronGrid was
throttling this IP harder than D17 measured that morning. The pacing change
spends 25% fewer requests for the same result. The remedy for the slowness
is `TRONGRID_API_KEY` in `.env`, which the worker and the daily run now load.

---

## D23 — deepen where the trail stops, not everywhere

**Date:** 2026-09-21 · **Status:** active · **Revisits:** D17 for keyed use

With the address and all 100 of its queued counterparties fetched,
`TAythDdKTZeNq6VnQ7o9cEvWRQGgRpPiKX` still had 84.8% of traced value
unattributed. All of it stopped at dead ends: addresses two or more hops out
with no stored history. Going blindly to depth 2 would be about 10,000
fetches. The traversal already knows exactly where value stopped: 122
addresses on the first screen.

**Decision:** after scoring, a screen queues the unfetched dead-end addresses
carrying the most unattributed value, up to 100 per screen. Each rescreen
reaches one ring further, bounded by `max_hops`, so the cost follows how far
the trail actually runs rather than how wide it fans out. Dead ends that have
already been fetched are genuine ends and are not queued again. The result
reports how many are queued, and the unattributed reason now reads "trail
stops", because "not ingested" is false for a fetched dead end.

Measured on that address, rescreening after each ring drained:

| Ring | Coverage | Score | Queue drained in |
|---|---|---|---|
| start | 15.2% | 5.0 | – |
| 1 | 37.0% | 10.1 | 184 s |
| 2 | 59.5% | 11.7 | 226 s |
| 3 | 79.9% | 12.5 | 173 s |
| 4 | **94.6%** | 13.8 | 121 s |

The remaining 5.4% is beyond the hop limit, which is configured, not unknown.
The commercial screener put 75.8% of this address in "Exchange" plus "Unnamed
service". We put 84.1% in unnamed service. The flows now match, and the
difference is naming, which is D19's problem.

**Keyed ingestion (revisits D17).** D17 kept one worker because, without a
key, a pool only multiplied 429s. With `TRONGRID_API_KEY` the chain uses
`rate_limit_per_sec_with_key` (8, under the free plan's 15 QPS, because the
worker pool, screening and the sampler share the key), and `worker.sh` runs
three workers. Each request is about a second of latency, so one stream would
use an eighth of the budget. The four rings above drained 100 addresses in
2–4 minutes each, work that took hours without the key.

## D24 — a failed fetch job waits before it is retried

**Date:** 2026-09-21 · **Status:** active

Job 238 (`TB37WWozkkenGVYWD7Do2N5WT2CedqDktJ`) was abandoned after five
attempts, all rejected by TronGrid with 429. Each one failed on a different
page (3, 25, 3, 5, 2), so the address itself was fine. The attempts ran from
19:27 to 19:31, right after a single keyless worker had drained three
addresses to the 50-page limit back to back. `Fail` returned the job to
`pending` straight away, a worker reclaimed it within a second, and every
attempt landed in the same rate-limit window. An abandoned job leaves no
freshness entry, so screenings reported the address as a coverage gap until
someone re-queued it by hand.

**Decision:** `Fail` sets `not_before`, and `Claim` skips a job until that
time has passed. The wait is one minute after the first failure and doubles
each time after that, capped at 15 minutes: 1, 2, 4 and 8 minutes across the
default five attempts. A job now has to fail for about a quarter of an hour
before it is abandoned, which a rate-limit window does not last. `Release`
clears `not_before`, because a worker shutting down is not a failure.
Nothing waits on a job finishing: screening scores what is stored and says
what is still being fetched, so the backoff only delays the retry.

## D25 — reserve lists anchor the deposit heuristic; unnamed services are described, not hidden

**Date:** 2026-09-22 · **Status:** active · **Follows:** D19, D24

A commercial screener's report on the same kind of address reads "Exchange
61%". Ours read "Unidentified high-volume service" for nearly all of it. We
hold no `exchange` labels (D19), so the gap is naming, not tracing.

**The weight of `unnamed_service` stays at 15.** Lowering it was considered
and rejected. Unnamed flow alone can score at most 15, inside the Low band,
so the weight never flags a clean address. What it does is keep an unnamed
service from reading as a regulated exchange. Many service-shaped wallets
are no-KYC exchanges, OTC desks or payment processors, and at weight 2 an
address that is 60% unnamed and 20% gambling would score 6, not 14. A score
comes down honestly when a service is named, not when unknown is re-rated.

**Reserve lists anchor the deposit heuristic.** The heuristic anchored only
on `exchange` labels, so it had never fired. The HTX and Poloniex reserve
addresses (D19) name the exchange, so `derived_deposit.anchor_sources` now
lets them anchor it. Labels keep the anchor's category, `unnamed_service`:
the list names the exchange without saying anything about its KYC tier.
They are named "Poloniex (sends to its reserves)" rather than "deposit
wallet", because some senders move hundreds of millions in a few transfers,
which is the exchange's own wallet rather than a customer's.

Two faults surfaced on the first run:

- **Unfetched candidates would have passed.** An address never fetched shows
  only its transfers to the hot wallet, so its share was 100% by
  construction. Candidates are now judged only once fetched; the rest are
  queued, 200 per run, and judged on the next.
- **A TRC-20 item with no token contract failed the whole address.**
  TronGrid returns an empty `token_info` for tokens it cannot resolve (seen
  on 2021 transfers of value 1). Every retry failed the same way, and D24's
  backoff only spread five doomed attempts over 16 minutes. The item is now
  skipped. It is never stored under a guessed asset, so D18 still holds.

Result: 9,439 addresses sent to the 22 reserve wallets. Of the 400 fetched
and judged, 208 were accepted: 197 Poloniex, 11 HTX. Precision is
unmeasured; see METHODOLOGY.md §4.

**Unnamed services are described.** The connections summary now shows each
unnamed service's stored activity, for example "$43.98M moved with 6,402
addresses across 10,000 stored transfers", so a reader can judge its size and
shape without a name. The "Unidentified" prefix is dropped from the list,
because the category line already says the operator is unnamed. Every
category is listed, including those at 0%, so a missing line cannot be read
as "not checked".

## D26 — the bot is sold by monthly subscription

**Date:** 2026-09-22 · **Status:** active · **Supersedes:** SPEC.md §1 non-goal "No user accounts / billing in v1"

The owner decided to sell the Telegram bot by monthly subscription. SPEC.md
listed accounts and billing as a v1 non-goal; this reverses that for the bot
only. The API stays unauthenticated and now listens on 127.0.0.1 by default,
because anyone who could reach it would bypass every plan.

**What is sold.** Two tiers in `config/billing.yaml`: Basic (20 screens a
day) and Pro (200 a day, `/details`, `/pdf`). New users get a 7-day Basic
trial, once per Telegram account. Prices there are placeholders to set
before selling. The file is not part of `config_version`, because a price
change must not look like a scoring change.

**How customers pay.**

- *Telegram Stars.* A Stars subscription via `createInvoiceLink` with
  `subscription_period` 2592000, the only value the Bot API accepts, and a
  price of at most 10,000 Stars. Telegram's rules require Stars for digital
  services sold in bots. Telegram renews it itself, and each renewal arrives
  as a `successful_payment`. Pre-checkout approves only a current plan at its
  current price, so a stale link cannot buy at an old price. `/cancel` stops
  renewal through `editUserStarSubscription`; a refund, whether issued by
  Telegram or via `/refund`, ends the period it paid for.
- *USDT on TRON.* No renewal: each payment buys `period_days`. An invoice is
  identified by its exact amount, the price plus 1–99 cents that no open or
  recently expired invoice uses. Exchanges withdraw to two decimals, so
  cents are the finest unit a customer can reliably send. The watcher reads
  confirmed transfers of the canonical USDT contract only, so a counterfeit
  "USDT" can never pay (D18). A payment matching no invoice is kept and the
  admins are told.

**What cannot go wrong twice.** Stars charge ids and USDT transaction
hashes are unique in the database, so a redelivered update or a rescan
never grants a second period. The daily limit is checked and incremented in
one statement, so concurrent screens cannot exceed it. A screen that fails
is refunded to the day's allowance. A payment that cannot be recorded tells
the customer and every admin rather than failing silently. Paying before
the current period ends continues from its end; no days are lost.

**Operational changes.**

- Updates are handled concurrently, with at most 4 screens at once and one
  per user. Before, one slow screen held up every user, and Telegram allows
  only 10 seconds to answer a pre-checkout query.
- The bot ignores group chats: one person's allowance would be shared with
  everyone in the group.
- `/terms`, `/support` and `/paysupport` exist because Telegram requires
  them of bots that take payments. `config/terms.txt` is a template to be
  reviewed by a lawyer before selling.

**Not solved here.** The stack runs on one Mac, which paying customers will
outgrow first. TronGrid's and Binance's terms for commercial use were not
read. Selling AML screening can be regulated in some jurisdictions; the
disclaimer helps but is not legal advice.

## D27 — the product: Mini App, watches, batch, API keys, Turkish

**Date:** 2026-09-22 · **Status:** active · **Follows:** D26 · **Supersedes:** SPEC.md §1 non-goal "No continuous monitoring or alerting in v1"

The owner asked for the product side to be taken seriously and built out in
full. D26 made the bot sellable. This makes it something customers keep
paying for.

**One gate for every channel.** The chat, the Mini App, the public API and
batch files all screen through one function, `gate` in `cmd/bot/gate.go`. It
checks the address and chain, the plan, the feature and the daily limit,
runs the screen, refunds it if it fails, and records it in
`screen_history`. A channel that bypassed it would bypass billing. The
screening itself is still the internal API's, unchanged; SPEC.md §8's "the
bot holds no logic of its own" holds for everything that decides risk.

**Mini App.** It is served by the bot process at `/app/` from files embedded
in the binary, and authenticated with Telegram's signed `initData`: an
HMAC-SHA256 keyed from the bot token, rejected after 24 hours. It covers
screening with charts, history, watches, the account with both payment
methods, batch, API keys and language (docs/APP_API.md). It lives in the bot
process because the bot already holds the token, the database and the
payment state. A separate service would have to duplicate or proxy all
three. It needs a public HTTPS URL (`APP_URL`); `APP_ADDR` stays on
localhost behind a tunnel.

**Watches (reverses a SPEC.md non-goal).** Customers watch addresses: Basic
3, Pro 25, Business 200. Every 6 hours the monitor rescreens each one
through the internal API and alerts in Telegram when risk worsens: a higher
band, a new high-risk category, or a direct listing. The first check sets a
baseline and never alerts. Improvements are not alerts. Rescreens count
against the watch allowance, not daily screens. Every rescreen spends
TronGrid budget, so the interval and `per_pass` are config. At 200 watches a
pass that is a few hundred requests every 6 hours.

**Batch.** Pro and Business upload a .txt or .csv file, or paste a list in
the app. Each address uses one daily screen. The bot checks the plan's batch
size and today's remaining screens up front, rather than failing halfway
through. Results come back as a CSV in the chat.

**Business plan and API keys.** A third plan (2,000 screens a day, API
access) at 199 USDT, or the Stars ceiling of 10,000 Stars. Keys are 192
random bits, shown once, and stored as SHA-256. A slow password hash would
add latency to every request and nothing else, because the key is not
guessable. Each key is limited to 5 requests a second. The public API is the
same gate, so a key cannot exceed its owner's plan.

**Turkish.** The bot, the connections report, the PDF caption, the Mini App
and the terms exist in English and Turkish. A user's language is their
`/language` choice, else their Telegram client's. It is stored, so alerts
and payment messages sent unprompted use it too. The report's category
names and wording are the reference, and the app uses the same. Tests keep
every catalogue complete, with matching format verbs.

**Chains.** Addresses are recognised by shape: T… for TRON, 0x… for EVM,
with an optional `bsc`/`eth` prefix. An address on a chain not enabled yet
is refused by name ("Ethereum is not available yet"), not treated as a typo.
Ethereum and BSC need only an Alchemy key (docs/ALCHEMY.md).

**Verified.** Tests use a fake Telegram, a fake TronGrid, a fake screening
API and a real Postgres. They cover initData forgery and expiry, the app and
API endpoints, watch limits and alerts, batch parsing and delivery, key
revocation and rate limits, chain parsing, and Turkish answers. Breaking the
signature check, the alert comparison and the rate limit each made a test
fail. The real Mini App was driven in headless Chrome against the real
backend and a real screen, in both themes.

**Not solved here.** A public HTTPS URL for the app (a Cloudflare tunnel on
this Mac to start). Hosting off this Mac. Legal review of both terms files.
Behavioural flags such as "in = out within days" are not scored yet.

## D28 — more precise results: follow-up, exchange hot wallets, behaviour notes

**Date:** 2026-09-22 · **Status:** active · **Follows:** D23, D25, D27

The owner asked what would make results more precise and to do it. Three
changes need nothing from outside. Naming the large exchanges needs test
transfers (D19), and measuring accuracy needs the competitor's results for a
set of addresses. Both wait on the owner.

**Follow-up.** A first screen of a fresh address can cover almost nothing
while its counterparties are still being fetched. D23 deepened ring by ring,
but only when someone screened again. The bot now follows up by itself. It
waits for the worker's queue, rescreens, and repeats until nothing is left
to trace, coverage stops growing (under 0.5 points twice), or the round and
time limits are reached. Then it sends the final result, with what changed
since the first answer. Rounds use the internal API and cost no daily
screens. At most 10 run at once, and one per user and address. Measured
live: TKKPgK…dk4V went from 0.6% coverage (score 0.1) to 82.5% (score 10.4)
in 6 rounds and 20 minutes, with no action from the customer. The first
answer now says the final result will follow instead of "screen again later".

That test exposed an ordering problem: the labeler's deposit candidates and
a customer's rings shared one priority, so a customer could wait behind
background work. Background work is now queued at priority 1000; customer
screens stay at 99–100. A customer asking for an address already waiting in
the background lifts it to customer priority.

**Exchange hot wallets from reserve flows.** A wallet that exchanges
transfers both ways with one exchange's own reserve wallets (at least 3
each way, at least $100k in), is service-shaped (at least 250
counterparties), and does at least 90% of its reserve traffic with that one
exchange, is that exchange's hot wallet. Each rule excludes a real case in
the data. One-way inflow is a withdrawal or a desk. A few counterparties is
cold storage. Traffic with two exchanges' reserves is a market maker. Of
12,282 wallets with any reserve flow, 5 passed, all HTX:
TFTWNg…5jLu, TKFREL…3Mnf, TREnSa…vDtJ, TXJgMd…qccvd, TYyriW…RTd. They
are labelled `derived:hotwallet` at confidence 0.8, above
`derived:service`'s 0.75, so a named hot wallet replaces "high-volume
service". They also anchor the deposit heuristic, since customer deposit
wallets sweep into hot wallets. With them, accepted deposit wallets rose
from 208 to 368. Category stays `unnamed_service` (D19): the name is
proven, the KYC tier is not.

None of the sources of the two addresses the owner screened today are HTX
or Poloniex wallets. Those services are most likely larger exchanges, which
only test transfers can name.

**Behaviour notes.** Patterns in an address's own activity, shown with the
result and never scored:

- *Pass-through:* in and out each at least $10k, at most 5% retained,
  within 30 days.
- *New address:* first activity within 30 days.
- *High-volume new address:* a new address that has moved at least $1M.

TJBsbT…8Y5P ($7.11M in and out in 11 days) and TKKPgK…dk4V both show
pass-through and high-volume new. They are notes, not scores, because a
pass-through wallet can be layering or an OTC desk, and the data cannot
tell which. Thresholds are in weights.yaml under `behaviour`.

## D29 — Tether's blacklist, a three-state verdict, and a benchmark for it

**Date:** 2026-09-22 · **Status:** active · **Follows:** D28

The owner's product question is "a customer gives a wallet; is it clean?".
A score does not answer that, and an unmeasured answer cannot be called
precise. Three changes address it.

**Tether's own blacklist.** Tether freezes USDT by calling `addBlackList` on
its own TRON contract. The events are public, free and first-party. The
labeler now reads every `AddedBlackList`, `RemovedBlackList` and
`DestroyedBlackFunds` event and replays them in order. An address is frozen
from its latest addition until a later removal. As of 2026-09-22 that is
7,570 addresses (8,555 additions, 967 removals), labelled
`tether_blacklist` at confidence 1.0 in a new category, `frozen_funds`
(weight 85; justification in METHODOLOGY.md). A direct listing always
reports High (`always_wins`). The whole list is re-read daily, and
addresses Tether releases are retired through the new `Store.Retire`.

The effect was immediate. TKKPgK…dk4V, which the D28 follow-up had
finished at Low 10.4, reaches a frozen address with 54% of its traced
value. The TJBsbT…8Y5P network touches the same frozen address,
TBFsge…GdhD, at three points.

**Verdict.** Every result now carries a verdict: clear, caution or
high_risk, with a confidence and the reasons, in the API, bot, app, CLI and
PDF, and first in each.

- *High risk:* the address is directly listed, the band is High, or exposure
  to a category reaches its line (sanctions and terrorist financing 1%;
  frozen, stolen and darknet 5%; mixer and scam 10%).
- *Caution:* any smaller risk exposure, a Medium band, coverage under 80%,
  tracing unfinished (5% or more of value still at unfetched dead ends), or
  a behaviour note.
- *Clear:* none of the above. Unknown is never called clean.

Thresholds are in weights.yaml under `verdict`. They are checked against
the benchmark and never fitted to another vendor (SPEC.md §9.5).

**Benchmark** (`validate verdict`). Three sets with a known answer, drawn
from stored data:

| Set | n | Clear | Caution | High risk | Must be | Misses |
|---|---|---|---|---|---|---|
| Listed (frozen/OFAC, fetched) | 5 | 0 | 0 | 5 | high risk | 0 |
| Exposed (direct flow with a listed address) | 50 | 0 | 35 | 15 | never clear | 0 |
| Exchange deposit wallets | 50 | 17 | 33 | 0 | never high risk | 0 |

The first run called only 1 of 50 deposit wallets clear. Most were held
back by "tracing unfinished" when a single pending address carried a
negligible share of value. That rule now counts value, not addresses:
under 5% pending is finished. With it, 17 of 50 are clear and nothing else
moved. The remaining deposit wallets are cautions for low coverage, their
customers' money coming from unnamed sources, which is true.

What the benchmark does not prove: its bad sets come from our own lists, so
it measures whether exposure is detected and reported, not whether the
lists are complete. The competitor comparison (§9.5) and a hand-checked
clean set are still needed before "precise" can be claimed.

**Licensed data.** Researched in docs/LICENSED_DATA.md: Bitquery's labels
(71.6M TRON, public prices) and MistTrack are the realistic options, both
needing written resale terms. Arkham and Nansen forbid redistribution.
Chainalysis and TRM paid products are enterprise-priced. Buying any of them
means changing SPEC.md §1's "zero licence spend"; that is the owner's
decision.

## D30 — bugs found by comparing with a competitor, and what fixed them

**Date:** 2026-09-22 · **Status:** active · **Follows:** D29

The owner pasted AMLBot's result for TTrcHL…BPQp (Exchange 87.5%, High-Risk
Exchange 9%, Sanctions 1.6%) and asked for every divergence to be run down.
Doing so found four faults in our own output. Each is fixed and pinned by a
test.

**1. Unfetched addresses were expanded from fragments.** Beyond the origin,
the traversal expanded any address that had stored edges, including
addresses whose own history was never fetched. Those are known only through
transfers other addresses revealed. Our report said dust was 49% of value;
the real cause was an intermediate wallet whose only known inbound edges
were address-poisoning dust from look-alike addresses (…eRM). That wallet's
whole share went to "dust", which counts as attributed, so coverage read
94.6% and confidence high for an address mostly unknown. Traversal now takes
a `HistorySource` and ends at unfetched addresses as dead ends. They count
as unknown and are queued for fetching, so follow-ups deepen through them.
`TestUnfetchedAddressesAreNotExpandedFromFragments` reproduces the old
inflated coverage and pins the fix. With it, TTrcHL…BPQp's frozen exposure
fell from 7.2% to 0.2% on the first rescreen. The 7.2% had been traced
through unknown wallets.

**2. Service detection sampled the same addresses every run.** Candidates
were the 200 addresses with the most counterparties, labelled or not, so
known services filled the budget each time. THasRe…geRM (1,430
counterparties, $126.5M in and out) was never sampled. It supplies 99.86% of
TTrcHL…BPQp's inflow, and without a label the traversal walked through it
into its other customers, picking up their exposure as if it were the
screened address's. Candidates now skip labelled addresses. The budget is
config (`derived_service.max_candidates`, 2,000, about 3,000 API calls),
because 1,734 service-shaped addresses were waiting and 200 a day would
take over a week.

**3. A decisive risk connection could be hidden.** The report listed the
five largest connections, and a 3% frozen connection sat behind five large
services while the verdict named it. Risk connections are now listed first,
in chat, app and PDF.

**4. Sanctions exposure versus AMLBot's 1.6%.** Searching four hops each way
found OFAC addresses (Alireza DERAKHSHAN, GRINEX, Arash Estaki ALIVAND,
ZEDCEX, ANSARALLAH), all reached through the hub above. Their value-weighted
shares are 0.018% and 0.0014%, so "under 0.1%" is our honest answer.
AMLBot's 1.6% is not a value flow that can be reproduced from the chain. It
most likely comes from how AMLBot groups entities. The divergence is
recorded, not closed.

**5. The sample missed slow hubs, and the fix over-reached.** Even when
sampled, THasRe…geRM was rejected: its latest 600 transfers had few distinct
counterparties. Candidates whose full history is stored are now judged from
that history (400 transfers, 250 counterparties, 0.2 counterparties per
transfer). The first run accepted 1,609 with 7 API calls, including a hub
active for only 18 days. A 90-day minimum between first and last transfer now
separates a lasting service from a burst, and every run rechecks these labels
and withdraws those that fail: 246 were withdrawn, leaving 1,527.

**6. "Clean" rested on services nobody had named.** Once THasRe…geRM was
labelled, TTrcHL…BPQp came back clean with high confidence and 100%
coverage. All of its value ended at that one unidentified hub, and the hub's
own value ends at other unidentified hubs. Terminal labels are right for a
named exchange, whose customers are its problem. For an unnamed service they
hide the other side. The verdict now says caution, with low confidence, when
at least `verdict.max_unnamed_pct` (50%) of traced value ends at unnamed
services. Risk checks then show a neutral mark and say what they could not
see. AMLBot rates the address medium; we now say caution.

Two presentation faults went with it. The score line read "Risk level: Low"
under a caution verdict. It measures only value linked to risk lists, so it is
now called the exposure score and points to the verdict. Confidence had meant
"how completely traced", which read as "how sure" on a caution about the
unknown.

**How the answer is shown.** The owner asked for two states and a number:
*risky* or *not risky*, a confidence percentage, and a line or two on why.
Caution stays in the API as a level, but readers see it as not risky with a
lower confidence. Confidence is the share of traced value that could be
vouched for: named non-risk entities count fully, unidentified services half
(`verdict.confidence_credit`), dust and unknown value nothing, and each
behaviour note takes 15 points off. A risky answer's confidence is its
coverage, never under 50; a direct listing is 99. Behaviour reasons are
listed before coverage reasons, because only two are shown. Measured:
TTrcHL…BPQp and TNwf8V…crmL not risky at 50%; TPJZrw…uBhM at 1% (round split,
new with large volume); TJBsbT…Y5P at 4%, and 16% once follow-up fetching
finished. Below `verdict.insufficient_below` (10%) a not-risky answer also
says "not enough data", because "not risky" alone reads as a clean bill.

**Measuring the final answer.** Unfetched addresses now count as unknown, so
first answers show lower coverage (deposit-set mean 80% → 63.5%) until the
follow-up fetches them. `validate verdict -rounds N` screens with
prefetching, waits for the worker, and rescreens, measuring the answer a
customer gets after following up.

**Also added,** from a case the owner asked about (TPJZrw…uBhM: $7M from one
hub, split into six $1,000,000 transfers to fresh wallets within three
minutes, none of which has moved since):

- two behaviour notes, *round split* and *parked funds*, both caution
  reasons, thresholds in `behaviour`;
- a plain-language paragraph under every verdict saying why, in the
  reader's language.

---

## D31 — naming wallets by who created them, and what else closes the gap

**Date:** 2026-09-22 · **Status:** active · **Follows:** D19, D28, D30

Our largest gap against competitors is naming. They name exchanges; we
mostly say "unidentified service", so a wallet AMLBot calls 87.5% exchange
reads to us as 50% confidence. This entry records what was tried without
the owner, what worked and what did not.

**Account creation is first-party evidence.** A TRON account exists once
something pays to create it. `labeler activations` reads the creating
transaction of every labelled service wallet (1,922 read, 8 with none, in
13 minutes) into `activations`. Two rules follow from an exchange's own
reserve list, and only from it:

- whoever created a reserve wallet is the exchange. HTX's reserves were
  created with transfers of up to 951M, 600M and 100M TRX. A creation under
  10 TRX (`derived_operator.min_creator_trx`) is not counted, because
  anyone can send a fraction of a TRX to an address; two HTX reserves were
  first reached by 0.1 and 1 TRX;
- whatever a reserve wallet created is the exchange's, because reserves
  never pay customers. Poloniex's reserve TWhDfw…5M2 created 265 of its
  deposit wallets.

Wallets created by a hot wallet are not named: a payout to a customer's
fresh wallet creates it, so "created by an exchange's hot wallet" describes
customers too. Nine wallets are named (`derived:operator`), four of them
service hubs. The yield is small today because only 22 reserve wallets are
known. Each wallet named another way, such as a test transfer, now names its
creator and its creations too.

**Found in my own code:** the rule read its own earlier labels as "already
named", so the second run produced nothing and withdrew all eleven labels
the first had made. `OperatorInputs` now separates the rule's own output from
its inputs, and `TestReserveOperatorsRerunIsStable` pins it.

**Sources read on the owner's behalf.** Both remain closed to a paid
product:

- Chainabuse: the Terms of Use grant a licence "solely for your personal,
  non-commercial use". Only a Chainabuse Pro agreement changes that.
- OKX reserve addresses: Terms of Service §9.4 forbid commercial use
  without OKX's explicit authorisation, and §8.1 forbids copying. This is
  the same position as Binance in D19.

**First-ring fetching.** A screen now fetches its largest unknown direct
counterparties itself before answering (20 s budget, serial because
TronGrid penalises concurrency, only those carrying at least 5% of the
address's value). Measured with `validate ring` on two samples of 20
fetched wallets:

| | Coverage | Confidence | Screen time |
|---|---|---|---|
| Every counterparty (first sample) | 12.0% → 15.4% | 9.9% → 10.6% | +~20 s |
| Value-gated (second sample) | 29.9% → 30.7% | 12.9% → 16.6% | 11.1 s → 17.4 s |

The gain is concentrated: two wallets went from 1.6% to 51.7% and from 5.1%
to 57.9%. A counterparty too large to fetch within the budget used to cost
the whole budget; each now gets at most 8 s, and the worker finishes it.
The measurement repeated D30's queue mistake: `validate ring` queued its
screens' follow-ups at customer priority, and with ad-hoc test screens that
put 8,574 jobs ahead of a real request. It now queues in the background,
as `validate verdict` does.

**Comparison tooling.** `validate sample` writes a stratified sheet (15
listed, 25 exposed, 20 deposit, 10 service, 30 ordinary wallets) for the
owner to screen in a competitor's tool. `validate compare` on the filled
sheet sets our risky or not-risky answer, confidence and category shares
beside theirs, counted per stratum. The sheet lives under `.data/`, not in
this public repository, because a competitor's results are theirs to
publish.

---

## D32 — address-poisoning senders are labelled scam

**Date:** 2026-09-22 · **Status:** active · **Follows:** D31

A poisoner watches for a real transfer between a victim and a
counterparty. It generates an address that starts and ends like the
counterparty's, and sends the victim a worthless transfer from it. The
look-alike now sits in the victim's history, and a victim who copies the
usual address from there pays the poisoner. It was found while measuring
D31: a wallet we called "not risky" had sent 144 transfers of 2 sun to 94
addresses.

**The signature is exact.** Take a dust edge (under $1) from S to V where V
has a real counterparty C (at least $100) with S's first four and last four
characters. Counting from the leading T, that fixes seven random
characters, so a coincidence has odds of about one in two trillion. In
stored data:

| | |
|---|---|
| Look-alike pairs | 31,007 |
| Dust arriving after the real transfer it imitates | 30,937 (99.8%) |
| … within the hour | 11,106 |
| Senders labelled | 24,952 |
| Distinct victims | ~2,500 |
| Overlap with any other label | none |

Only senders with at least one reactive pair (dust after the real
transfer) are labelled, since a coincidence between vanity addresses could
only hide among the rest. The rule is rerun in full daily and withdraws what
it no longer produces. Joining on the recipient and the look-alike key
together keeps the query at 21 s. Filtering after a join on the recipient
alone ran past the ClickHouse read timeout.

**What the reader sees.** Screening a poisoner answers "RİSKLİ · güven %99",
names the address it imitates, and tells the reader to take the real address
from the recipient, never from history. The listing line says "Adres
zehirleme, TBkgik…EtN8 adresinin taklidi". No secondary reason follows a
listing or a poisoning: "coverage 51%" under "this is a poisoner" read as
if it mattered as much.

**Found in my own code on the way:** a verdict reason read "0.0% of its
money is linked to Frozen by Tether". Exposures under 0.1%, which the
report lists as "less than 0.1%", are no longer reasons
(`TestVerdictIgnoresNegligibleExposure`).

**The other side: warning the target.** A screened wallet that poisoning
senders have dusted gets a note: how many look-alikes reached it, one
address they imitate, and "never copy an address from history". The
victim did nothing, so the note changes neither verdict nor confidence.
Inbound dust is classified as dust before labels are consulted, so a
victim's traced exposure never turns scam because it was dusted. Of 40
victims screened, the 3 answered risky were risky for Tether-frozen
exposure of 6–27%, not for the poisoning.

**Batch queries get their own timeout.** Under a busy worker the
poisoning join ran past the 60 s read timeout meant for interactive
screens (3 m 37 s on the second run, 43,717 pairs, 33,540 senders). The
labeler now opens ClickHouse with a 15-minute read timeout
(`store.OpenClickHouseBatch`); screens keep 60 s.

**`named_service` split from `unnamed_service`.** The benchmark showed
all 50 exchange deposit wallets answering caution, because wallets we know
to be HTX's or Poloniex's carried the same category as wallets nobody can
name. The report called HTX "a service whose operator is unknown" and
confidence counted it at half. Reserve lists, and every wallet derived from
them (hot wallets, operators, deposits: 455 today), are now
`named_service`. It is weighted 15 like `unnamed_service`, because a name is
not a KYC tier (D19). Its value counts fully towards confidence, and it
never triggers the unidentified-service reason.

## D33 — Russian, the third language

**Date:** 2026-09-22 · **Status:** active · **Follows:** D27

The product is sold in Russia and the CIS, so the bot, the command menu,
the connections report, the Mini App, the PDF verdict and the terms exist
in Russian as well as English and Turkish. Telegram clients set to ru, uk,
be, kk, uz, ky or tg are answered in Russian; the Russian command menu is
registered for each of those codes. `/language` offers English / Türkçe /
Русский.

**The verdict headline** is "ЕСТЬ РИСК" or "НЕТ РИСКА", then "уверенность
X%", then one or two lines of why; under 10% confidence a not-risky answer
adds "недостаточно данных". Counts use Russian plural forms (1 адрес,
3 адреса, 5 адресов). Where a number would force agreement inside a
sentence, the Russian text puts it after a colon instead.

**The PDF font.** The PDF core fonts cover only Western European text:
Cyrillic, and Turkish ı, ş and ğ, printed as wrong glyphs. The report now
embeds Noto Sans (regular, bold, italic) under the SIL Open Font License
1.1, with the licence at `internal/report/fonts/OFL.txt`. Only the glyphs
used are embedded. `TestPDFFontsCoverEveryLanguage` reads the font's cmap
and fails if any letter in the three catalogues has no glyph. The PDF's
verdict block is written in the requested language. The analyst detail
below it stays English, as it was for Turkish. The CLI passes `-lang`;
`/v1/report` takes `lang` since D34, and the bot passes the user's.

**Open:** `config/terms.ru.txt` is a translation awaiting legal review; the
English text governs.

---

## D34 — Tether freezes: what the data supports, checking a payment, UK and EU lists

**Date:** 2026-09-22 · **Status:** active · **Follows:** D29, D32, D33

The owner asked how to get ahead of competitors in an international market
(Russia and the CIS, the EU, the US, the UAE). A payer's question is not
"what is this address" but "may I take or send this money". Three answers
to it were built, and one idea was measured and dropped.

**Freeze prediction is not supported, so it is not sold.** The idea was to
put a percentage on "Tether will freeze this wallet within 30 days". The
first step was to measure it. `validate freeze` takes 800 wallets Tether
froze between 2025-01-01 and 90 days ago, and stores only the USDT
transfers from the 30 days before each freeze: a page or two instead of
the whole history (`USDTWindowCursor`). The blacklist gives every
address's freeze time, so each counterparty's outcome is known without its
own history. The control is the same measure around 800 ordinary fetched
wallets, each paired with one sampled freeze time. Services are left out.

| Counterparties of a frozen wallet | n | frozen within 7 d | 30 d | 90 d |
|---|---|---|---|---|
| all | 22,804 | 0.64% | 0.70% | 0.75% |
| received ≥ $1k from it, last transfer < 7 d before | 2,664 | 3.00% | 3.08% | 3.15% |
| exchanged ≥ $10k | 5,827 | 2.01% | 2.18% | 2.37% |
| exchanged < $1k | 14,391 | 0.06% | 0.07% | 0.07% |
| control: ordinary wallets' counterparties | 2,583 | 0.04% | 0.12% | 0.19% |

The strongest group is about 25 times the control. The risk is almost all
in the first days, though: Tether freezes in clusters. For the strongest
group still unfrozen 3 days after the first freeze, the chance of a freeze
in the next 30 days is 0.27%, and 0.08% after 7 days, which is the
control's level. A 30-day percentage would be true for three days and
misleading afterwards, so it is not shown. What the data supports is speed:

- **A note, for three days.** A screened wallet that received at least
  $1,000 from a wallet Tether froze in the last three days, within the week
  before that freeze, gets a `frozen_contact` note. The note gives the
  measured 3.1% against 0.12%. Like the poisoning-target note (D32), it
  changes neither the verdict nor the confidence: exposure to frozen funds
  already counts through `frozen_funds` (D29). Thresholds are in
  `weights.yaml` under `behaviour.frozen_contact`.
- **The blacklist every ten minutes.** `labeler tether` (`make
  tether-install`) reads the blacklist alone into its own snapshot in
  35 s. It then makes every watch with direct flow to or from a newly frozen
  address due, and the bot's monitor, which ticks every five minutes,
  rescreens it. A watcher hears of a freeze within minutes rather than at
  the next six-hourly check. The refresh skips a run while another labeler
  run holds an open snapshot. Sealing a later snapshot while an earlier one
  is still being written would change what the sealed one resolves to
  (SPEC.md §2).

**Checking a payment before it is sent.** `POST /v1/presend` (bot:
`/send <recipient>` or `/send <your wallet> <recipient>`) screens the
recipient. Given the paying wallet, it also looks for a real counterparty
of that wallet (at least $100, the D32 bar) with the recipient's first and
last four characters. A match is a poisoning copy even when no label has
caught the poisoner yet. The decision is "do not send" when the recipient is
risky or is a copy. A first payment is said, not counted against. Tried
on two stored poisonings: the copy was stopped and named the address it
imitates ($150,000 and $10,000 of real history); the real address passed.

**Inline mode.** "@bot T…" in any chat offers a check. Telegram wants an
inline answer within seconds, and a screen can take a minute. So the result
sent is a placeholder, and on `chosen_inline_result` the screen runs as the
sender's screen, through the gate like any other, and the placeholder is
edited into the verdict. It needs inline mode and inline feedback enabled
with @BotFather (`/setinline`, `/setinlinefeedback`). A result carries a
keyboard, because without one Telegram does not report the message id
needed to edit it.

**UK and EU sanctions.** A research pass over international sources (dated
2026-09-22):

- **UK Sanctions List (FCDO):** allowed under the Open Government Licence
  v3.0. Addresses sit in free text. 49 TRON addresses: XINBI (40), AYASH
  (4), EXMO (3), Byex (2). The EXMO and Byex ones are absent from OFAC.
- **EU consolidated list:** configured since D14 but never ingested. Its URL
  had been answering 403, and the labeler only logged "not yet
  implemented". It needs the public `token` its own page uses. 3 TRON
  addresses, for Garantex and Grinex. Garantex's is absent from OFAC.

One parser (`ParseFreeTextSanctions`) reads both. It takes every
address-shaped token in a designation and keeps a TRON one only if its
base58check checksum holds. An EVM address is labelled on both Ethereum
and BSC, because its key controls it on both. Screening Garantex's EU
address answers risky at 99%, "Garantex (eu)". EXMO's UK address first
resolved to its Tether freeze. The cause was the running API's old
configuration, in which `uk` was unranked, not a code fault. A restart
resolved it to "EXMO EXCHANGE LIMITED (uk)". The API must be restarted
after any configuration change.

**Not used, and why:**

- **Israel NBCTF seizure orders:** 583 TRON addresses, 576 absent from
  OFAC, mostly Hamas, IRGC and Hezbollah USDT. The Ministry of Defense
  terms forbid redistribution without written consent, and the index sits
  behind bot protection. The single most valuable open list for TRON;
  written consent is the owner's step.
- **Exchange reserve lists:** Gate, Bitget, KuCoin, MEXC and Bybit publish
  them only on their own sites, under terms that forbid the use (D19, D31).
  None of their GitHub repositories holds a TRON address.
- **OpenSanctions:** CC BY-NC.

**Measured in my own code on the way:** the first run fetched each frozen
wallet's whole history and managed 17 wallets in ten minutes; fetching only
the window finished 800 in about 25 minutes.

**The API key's daily quota ran out.** After the backtest, every blacklist
read failed with 429 even at two requests a second. The response body said
why: "Exceed the user daily usage (100000), the maximum query frequency is
1 time per second". TronGrid's free key allows 100,000 requests a day, and
the backtest and the worker's 14,000-job queue spent them. The client used
to drop that body and log a bare "429", which hid the cause; it now keeps
it. For paying customers one free key is not enough: a paid TronGrid plan
or our own node is a go-live requirement. `validate freeze` costs about
one request per sampled wallet plus pages.

---

## D35 — background fetching has a daily budget

**Date:** 2026-09-23 · **Status:** active · **Follows:** D17, D34

The morning after D34, the refresh log showed the Tether blacklist had
failed every run from 23:43 to 03:44: four hours in which a new freeze
would have warned nobody. The API key's 100,000 requests a UTC day were
gone, and after that TronGrid serves one request a second to everyone
sharing the key. All 12,374 pending jobs were background work (priority
1000), which no customer was waiting on. The worker spends about 100
requests a minute, so left alone it uses the whole quota in about 16 hours.

Every process now counts its TronGrid requests (`tron.TakeRequests`) and
adds them every 15 seconds to `api_usage`, per UTC day, which is how
TronGrid counts. The worker reads the total at most every 30 seconds. Once
it reaches `background_share` (0.6) of `daily_request_quota` (100,000) in
`sources.yaml`, the worker claims only jobs below `BackgroundPriority`.
Customer screens, their follow-ups and the ten-minute blacklist refresh
keep the other 40,000. Background work resumes at midnight UTC.

The budget does not make the quota bigger. A paid TronGrid plan or our own
node remains a go-live requirement (D34). The budget only decides who goes
without when the quota runs out.
