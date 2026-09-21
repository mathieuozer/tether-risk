# METHODOLOGY.md

How this engine produces a risk score, what it is built on, and where it
cannot deliver what it would like to.

> **This is a triage and pre-screening tool built on open data. It is not a
> regulated AML determination and must not be used as one.** It exists to help
> a reviewer decide what deserves attention, not to decide anything on their
> behalf. Every number here is reconstructible from stored data; none of it is
> a judgement about a person.

---

## 1. What the engine answers

Given a blockchain address, it reports exposure by counterparty category as a
percentage of traced value — separately for inbound and outbound — a composite
0–100 score, a **coverage** figure saying how much of that traced value could
actually be attributed to a known entity, and the specific paths that drove the
score.

Coverage is the number to read first. A score computed over 8% coverage is a
statement about 8% of the value, and the other 92% is unknown rather than
clean.

---

## 2. Data sources and their licences

### Chain data

| Chain | Source | Status |
|---|---|---|
| TRON | TronGrid public API, free tier | Live |
| Ethereum | JSON-RPC (BigQuery backfill not built) | **Partial — recent blocks only** |
| BSC | JSON-RPC | **Partial — recent blocks only** |

The EVM adapter is implemented and tested, including against a live endpoint:
it parses ERC-20 Transfer logs correctly, honours per-endpoint block-range
caps by narrowing automatically, and skips logs from reorganised blocks. What
it **cannot** do is fetch an address's history, and the reason is worth stating
precisely because it is a property of the available endpoints rather than of
the code.

Raw JSON-RPC has no per-address history call. Substituting for one means
scanning every block since the address first appeared with a topic filter,
which needs archive access. Measured across nine public endpoints on
2026-09-21:

| Endpoint | Archive behaviour |
|---|---|
| publicnode.com | refused outright |
| ankr, drpc | authentication required |
| 1rpc.io, pokt | served, capped at **50 blocks per request** |
| bsc-dataseed | `eth_getLogs` rate-limited below usefulness |

A 50-block cap is roughly 520,000 requests to cover Ethereum's history. So
demand-driven ingestion on these chains needs either a provider key with
archive access or the BigQuery backfill path, and until one exists
`FetchAddress` returns a typed `ErrArchiveRequired` rather than an empty page.
An empty page would read as "this address has no history", which is a
different and wrong claim.

The API returns `chain_unavailable` for these chains for the same reason.

Ingestion is demand-driven per address rather than full-chain, with results
cached by a TTL and by fetch depth.

### Label sources

| Source | Confidence | Licence | Status |
|---|---|---|---|
| OFAC SDN | 1.0 | US Government work, public domain | **Live** |
| UN Consolidated | 1.0 | Reproduction permitted with attribution | Declared, not implemented |
| EU Consolidated | 1.0 | Reuse permitted (2011/833/EU) | Declared, not implemented |
| Curated in-repo | 0.95 | This repository | **Live**, nearly empty |
| Dune Spellbook | 0.8 | Apache-2.0 | **Unavailable — contains no address data** |
| CryptoScamDB | 0.5 | MIT | **Live** |
| ScamSniffer | 0.5 | MIT | **Live** |
| Etherscan / BscScan / Tronscan | 0.9 | — | **Blocked, deliberately** |
| Chainabuse | 0.5 | — | Needs terms review |

**On the abuse feeds and what they actually cover.** Both were measured
against the live data on 2026-09-21 rather than assumed:

| Source | Addresses | Of which TRON |
|---|---|---|
| ScamSniffer | 2,530 | **0** |
| CryptoScamDB | ~5,300 | **20** |

ScamSniffer carries no TRON addresses at all; every entry is EVM. CryptoScamDB
carries twenty. TRON is currently the only chain with a live data path, so
neither source meaningfully improves TRON coverage today — their value is the
~5,500 EVM addresses waiting for an Ethereum endpoint.

This is recorded because it would be easy to see "abuse feeds: live" and infer
coverage that does not exist. A source being ingested is not the same as a
source being useful for the chain you are querying.

**On the Dune Spellbook.** SPEC.md §6.3 planned to clone the repository and
ingest its labels schema, which was the intended route to exchange labels.
Checked on 2026-09-21, it cannot work: the label models are dbt SQL selecting
from Dune's own warehouse rather than static data, the files contain no
addresses, and there is no TRON label model at all. Running the queries would
need a Dune subscription, which §1 rules out. See docs/DECISIONS.md D14.

**On the block explorers.** The specification asks for their public address
nametags, and also says to record each site's terms and stop rather than work
around anything prohibiting redistribution. Those two instructions resolve to
one outcome: Etherscan does not expose labels through its API at all, they
exist only on rendered pages, and its terms prohibit automated extraction and
redistribution. So no scraper exists and none should be written while that
clause stands. The lost coverage appears honestly in the coverage figure.

---

## 3. Label confidence and resolution

Each source writes its own labels with its own confidence. When an address
carries several:

