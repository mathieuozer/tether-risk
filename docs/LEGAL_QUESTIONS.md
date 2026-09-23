# Questions for a lawyer before go-live

These are questions, not answers. Nothing here is legal advice; each item
records why it arose so the lawyer can see the context. The answers depend
first on where the operating company is incorporated and where its owners and
servers are, so that comes first.

## 1. The operating entity

- Where is the company incorporated, and where are its owners and directors
  resident? Every item below turns on this.
- Is a screening service that holds no customer funds, moves no transfers and
  only reports on public blockchain data a regulated activity there? Examples:
  UAE (VARA or the Central Bank), the EU (MiCA), the UK (FCA cryptoasset
  registration), Turkey (SPK/MASAK). We believe it is not a VASP, but that
  needs confirming.

## 2. Customers in Russia and the CIS

The product is sold in Russian and targets Russian-speaking P2P traders.

- EU Regulation 833/2014 (as amended) forbids providing certain crypto-asset
  wallet, account and custody services to Russian nationals and residents, as
  well as some IT and software services. Does an analytics and screening
  subscription fall under any of these, if the company or its people have an
  EU link?
- The UK and US have comparable restrictions: OFAC's ban on certain IT and
  software services to Russia, and the UK's services prohibitions. Do they
  reach us through any UK or US person, bank, cloud provider, or payment
  route?
- Do we need to screen customers by residence? Telegram does not tell us a
  user's country.

## 3. Payments from risky wallets (D43)

A USDT payment from a wallet our screen links to sanctions, Tether-frozen
funds or serious risk is now held and does not activate a plan.

- May we return such a payment, must we keep it, or must we report it (for
  example suspicious-transaction reporting such as goAML in the UAE)?
- Does receiving it expose the company to sanctions liability even though we
  did not ask for it?
- How long should such funds and records be kept?

## 4. Data protection

- We store Telegram user ids, usernames, the addresses each user screens,
  payments and usage, and review usage patterns for misuse (D43). Are these
  personal data under GDPR for EU users, or under UAE PDPL or Turkish KVKK?
- The terms say records are kept "as long as needed" and disclosed "where
  the law requires". What retention period and privacy notice are required?

## 5. Labelling addresses

- Reports name addresses as sanctioned, terrorist financing (from US DOJ and
  FBI filings), scam, address poisoning, or frozen by Tether. What is our
  exposure if a label is wrong: defamation, or harm to a business whose
  wallet we call risky?
- Do we need a process for disputing or removing a label, and a published
  methodology?
- Attribution duties of the open data used: the UK Sanctions List is under
  the Open Government Licence v3.0, which requires the statement "Contains
  public sector information licensed under the Open Government Licence v3.0"
  to appear somewhere in the product. It does not yet.

## 6. Terms, consumers and marketing

- The terms exist in English, Turkish and Russian. Which version governs,
  and which law and courts apply?
- EU and Turkish consumer law: withdrawal rights for digital subscriptions,
  auto-renewal disclosures (Telegram Stars renew every 30 days).
- The product avoids claiming AML compliance ("pre-screening, not a regulated
  AML determination"). Is that disclaimer, and the liability cap at 30 days of
  fees, enforceable where we sell?

## 7. Payment channels

- Telegram Stars and Telegram's payment terms for digital goods.
- Accepting USDT directly: accounting, tax and any licensing needed to hold
  crypto revenue where the company is based.
