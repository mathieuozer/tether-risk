# Licensed address-attribution / AML data for a TRON-first USDT screening product

Research date: 2026-09-22. Everything below comes from vendor pages opened on that date unless it is marked **reported** (a third-party figure) or **unverified**. Several vendor pages (Crystal, Breadcrumbs pricing, Tronscan docs) returned HTTP 403 to automated fetches, so I could not read them. Those rows say so.

**Repo context (read-only check).** `SPEC.md:30` currently says "No commercial data feeds (Chainalysis / TRM / Elliptic). Zero licence spend." `config/sources.yaml` already ingests OFAC/UN/EU sanctions, Tether's on-chain USDT blacklist, exchange proof-of-reserves lists, CryptoScamDB and ScamSniffer. It blocks Etherscan/BscScan/Tronscan tags and marks Chainabuse `needs_review`. Buying any option below means changing that SPEC line. Each licence should be recorded in `sources.yaml`, using the same `redistribution:` discipline the file already follows.

---

## 1. Comparison table

| Provider | What you get | TRON / USDT-TRC20 | Price (public?) | Can results be shown to *your* paying customers? | Notes |
|---|---|---|---|---|---|
| **Bitquery** – Address Labels add-on | Entity labels (≈45 types: exchange hot/cold/deposit, sanctioned, USDT-frozen, mixer, darknet, scam, gambling, payment processor…). GraphQL, 100 addresses per query. **Bulk export on Enterprise.** | **Yes, deepest chain: 71.6M TRON labels** (of 169M+ total) | **Public:** labels $99/mo ($79.20/mo yearly) on top of a plan: Pro $79/mo, Scale $239/mo (billed annually). Bulk export: contact sales | **Not without written consent.** ToS bans "sell, resell, license… material of Bitquery" and "develop any third-party applications that interact with the Service without our prior written consent". Public disclosure is allowed only on paid plans | Cheapest TRON label volume found. Label quality/provenance is heuristic plus curated (unverified accuracy) |
| **MistTrack** (SlowMist) OpenAPI | Per-address labels (entity + type: exchange/defi/mixer/nft), risk score 3–100 with a risk-detail breakdown, counterparty, profile, actions. Per-call only, no bulk | **Yes**: TRX, USDT-TRC20, USDC-TRC20, USDD-TRC20. Also ETH, BSC, 20+ chains | **Public:** Standard $689/mo (API 1 rps, 10k/day). Compliance $2,069/mo. Developer "from $20/yr", $20–$30,000/yr for 100–1M calls, 10 rps. x402 PAYG: labels $0.10, risk score $1.00 | **Unclear.** Terms of Use say nothing on API redistribution. Must get written confirmation | Very TRON-focused. Claims 500M+ labeled addresses (vendor claim). Payments non-refundable. $10/48h Standard trial |
| **Arkham Intel API** | Entity attribution (claims 3.1B addresses labelled, "94% on-chain value attributed"), 0–100 risk score, counterparties, batch (up to 1,000 addresses) | Yes (Tron listed on AWS Marketplace listing) | Credit-based. Credit prices not public. AWS Marketplace listing is $0 "free tier". Access by application form | **No.** API ToS forbids distributing the Services "or any portion thereof to any third party without Company's prior written consent" and "display[ing] any compilation or directory based upon information… derived from the Services". Bars use to build competing products and bars users "affiliated with… any competing crypto analytics… company" | Not usable for a customer-facing product without a bespoke licence |
| **Nansen API** | Labels, profiler, smart money | Unverified | **Public:** Pro $49/mo (annual) / $69/mo, 2,000 credits. Extra credits $10 per 1,000 (per search results; not re-verified). "Premium labels" = 500 credits per call | **No for labels.** Redistribution guide: labels "cannot be redistributed under any circumstances" | Excluded for this use |
| **Chainalysis** – free Sanctions API + Oracle | Boolean-style "is this address sanctioned" (OFAC SDN and other major authorities) | API is address-string based. Whether it returns TRON listings is **unverified**; test with a known SDN TRX address. **Oracle is EVM-only** (ETH, BSC, Polygon, Avalanche, OP, Arbitrum, Fantom, Celo, Blast, Base) | Free. 5,000 requests per 5 minutes per key | Free product with no specific licence found. Chainalysis AUP bans sublicensing/"service bureau" use and building "substantially similar" products. Read the key's terms at signup | Mostly redundant with the OFAC XML you already ingest. Use as a cross-check only |
| **Chainalysis** – KYT / Address Screening (paid) | Entity attribution, exposure by category, risk scores, alerts | KYT supports Tron (per DFNS integration docs) | **Not public.** **Reported** (Vendr buyer data): average ≈ $174.7k/yr, range ≈ $25.7k–$297.3k/yr | AUP: may not "sublicense, sell, lease (including on a service bureau basis), share, distribute… or make it available to anyone other than Licensee". Also no "substantially similar" products | Industry standard. Price and AUP are hostile to a small reseller-style product |
| **TRM Labs** – free sanctions API | `isSanctioned` per address | Chains not stated in docs (unverified for TRON) | Free: 1 rps / 100 per day without key; with key 1,000 rps / 100,000 per day | Docs refer to a "Proprietary API" licence, but no terms were visible | Worth adding as a second sanctions cross-check |
| **TRM Labs** – Wallet Screening (paid) | Entity attribution, 155+ risk configurations, <400 ms | 184+ chains claimed. TRON not named on the page (very likely, unverified) | **Not public** (demo request). **Reported** by Vendr: often 10–25% below Chainalysis for similar scope | Not public. Assume enterprise no-resale by default | Also runs Chainabuse (see below) |
| **Elliptic** – Lens / API | Risk score 0–10, entity identification in 70+ categories, <500 ms sync (per search summary) | **Yes**: TRC-20 named in the Nov 2025 Wallet-in-Telegram announcement | **Not public** | Has an **Integration Partner** track for custodians/wallets/"KYC/AML or regtech" firms to "enhance their offering" with Elliptic scoring. Terms not public | Closest precedent to this product: Wallet in Telegram (100M+ users) embeds Elliptic, TRON included |
| **AMLBot** | Risk score and category exposure. Pro+ has API, KYT, numeric scores | Yes (TRC20 is its core market) | Lite bundles from $9/20 checks (**reported**, via search, not seen on a primary page). Pro/Pro+ "Available on Request". Pro+ needs corporate KYB | Not public | **Direct competitor** (sells Telegram AML checks). Many look-alike "AMLBot" sites on vercel.app / .services / .limited; use only amlbot.com |
| **Scorechain** | Risk/exposure, entities ("1B+ entities labeled", 2,700+ VASPs), SDK | Yes: TRX + TRC10/TRC20 | **Not public** (pricing page gives principles only) | Not public | EU (Luxembourg) vendor. Pricing is based on volume |
| **Crystal Intelligence** | Risk/entity API, Expert UI | Tron added (vendor product update) | Not public. Site returned 403 to my fetch | Not public | Could not verify details |
| **Merkle Science** | Compass screening/monitoring | TRON + USDT added (vendor blog) | Not public. No free trial (**reported**, Techjockey) | Not public | Enterprise-oriented |
| **Breadcrumbs** | Risk Score API (1–4 scale + indirect risk), labels | TRON is a "premium chain" | Compliance plan from $999/mo (**reported** via search snippet; pricing page 403) | Not verified | Cheaper mid-tier; not verified |
| **Chainabuse** (TRM) | Community scam reports + confidence | Yes (Tron listed) | Free key: 10 calls/month (50 reports per call). Partner access up to 5,000 calls/hour on request | Partner terms not public | Already `needs_review` in `sources.yaml`. Worth a partner request |
| **Blockchair** | Raw chain data | Yes | Public API plans | – | **No entity attribution product found**. Not a label source |

