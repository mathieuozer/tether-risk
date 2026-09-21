# External comparison data (SPEC.md §9.5)

Publicly-obtained scores for addresses, recorded verbatim as the counterparty
reported them. `cmd/validate` consumes this to produce the divergence table.

**SPEC.md §9.5 is explicit: do not tune weights to match these.** The value of
the table is in explaining where we differ and why. A divergence that we can
account for is evidence the methodology is understood; a match achieved by
fitting weights to someone else's output is evidence of nothing.

Obtaining these is a manual step (docs/PLAN.md F4).

---

## TAythDdKTZeNq6VnQ7o9cEvWRQGgRpPiKX (Tron)

**Source:** competitor screening tool · **Collected:** 2026-09-20
**Reported risk:** Low (26.7%)

| Category | Share |
|---|---|
| Exchange | 61.0% |
| Dust | 20.5% |
| Unnamed service | 14.8% |
| Token contract | 0.7% |
| Enforcement action | 0.6% |
| Gambling | 0.5% |
| High-Risk Exchange | 0.5% |
| Custodial wallet | 0.4% |
| DEX | 0.3% |
| Bridge | 0.2% |
| Sanctions | 0.2% |

Reported as under 0.1% each: Payment Service Provider, Other, Lending, Smart
contract, Stolen Funds, P2P exchange, Terrorist Financing, High-Risk
Jurisdiction, Scam, Privacy protocol, ATM, Mining Pool.

### Observations about the comparison itself

These shape the divergence table and are worth recording before any of our own
numbers exist, so they cannot be rationalised afterwards.

**1. No coverage figure is reported.** The shares sum to approximately 100%
with no unattributed bucket. Either everything traced was attributable to a
known entity, or unattributed value was excluded from the denominator. The
second is far more likely at this level of category granularity.

This is precisely the gap SPEC.md §7 makes mandatory for us: *"Do not hide
unknown exposure — showing it honestly is a deliberate design choice and a
differentiator, not a weakness to paper over."* Expect our percentages to look
worse against theirs while describing the same reality. That difference is the
product argument, not a defect to close.

**2. A 0.2% sanctions share still scores Low.** Worth being precise about, as
it is easy to misread as a scoring disagreement. SPEC.md §7 says *any direct
sanctions hit* reports High regardless of computed score. A 0.2% indirect
exposure several hops away is not a direct hit, so Low is defensible and our
engine should agree. Our override must fire on direct labels only; wiring it
to indirect exposure would send almost every long-lived address to High and
make the band meaningless.

**3. Their vocabulary is roughly twice ours.** 23 categories against our 12.
Categories they report that we cannot currently express at all: Token
contract, Enforcement action, Custodial wallet, Bridge, Payment Service
Provider, Lending, Smart contract, P2P exchange, High-Risk Jurisdiction, ATM,
Mining Pool, Other. Their "Privacy protocol" is approximately our `mixer`.
Our `darknet` has no counterpart in this output.

Any divergence table has to map their vocabulary onto ours, and that mapping
is lossy in one direction. See the open question in docs/DECISIONS.md.

**4. Dust at 20.5% is high and is a useful check on our own handling.**
SPEC.md §7 requires inbound dust be classified into a near-zero-weight
category and excluded from traversal expansion. If our dust share for this
address comes out wildly different from theirs, the threshold or the
inbound-only rule is worth re-examining before the weights are.

### What our Phase 1 data already shows for this address

Ingested 2026-09-20: 193 transfers, 35 outbound and 158 inbound, across TRX
and USDT. 17 distinct outbound counterparties against 125 inbound ones.

**Inbound TRX value distribution**

| Bucket | Transfers | Senders |
|---|---|---|
| < 0.001 TRX | 96 | 87 |
| > 1000 TRX | 1 | 1 |

84% of inbound senders (105 of 125) sent exactly once. That is the textbook
dusting signature, and it confirms the address is a dusting target.

**Predicted divergence, recorded before our scoring exists.**

96 dust transfers of under 0.001 TRX are worth roughly two cents in total. As a
share of *value* — which is what SPEC.md §7 mandates, *"normalise to
percentages of total traced value"* — that is approximately 0.00%. The
competitor reports Dust at 20.5%.

Their figure cannot be value-weighted. Checking the plausible alternatives
against our data: dust senders are 70% of senders (87/125) and dust transfers
are 50% of transfers (96/193). Neither is 20.5% either, so their denominator
also spans hops we have not traced yet. Whatever the exact construction, it is
not value.

So: **our dust share for this address will be near zero where theirs is 20.5%,
and that is the design working, not a bug.** SPEC.md §7 requires that inbound
dust "never meaningfully move the score". Value-weighting achieves that
automatically, because unsolicited dust carries no value by definition. A
breakdown where dust occupies a fifth of the chart has let anyone who can
afford 96 transactions reshape a stranger's risk profile.

This entry exists so that when the divergence table is produced and our dust
row reads 0.0% against their 20.5%, the explanation is one that was written
down in advance rather than constructed afterwards to be reassuring.

### Measured result, 2026-09-21

Our engine, run against label snapshot 1 (458 OFAC + 1 curated label, **no
exchange labels at all**):

| | Competitor | Ours |
|---|---|---|
| Risk level | Low (26.7%) | Low (0.0) |
| Coverage | not reported | **0.0%** |
| Dust | 20.5% | **0.0%** |
| Exchange | 61.0% | 0.0% |
| Unattributed | not reported | **100.0%** |

**The dust prediction held.** Written down before scoring existed: our dust row
would read approximately zero where theirs reads 20.5%, because SPEC.md §7
mandates percentages of traced value and the dust on this address is worth
about two cents. It came out at 0.0%.

**Our 0% coverage is honest, not broken.** We hold 333 TRON sanctions
addresses and one token contract. We hold no exchange labels, so nothing this
address transacted with can be attributed to a named entity. The competitor
reports 61% Exchange because they have exchange labels and we do not.

The right conclusion is not that our engine is worse at tracing. It traced the
same flows — 193 transfers, 17 outbound and 125 inbound counterparties, with
individual USDT edges over $800,000. What it cannot do is *name* the
counterparties. Coverage says so, in the response, prominently, which is
exactly what SPEC.md §7 asks for and exactly what the competitor's output does
not do.

Closing that gap is a labelling problem, not a traversal problem, and the fix
is populating `config/curated_labels.yaml` with verified exchange hot wallets
plus implementing the Dune spellbook ingester. Until then the honest report is
"we could not attribute this", not a confident-looking Low.

**A bug this comparison caught.** The first run of this address returned "no
traced value" for outbound and 100% dust for inbound, with 100% coverage. That
looked like a clean result and was not: traversal was dropping the value of
expanded nodes that led nowhere, so unknown exposure disappeared from both the
numerator and the denominator. Without a real address to compare against, a
0%-coverage result and a silently-empty one are hard to tell apart. Fixed, and
`TestDeadEndsAreCountedAsUnattributed` now pins it.