1. **Sanctions and terrorist financing always win**, regardless of confidence.
   Being wrong about an exchange costs a false positive; being wrong about a
   sanctions hit costs a missed one.
2. Otherwise **highest confidence wins**.
3. Ties break by **source priority** from `config/weights.yaml`.
4. Remaining ties break by category, then entity, then row id, so the ordering
   is total and the result cannot depend on database row order.

Conflicting *categories* — not merely differing confidence for the same
category — are recorded in `label_conflicts` for human review rather than
silently merged.

**Unverified abuse reports are capped.** An address whose only evidence is
community abuse reports cannot reach a high band on that basis alone; it needs
at least two independent abuse sources, or corroboration from a non-abuse
source. One report is an accusation, not evidence.

### Snapshots

Labels are versioned as a slowly-changing dimension: every row is valid over a
half-open range of snapshot ids. A score stamps the snapshot it resolved
against, so a score produced last week reproduces exactly today. Scoring
resolves only against *sealed* snapshots; an open one is still being written.

---

## 4. The derived deposit-wallet heuristic

An address that repeatedly forwards funds to a known exchange hot wallet, with
little other activity, is a deposit wallet of that exchange. This is what
closes most of the gap against commercial feeds.

Current thresholds (`config/weights.yaml`, `derived_deposit`):

| Parameter | Value |
|---|---|
| Minimum transfers to the hot wallet | 3 |
| Minimum share of outbound value to it | 80% |
| Maximum share going anywhere else | 20% |
| Confidence emitted | 0.6 |

Every emitted label records the full arithmetic in `evidence` — the hot wallet,
transfer counts, value shares and the thresholds in force — so a reviewer can
recompute the judgement rather than trust it.

## 4b. Behavioural service detection

No citable source for named TRON exchange hot wallets exists within the
project's constraints, so services are detected from behaviour instead. An
address transacting with hundreds of distinct counterparties while still
paginating after a deep sample is a service.

| Threshold | Value |
|---|---|
| Transfers sampled | 400 minimum |
| Distinct counterparties | 250 minimum |
| History must not be exhausted | yes |
| Confidence emitted | 0.75 |

**These are labelled `unnamed_service`, never `exchange`.** The behaviour
proves an address is a service; it does not identify which one. `exchange`
carries weight 2 and `unnamed_service` 15, so the distinction changes a score
sevenfold. A verified name from the curated source outranks this and should
replace it.

Every label records the measurement and a URL that reproduces it.

Measured results, 2026-09-21, over 101,869 transfers and 18,700 addresses:

| | Value |
|---|---|
| Candidates sampled | 160 |
| Services detected | 24 |
| Typical detected profile | 600 sampled transfers, 510-600 distinct counterparties, still paginating |

Effect on coverage for two reference addresses, both previously at zero:

| Address | Coverage before | Coverage after |
|---|---|---|
| TAythDdKTZeNq6VnQ7o9cEvWRQGgRpPiKX | 0.0% | 4.7% |
| TNwf8VBNCkg7Y1pgyzbHdWdekkamoqcrmL | 0.0% | 18.1% |

Both remain low-confidence, and correctly so — the majority of traced value is
still unattributed. The point is that the unattributed share is now an honest
81.9% rather than a silent 100%.

### Measured precision

**Not yet measured.** The heuristic anchors on known exchange hot wallets, and
the curated label set currently contains none, so it has never produced a
label. Precision cannot be measured against an empty output, and no number is
recorded here in place of one.

The measurement harness exists (`MeasurePrecision`) and deliberately reports
precision as *unmeasurable* rather than as 0.0 when no ground truth is
available, because a 0.0 in this table would look like a finding.

**This is the single highest-value piece of work outstanding.** Populating
`config/curated_labels.yaml` with verified exchange hot wallets unblocks the
heuristic, which in turn is what moves coverage off the floor.

---

## 5. Traversal

Breadth-first over the aggregated `edges` table, one direction at a time.
Traversal never reads raw transfers.

| Rule | Value | Why |
|---|---|---|
| Max hops | 5 | Beyond this, contributions are negligible after decay |
| Stop at labelled addresses | always | Without it everything reaches a major exchange within six hops and every score becomes noise |
| Fan-out cap | 5,000 per node, highest value first | Bounds cost; hitting it is recorded so the result admits truncation |
| Cycle detection | per traversal | — |
| Minimum contribution | 0.0001 | Bounds path explosion |

### Dust

Anyone can send an unsolicited transfer to any address, so inbound dust must
never meaningfully move a score. Inbound edges whose **mean** transfer value is
below $1 are classified `dust` (weight 5) and excluded from expansion entirely.

The mean is used rather than the edge total, so a dusting campaign cannot
escape the rule by being large. Dust is classified *before* the
minimum-contribution filter: dust is by definition tiny, and filtering it first
would mean the report showed no dust at all, when "no dust" and "dust worth two
cents" are different findings.

Dust classification applies to inbound only. An outbound transfer of any size
is a deliberate act by the address owner.

---

## 6. Haircut propagation and the score