---

## 2. Per-provider detail

### Bitquery – Address Labels API
- **What:** GraphQL cube `Metadata.Labels`. About 45 label types grouped as exchanges (hot/cold/deposit/withdrawal), risk (sanctioned, "banned-by-usdt"/issuer-frozen, mixers 495K, darknet 2.3M, scams, phishing, ransomware, hackers), services (gambling 8.4M, payment processors 5.9M, mining pools, OTC, market makers) and protocols. Exchange deposit addresses total 119.5M and hot wallets 20.2M. Append-only with `RecordedAt` timestamps, so you can poll for new labels. Provenance is "on-chain detection (sweep patterns, blacklist events), contract metadata, and curated research". — https://bitquery.io/products/address-labels-api
- **TRON:** 71.6M labelled TRON addresses, the largest chain in the set (BSC 29.8M, ETH 25.8M, BTC 25.0M). — same page
- **Price:** labels add-on $99/mo ($79.20/mo yearly). Plans (billed annually): Personal $39 (non-commercial), Pro $79 (1M points ≈ 200k calls/mo, commercial use permitted), Scale $239 (5M points), Enterprise custom with "S3 bulk exports". Extra points $40/1M. — https://bitquery.io/pricing
- **Terms:** the ToS prohibits using the service in a way that could "be a substitute for the Service by a third party… or compete with Bitquery's business". It also bars "sell, resell, license, rent or sub-license material of Bitquery" and "develop any third-party applications that interact with the Service without our prior written consent". "Disclosure and open publication of data… is permitted solely… under a paid plan." No attribution clause was found, and no explicit caching clause. — https://bitquery.io/terms-of-service
- **Reading:** showing "this address is Binance deposit" in a paid bot is probably fine on a paid plan. Storing the labels in your own DB and serving them through your own API looks like resale. **Get a written commercial/OEM licence.** An Enterprise bulk-export contract is the natural route.
- **Integration:** GraphQL, ~120 ms (vendor), 100 addresses per query. A bulk poll into the existing `labels` Postgres table fits the current architecture (`internal/store/migrations/postgres/001_labels.sql`).
- **Startup fit:** best. Self-serve, cheap, and TRON-heavy.

