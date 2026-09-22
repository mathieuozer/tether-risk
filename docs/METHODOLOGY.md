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

A candidate is judged only once its own history has been fetched. Before
that, the stored edges show only its transfers to the hot wallet, and every
such address would pass with a 100% share. Unfetched candidates that could
pass are queued (`fetch_candidates`, 200 per run) and judged on a later run.

**Reserve-list anchors (D25).** Addresses on HTX's and Poloniex's own
proof-of-reserves lists also anchor the heuristic (`anchor_sources`). What
sweeps into a reserve wallet is not always a customer deposit wallet: some
senders move hundreds of millions in a handful of transfers, which is the
exchange's own wallet. These labels are therefore named for what was
observed, "Poloniex (sends to its reserves)", and keep the anchor's
`unnamed_service` category. First runs, 2026-09-22:

| | Value |
|---|---|
| Anchors (HTX 15, Poloniex 7) | 22 |
| Addresses seen sending to them | 9,439 |
| Fetched and judged | 400 |
| Accepted | 208 (Poloniex 197, HTX 11) |

**Precision is not yet measured** for either anchor kind. SPEC.md §6 requires
it, and it needs a hand-labelled sample, which does not exist yet.

## 4a. Derived exchange hot wallets (D28)

A service-shaped wallet (at least 250 counterparties) that exchanges transfers
both ways with one exchange's own reserve wallets (at least 3 each way, at
least $100k from the reserves), with at least 90% of its reserve traffic going
to that exchange, is labelled that exchange's hot wallet (`derived:hotwallet`,
confidence 0.8). Measured 2026-09-22 against the 22 HTX and Poloniex reserve
wallets: 12,282 wallets had some reserve flow and 5 passed, all HTX. The
rejected cases are the reasons for each rule. One-way inflow means a
withdrawal or an OTC payment. Two to four counterparties means cold storage.
Two-way traffic with both exchanges' reserves means a market maker.
Precision is unmeasured, like §4's.

## 4c. Behaviour notes (D28)

Notes on an address's own activity accompany every result and are never
scored:

- **pass-through:** in and out each at least $10k, at most 5% retained,
  within 30 days;
- **new address:** first activity within 30 days;
- **high-volume new address:** a new address that has moved at least $1M.

They record what the address did. The same pattern fits layering and an OTC
desk, and the data cannot tell them apart, so a note never moves the band.

## 4d. Verdict (D29)

Every result carries a verdict derived from it: **clear**, **caution** or
**high risk**, with a confidence and the reasons. High risk means a direct
listing, a High band, or exposure at or above a category's line (sanctions
and terrorist financing 1%; frozen funds, stolen funds and darknet 5%; mixer
and scam 10%). Caution means any smaller risk exposure, a Medium band,
coverage under 80%, 5% or more of value still at unfetched dead ends, or a
behaviour note. Clear means none of these. Confidence is high at 90%
coverage or more with tracing finished, medium at 60% or more, and low
below that. A direct listing is high risk with high confidence at any
coverage.

Measured with `validate verdict` on 2026-09-22. No listed or exposed
address was called clean, and no exchange deposit wallet was called high
risk (see D29 for the table and what it does not prove).

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

**Judging from stored history (D30).** A candidate whose full history is
already stored is judged from that history rather than from an API sample,
because a sample of its latest 600 transfers can miss a hub whose customers
change slowly. It needs:

| Threshold | Value |
|---|---|
| Stored transfers | 400 minimum |
| Distinct counterparties | 250 minimum |
| Counterparties per transfer | 0.2 minimum |
| Days between first and last transfer | 90 minimum |

The 90-day rule separates a lasting service from a wallet that was busy for a
few weeks (a young hub, 18 days old, had been labelled a service before the
rule). Every run rechecks labels made this way and withdraws those that no
longer pass. Candidates whose history is not stored are sampled as before.

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

## 4e. Wallets named by account creation (D31)

A TRON account exists once something pays to create it, and the creating
transaction is on chain. Starting from an exchange's own reserve list:

| Rule | Why it holds |
|---|---|
| The creator of a reserve wallet is the exchange | Reserves were created with up to 951M TRX; only the owner makes that transfer. Creations under 10 TRX are ignored, because anyone can send a fraction of a TRX. |
| A wallet a reserve created is the exchange's | Reserves are cold and never pay customers. |

Wallets created by hot wallets are not named, because a payout creates a
customer's wallet the same way. Labels are `derived:operator`, confidence
0.8, category taken from the reserve list (identity, not a KYC tier).

## 4f. Address-poisoning senders (D32)

A dust transfer (under $1) from S to V, where V has a real counterparty C
(at least $100) sharing S's first four and last four characters, is a
poisoning. Counting from the leading T, seven random characters agree, so a
coincidence has odds of about one in two trillion. A sender is labelled
`scam` (`derived:poisoning`, confidence 0.9) when at least one of its dust
transfers arrived after the real transfer it imitates. That was 99.8% of
31,007 pairs, 11,106 of them within the hour. Screening such a sender
answers risky, names the address it imitates, and tells the reader to take
addresses from the recipient rather than from history.

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
| `frozen_funds` | 85 | Tether froze the address's USDT on its own contract, typically at a law-enforcement request over hacks, scams or sanctions. Authoritative for the fact of the freeze, silent on its reason, so set with stolen funds rather than sanctions. A direct listing always reports High (D29). |
| `mixer` | 70 | Deliberate obfuscation. Not illegal in itself and has legitimate privacy uses, which is why it is not 90. |
| `scam` | 70 | Fraud proceeds. Equal to mixer because the evidence quality is comparable and the harm is direct. |
| `high_risk_exchange` | 40 | Absent or nominal KYC. A real signal, but these process large volumes of ordinary activity. |
| `gambling` | 25 | Legal in most jurisdictions; elevated only because it is a common layering venue. |
| `unnamed_service` | 15 | Service-shaped behaviour with an unidentified operator. Mild, and mostly a prompt to investigate. |
| `named_service` | 15 | A service whose operator is named by its own reserve list or by chain evidence tied to it, KYC tier unrated. Weighted as `unnamed_service` because a name is not a KYC claim (D19). Its value counts fully towards confidence (D32). |
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
2. **The deposit-wallet heuristic runs only on HTX and Poloniex reserve
   wallets** (D25), and its precision is unmeasured. With no exchange hot
   wallets labelled, it has nothing else to anchor on.
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