Value splits proportionally at each node:

```
contribution(path) = share(hop1) * share(hop2) * ... * share(hopN) * decay^N
```

with `decay = 0.5` per hop. Contributions accumulate by category and normalise
to percentages of total traced value.

```
score = sum(category_pct * category_weight) / 100
```

All value arithmetic uses decimal, not binary floating point, so a reviewer
recomputing a score by hand from the stored path set gets the same number back.

### Weight table

| Category | Weight | Justification |
|---|---|---|
| `sanctions` | 100 | A legal prohibition, not a risk signal. Maximum by definition. |
| `terrorist_financing` | 100 | As above. |
| `darknet` | 90 | Proceeds are almost always criminal; near-zero legitimate use. |
| `stolen_funds` | 85 | Documented theft. Slightly below darknet because victims' own funds move through these paths too. |
| `mixer` | 70 | Deliberate obfuscation. Not illegal in itself and has legitimate privacy uses, which is why it is not 90. |
| `scam` | 70 | Fraud proceeds. Equal to mixer because the evidence quality is comparable and the harm is direct. |
| `high_risk_exchange` | 40 | Absent or nominal KYC. A real signal, but these process large volumes of ordinary activity. |
| `gambling` | 25 | Legal in most jurisdictions; elevated only because it is a common layering venue. |
| `unnamed_service` | 15 | Service-shaped behaviour with an unidentified operator. Mild, and mostly a prompt to investigate. |
| `dust` | 5 | Near-zero by design: the address owner did not consent to receiving it. |
| `dex` | 5 | Ordinary activity; non-custodial and fully transparent on-chain. |
| `exchange` | 2 | Functioning KYC. Reaching a regulated exchange is close to reassuring. |

Bands: **Low 0–30, Medium 30–60, High 60–100.**

**A direct sanctions hit on the queried address reports High regardless of the
computed score.** This applies only to a direct hit. Indirect exposure several
hops away does not trigger it — wiring it that way would send almost every
long-lived address to High and make the band meaningless.

The score is the **worse** of the two directions, not their average. Averaging
lets clean inbound history dilute outbound exposure to a mixer, which is
precisely the case a screening tool exists to surface.

---

## 7. Coverage

```
coverage = attributed_value / total_traced_value
```

Reported in every response. Below 40% the result is marked low-confidence and
the report says so prominently.

Value counts as **attributed** when it reaches a labelled address or is
classified as dust. It counts as **unattributed** when it runs out of hops,
hits the fan-out cap, or reaches a node with no further known activity.

That last case matters more than it sounds. A dead end usually means "we have
not ingested this address yet", not "this address never moved funds again", so
it is unknown and counts against coverage. An earlier version dropped that
value from both the numerator and the denominator, which produced results that
looked clean and complete while hiding everything they could not explain.

---

## 8. USD valuation

| Basis | Applies to | Quality |
|---|---|---|
| `pinned` | USDT, USDC, TUSD, USDD | Assumes the peg holds |
| `daily_close` | TRX, ETH, BNB | Daily granularity — real error on any individual transfer |
| `unpriced` | everything else | Contributes no value; counted, never assumed zero |

Every transfer records which basis applied, so a score resting on
imprecisely-valued native flows can be distinguished from one resting on
stablecoins. Prices are not interpolated across gaps: a missing day is
unpriced, because interpolating would invent a number that looks like a
measurement.

---

## 9. Known limitations

1. **Coverage is currently near zero for most TRON addresses.** The label set
   holds 5,976 labels, but only 353 are TRON (333 sanctions, 20 scam) and none
   are exchanges. The engine traces flows correctly but
   usually cannot name the counterparties. This is a labelling gap, not a
   traversal one, and the reports say so rather than rounding it away.
2. **The deposit-wallet heuristic has never run**, for the same reason.
3. **Ethereum and BSC have no live data path.**
4. **Block-explorer labels are deliberately not ingested.**
5. **TRC-20 transfers carry a synthetic log index.** TronGrid's TRC-20
   endpoint returns no event index, so one is derived by hashing the
   transfer's identifying fields. Two transfers in one transaction sharing
   sender, recipient, token *and* value collapse into one. This undercounts,
   which is the safer direction, but it is a real loss.
6. **TRC-20 transfers carry no block number.** That endpoint does not return
   one. Nothing depends on it — ordering uses block time — but the column is
   zero for TRC-20 rows.
7. **Native-asset valuation is daily-granularity.**
8. **No UTXO clustering, no Bitcoin, no machine learning.** By design.
9. **Single-address query only.** No continuous monitoring.

---

## 10. Reproducibility

Every result stamps a **label snapshot id** and a **config version** (the
SHA-256 of `weights.yaml` and `sources.yaml`). Identical input against the same
pair reproduces byte-identically; this is enforced by test, not assumed.

Every score persists its full path set — hops, per-hop value shares, the decay
applied, and the terminal entity — so any number can be reconstructed from
stored rows rather than re-derived by re-running the engine.