### MistTrack (SlowMist)
- **What:** endpoints `address_labels`, `address_overview`, `risk_score` (KYT/KYA, sync and async), `transactions_investigation`, `address_action`, `address_trace`, `address_counterparty`. — https://docs.misttrack.io/llms.txt
  - Labels return `label_list` (e.g. "Binance", "hot") and `label_type` ∈ {exchange, defi, mixer, nft, ""}. — https://docs.misttrack.io/api-endpoints/get-address-labels.md
  - Risk score is 3–100 with detail_list/risk_detail (entity, risk type, exposure type, hop count, volume, %). P95 under 500 ms. Claims "500M+ labeled addresses". — https://misttrack.io/solutions/crypto-risk-scoring-api.html
  - The docs index claims "500K Threat Intelligence addresses, and over 90M addresses… tied to malicious activities" and "over 300 million addresses" from trading platforms. — https://docs.misttrack.io/
- **TRON:** TRX, USDT-TRC20, USDC-TRC20, USDD-TRC20. ETH, BSC and 20+ other chains are also covered. — https://docs.misttrack.io/openapi/overview.md
- **Price (public):** — https://misttrack.io/pricing.html
  - Basic $229/mo (no API)
  - Standard $689/mo: OpenAPI with 7 endpoints, 1 call/s, 10k calls/day
  - Compliance $2,069/mo. The pricing page lists the same API limits as Standard; the docs say 5 rps / 50k per day, which conflicts. Verify.
  - Developer: "from $20/year", usage tiers $20–$30,000/yr for 100–1,000,000 calls, 10 rps, valid 1 year. That works out to about $0.20/call at the smallest tier and $0.03/call at 1M. Intermediate breakpoints were not visible.
  - Enterprise: unlimited, contact
  - Trial: $10 for 48 h of Standard. "Payments made are non-refundable".
  - x402 pay-as-you-go (USDC on Base): labels $0.10, overview $0.50, risk_score $1.00, counterparty $0.50. — https://docs.misttrack.io/openapi/x402-pay-as-you-go-pricing.md
- **Terms:** the public Terms of Use have no API-redistribution, caching, attribution or non-compete clause. They are written for personal reference and screenshots. — https://misttrack.io/terms-of-use.html. **Silence ≠ permission. Get written terms.** MistTrack also runs its own "Telegram AML Bot" (listed as a plan feature), so it may treat you as a competitor.
- **Integration:** REST with `api_key` as a query parameter, HTTP 429 on limits, Python SDK and "Agent Skills" (https://github.com/slowmist/misttrack-skills). 1 rps on Standard is too low for batch screening; Developer's 10 rps is better.
- **Startup fit:** good. Self-serve, cheap entry, strongest TRON focus among the vendors that publish prices.

### Arkham Intelligence
- **What:** entity attribution, a 0–100 risk score, counterparties. Claims 3.1B addresses labelled and 94% of on-chain value attributed. — https://arkm.com/api
- **TRON:** Tron is listed on the AWS Marketplace product page. — https://aws.amazon.com/marketplace/pp/prodview-dwjpcmkqtnnty. The arkm.com page names 16 chains without an exhaustive list.
- **Price:** usage credits per endpoint. Credit prices are not visible in the public docs (https://arkm.com/api/docs). The AWS listing is a $0 free tier. Counterparty endpoints are limited to 1 rps; batch accepts up to 1,000 addresses. Access is by application form (https://codex.arkm.com/arkham-api).
- **Terms (decisive):** — https://arkm.com/api-terms-of-service
  - "shall not… disclose, release, distribute, or deliver the Services… to any third party without Company's prior written consent"
  - may not "publish, enhance, or display any compilation or directory based upon information provided through or derived from the Services"
  - may not use it "for purposes of developing or enhancing any product or service that competes with the Services"
  - client represents it is not "affiliated with… any competing crypto analytics or blockchain intelligence company"
  - credits "expire at the end of each billing period"
- **Verdict:** incompatible with a customer-facing screening product unless Arkham grants a written exception, and this product arguably competes with them.

### Nansen
- Pro $49/mo (annual) or $69/mo, with a 2,000-credit monthly top-up. "Premium labels" cost 500 credits. — https://docs.nansen.ai/getting-started/credits
- Redistribution guide: labels are "completely prohibited" from redistribution, internal use only. — https://docs.nansen.ai/guides/redistribution-guide.md
- TRON coverage not verified. **Excluded.**

### Chainalysis
- **Free Sanctions Screening API:** `GET https://public.chainalysis.com/api/v1/address/{address}` with an `X-API-Key` header, 5,000 requests per 5 min. You get HTTP 403 when rate-limited, and a higher limit means contacting sales. The key is emailed at signup.
  - I found this via search: the developer pages at auth-developers.chainalysis.com returned "page not found" when fetched, so **re-check the docs URL**.
  - Coverage is OFAC SDN and "other major sanctions authorities". The launch blog framed the tool for EVM chains and DeFi. — https://www.chainalysis.com/blog/sanctions-screening-tools/
- **Oracle:** EVM only (Ethereum, Polygon, BSC, Avalanche, Optimism, Arbitrum, Fantom, Celo, Blast, Base). "available for anyone to use and does not require a customer relationship", but Chainalysis "cannot guarantee the accuracy…". **No TRON.** — https://go.chainalysis.com/chainalysis-oracle-docs.html
- **Paid KYT / Address Screening:** API-integrated. Rescreens are not charged ("we do not charge for rescreens"). No public price or chain list. — https://www.chainalysis.com/product/address-screening/
  - Tron is supported for KYT per the DFNS integration docs. — https://docs.dfns.co/integrations/aml-kyt/chainalysis
- **Reported price:** Vendr average $174,708/yr, range $25,742–$297,338/yr, plus implementation fees of $5k–25k+. — https://www.vendr.com/marketplace/chainalysis (a buyer-data aggregator, not Chainalysis)
- **AUP:** no "sublicense, sell, lease (including on a service bureau basis), share, distribute… or make it available to anyone other than Licensee". No bulk export of Chainalysis data. No product "substantially similar to… any Chainalysis product". — https://www.chainalysis.com/acceptable-use-policy/
- **Verdict:** too expensive, and the default terms forbid what you need. Only viable through a negotiated OEM/partner deal.

### TRM Labs
- **Free sanctions API:** `POST /public/v1/sanctions/screening` returns `{address, isSanctioned}`. Limits are 1 rps / 100 per day without a key and 1,000 rps / 100k per day with one. Chains are not specified. The docs mention a "Proprietary API" licence, but no terms were shown. — https://docs.sanctions.trmlabs.com/
- **Paid Wallet Screening:** 184+ chains, <400 ms, attribution to real-world entities, 155+ risk configurations. Pricing is demo-only. — https://www.trmlabs.com/products/wallet-screening
- **Chainabuse (TRM):** a free key gives 10 calls/month. Partner access is 5,000 calls/hour, P50 75 ms, via chainabuse.com/partner-contact. — https://docs.chainabuse.com/docs/getting-started-2-1

### Elliptic
- **Lens:** 60+ chains and 250+ bridges, explainable risk scores, API at developers.elliptic.co. No public pricing. — https://www.elliptic.co/platform/lens
- **TRON:** Wallet in Telegram integrated Lens on 2025-11-17, covering BTC, ETH, **Tron (TRC-20)**, TON and stablecoins, with "entity identification across 70+ categories". — https://www.elliptic.co/media-center/elliptic-powers-compliance-for-wallets-100m-users-on-telegram
- **Partner program:** has an "Integration Partners" track for custodians, wallets and "KYC/AML or regtech businesses" to "enhance their offering with Elliptic's pre- and post-transaction risk scoring". Resale terms are not published. — https://www.elliptic.co/company/partner-program
- Of the three majors, this is the one most likely to have a contract shape that allows embedding. Price is unknown; the Vendr aggregator reports it "within 10–20%" of Chainalysis.

### AMLBot
- The official site describes a consumer bot plus KYT API for businesses. — https://amlbot.com/crypto-checker
- Plans are Lite/Pro/Pro+. "Pricing for Pro is Available on Request", Pro+ likewise, and Pro/Pro+ are for "verified businesses only". — https://blog.amlbot.com/amlbot-plans-explained/
- The Lite price ($9 for 20 checks) came from a search summary and is **reported**.
- It competes head-on with a Telegram USDT-check bot. Beware the many clone/impostor domains.

### Scorechain
- API/SDK, "1B+ entities labeled", 2,700+ VASP entities, 25+ chains. — https://www.scorechain.com/developers/api
- TRON TRC10/TRC20 coverage per the vendor's Tron AML API page (https://www.scorechain.com/resources/crypto-glossary/tron-aml-api; content taken from a search snippet).
- The pricing page has principles only and says to book a demo. — https://www.scorechain.com/resources/crypto-glossary/scorechain-pricing

### Crystal, Merkle Science, Breadcrumbs (lower confidence)
- **Crystal:** crystalintelligence.com returned 403 to my fetches. TRON support is announced in a vendor product update (search result). Pricing is not public. **Unverified.**
- **Merkle Science:** vendor blog says TRON and USDT were added. Pricing is on request, and there is no free trial (**reported**, Techjockey). **Unverified.**
- **Breadcrumbs:** a Risk API exists (scores 1–4 plus indirect risk; https://breadcrumbscomply.readme.io/reference/riskaddress per search). TRON is a premium chain. Compliance plan "from $999/mo" is **reported**, because the pricing page returned 403.

### Bulk label datasets (as opposed to per-call scores)
- **Bitquery Enterprise:** S3 bulk exports. The self-serve labels add-on can be polled incrementally (append-only), which gets you close to bulk.
- **Chainalysis, TRM, Elliptic, Arkham and Nansen** all forbid bulk export or redistribution by default.
- No other credible vendor I found publicly sells a TRON label dump.

---

## 3. Recommendation

**Start with Bitquery Address Labels. Add MistTrack Developer as a second opinion for risk scoring on flagged or unlabelled addresses.**

1. **Bitquery (primary, labels):** its TRON label set is the largest I found (71.6M), it covers the exact categories your engine is missing (exchange deposit/hot wallets, USDT-frozen, gambling, payment processors, mixers, scams), it is self-serve and cheap, and it can be synced into your own `labels` table. That matches your architecture of "own tracing engine, missing labels". Treat its labels as lower confidence (e.g. 0.7–0.8) until you have spot-checked them against your PoR and curated sets.
   - **Cost:** $79 Pro + $79.20 labels ≈ **$160/mo** annual, or Scale + labels ≈ **$320/mo**.
2. **MistTrack (secondary, TRON-native scoring):** use the Developer plan (prepaid call tiers, 10 rps) or x402 at $0.10 per labels call, only for addresses your engine cannot attribute. MistTrack is heavily used in the TRON/USDT world, and its label_type, risk detail and hop data are a useful validation set for your scores.
   - **Cost:** usage-based. At 0.2–0.03 $/call, 1k–5k calls/month is roughly **$50–$500/mo** (intermediate tiers not visible). The Standard plan at $689/mo buys 10k calls/day at 1 rps.

**Expected spend: ≈ $200–$900/month** for both. That is two orders of magnitude below Chainalysis, TRM or Elliptic (reported $25k–$300k/yr).

**Do not use:** Arkham and Nansen (their terms prohibit showing labels to third parties). Chainalysis KYT at startup scale (price, and the AUP's service-bureau ban). AMLBot (a direct competitor). Revisit Elliptic's Integration Partner track once revenue can support an enterprise contract. It is the most relevant precedent (Wallet in Telegram, TRC-20).

### Add regardless (free, official)
- **Already in place:** OFAC/UN/EU lists and the Tether USDT blacklist events. Keep them; they are authoritative.
- **Add:** the TRM free sanctions API (keyed: 100k/day) and the Chainalysis free sanctions API (5,000 per 5 min). Use them as cross-checks against your SDN parse (catch parser gaps like D20), not as primary sources. First confirm each returns TRON SDN addresses, and read the key's terms.
- **Chainabuse:** request partner access, read the terms, and record them in `sources.yaml` (it is currently `needs_review`).

### Questions to put to the vendor before signing (in writing)
1. May we **display your labels/entity names and categories to our paying end users** in a Telegram bot, a Mini App and our own REST API? Is attribution ("Powered by X") required?
2. May we **store/cache labels indefinitely** in our database and serve them from there? Do we have to delete them if we cancel?
3. Is our product (a paid wallet-risk screening API/bot) considered a **"competing product" or "substitute"** under your ToS? Please waive that clause for our use case.
4. Can we **combine** your labels with our own scores and show only a derived risk band, without the raw label? Does that change the licence?
5. Exact **TRON coverage**: how many TRON/USDT-TRC20 entities, what share of exchange deposit addresses, how often data is refreshed, and what the false-positive rate or correction process is. Do you provide per-label provenance/timestamps?
6. For Bitquery: price and format of **Enterprise bulk export** limited to labels (TRON + ETH + BSC). Does the self-serve add-on permit commercial display?
7. For MistTrack: the **intermediate Developer tiers** between 100 and 1M calls. The Compliance-plan API limits (pricing page says 1 rps/10k, docs say 5 rps/50k). Is there an SLA or uptime guarantee? Refund terms, since payments are non-refundable?
8. Rate limits, latency SLA, batch endpoints, and sandbox access for testing before paying.
9. Liability: does the contract include any **warranty or indemnity** for mislabels that harm a customer? Which jurisdiction's law governs it?
10. Can we run a **trial on a sample of ~1,000 of our own addresses** that have known ground truth, to measure the accuracy lift before committing?

---

### Sources opened
- https://bitquery.io/products/address-labels-api
- https://bitquery.io/pricing
- https://bitquery.io/terms-of-service
- https://docs.misttrack.io/
- https://docs.misttrack.io/llms.txt
- https://docs.misttrack.io/openapi/overview.md
- https://docs.misttrack.io/openapi/x402-pay-as-you-go-pricing.md
- https://docs.misttrack.io/api-endpoints/get-address-labels.md
- https://misttrack.io/pricing.html
- https://misttrack.io/solutions/crypto-risk-scoring-api.html
- https://misttrack.io/terms-of-use.html
- https://arkm.com/api
- https://arkm.com/api/docs
- https://arkm.com/api-terms-of-service
- https://codex.arkm.com/arkham-api
- https://aws.amazon.com/marketplace/pp/prodview-dwjpcmkqtnnty
- https://docs.nansen.ai/getting-started/credits
- https://docs.nansen.ai/guides/redistribution-guide.md
- https://go.chainalysis.com/chainalysis-oracle-docs.html
- https://www.chainalysis.com/blog/sanctions-screening-tools/
- https://www.chainalysis.com/product/address-screening/
- https://www.chainalysis.com/acceptable-use-policy/
- https://docs.dfns.co/integrations/aml-kyt/chainalysis
- https://www.vendr.com/marketplace/chainalysis (reported figures)
- https://docs.sanctions.trmlabs.com/
- https://www.trmlabs.com/products/wallet-screening
- https://docs.chainabuse.com/docs/getting-started-2-1
- https://www.elliptic.co/platform/lens
- https://www.elliptic.co/company/partner-program
- https://www.elliptic.co/media-center/elliptic-powers-compliance-for-wallets-100m-users-on-telegram
- https://amlbot.com/crypto-checker
- https://blog.amlbot.com/amlbot-plans-explained/
- https://www.scorechain.com/developers/api
- https://www.scorechain.com/resources/crypto-glossary/scorechain-pricing

**Search results only (not opened):** the Chainalysis sanctions API endpoint and rate limit (the developer pages 404'd on fetch), Crystal, Merkle Science, Breadcrumbs, Tronscan security API, and the AMLBot Lite price.
