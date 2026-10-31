package scoring

import (
	"sort"

	"github.com/mozer/tether-risk/internal/config"
	"github.com/shopspring/decimal"
)

// Counterparties' own contacts with listed addresses (docs/DECISIONS.md D48).
//
// Traversal stops at a service and, through anything else, dilutes what a
// listed address sent by its share of the intermediary's volume. Both are
// right for the score: money that meets in a pool cannot be followed
// through it. But they hide a fact a reader wants to see: that the wallet
// which paid the screened address itself took millions from a sanctioned
// or frozen one. This lists that fact beside the score, never in it.

// Contact is one direct counterparty of the screened address and its own
// direct flows with listed addresses.
type Contact struct {
	Address  string
	Entity   string
	Category string // the counterparty's own label; "" when it has none

	// Inbound: the counterparty paid the screened address, and its risky
	// flows are what it received from listed addresses. Outbound: the
	// screened address paid it, and its risky flows are what it paid them.
	Inbound bool

	// FlowUSD is what passed between the screened address and it.
	FlowUSD decimal.Decimal

	// TotalUSD is the counterparty's whole volume on the side its risky
	// flows are on: everything it received, or everything it sent. Partial
	// says its own history is not stored, so TotalUSD and the flows are
	// only what other fetched addresses revealed, and SharePct is zero.
	TotalUSD decimal.Decimal
	Partial  bool

	// RiskyUSD is the sum over Risks; SharePct is its share of TotalUSD, in
	// percent.
	RiskyUSD decimal.Decimal
	SharePct decimal.Decimal

	// Risks are the flows by the listed addresses' category, largest first.
	Risks []ContactRisk
}

// ContactRisk is a counterparty's flow with one category of listed
// addresses.
type ContactRisk struct {
	Category  string
	USD       decimal.Decimal
	Transfers uint64
	Addresses int // distinct listed addresses
	// Entity and Address name the listed address with the most flow.
	Entity  string
	Address string
}

// ContactCounterparty is a direct counterparty as the screen found it.
type ContactCounterparty struct {
	Address, Entity, Category string
	Inbound                   bool
	FlowUSD                   decimal.Decimal
	TotalUSD                  decimal.Decimal
	Fetched                   bool
}

// ContactFlow is one aggregated edge between a counterparty and a listed
// address, in the counterparty's risky direction: Inbound says the
// counterparty received it, else it paid it.
type ContactFlow struct {
	Counterparty string
	Inbound      bool
	Listed       string
	USD          decimal.Decimal
	Transfers    uint64
}

// ListedAddress is what a listed address resolved to.
type ListedAddress struct {
	Category string
	Entity   string
}

// JudgeContacts builds the contact list: counterparties whose flows with
// listed addresses reach rules.MinUSD, largest first, at most
// rules.MaxListed of them. It is pure, so the rules are tested without a
// database.
func JudgeContacts(cps []ContactCounterparty, flows []ContactFlow, listed map[string]ListedAddress,
	rules config.CounterpartyContact) []Contact {

	type byCat struct {
		usd       decimal.Decimal
		transfers uint64
		addrs     map[string]decimal.Decimal
	}
	type side struct {
		address string
		inbound bool
	}
	perCP := map[side]map[string]*byCat{}
	for _, f := range flows {
		l, ok := listed[f.Listed]
		if !ok || f.USD.Sign() <= 0 {
			continue
		}
		k := side{f.Counterparty, f.Inbound}
		cats := perCP[k]
		if cats == nil {
			cats = map[string]*byCat{}
			perCP[k] = cats
		}
		c := cats[l.Category]
		if c == nil {
			c = &byCat{usd: decimal.Zero, addrs: map[string]decimal.Decimal{}}
			cats[l.Category] = c
		}
		c.usd = c.usd.Add(f.USD)
		c.transfers += f.Transfers
		c.addrs[f.Listed] = c.addrs[f.Listed].Add(f.USD)
	}

	minUSD := decimal.NewFromFloat(rules.MinUSD)
	var out []Contact
	for _, cp := range cps {
		cats := perCP[side{cp.Address, cp.Inbound}]
		if len(cats) == 0 {
			continue
		}
		c := Contact{
			Address: cp.Address, Entity: cp.Entity, Category: cp.Category, Inbound: cp.Inbound,
			FlowUSD: cp.FlowUSD, TotalUSD: cp.TotalUSD, Partial: !cp.Fetched,
			RiskyUSD: decimal.Zero, SharePct: decimal.Zero,
		}
		for cat, bc := range cats {
			r := ContactRisk{Category: cat, USD: bc.usd, Transfers: bc.transfers, Addresses: len(bc.addrs)}
			for a, usd := range bc.addrs {
				if r.Address == "" || usd.GreaterThan(bc.addrs[r.Address]) || (usd.Equal(bc.addrs[r.Address]) && a < r.Address) {
					r.Address = a
				}
			}
			r.Entity = listed[r.Address].Entity
			c.Risks = append(c.Risks, r)
			c.RiskyUSD = c.RiskyUSD.Add(bc.usd)
		}
		if c.RiskyUSD.LessThan(minUSD) {
			continue
		}
		sort.Slice(c.Risks, func(i, j int) bool {
			if !c.Risks[i].USD.Equal(c.Risks[j].USD) {
				return c.Risks[i].USD.GreaterThan(c.Risks[j].USD)
			}
			return c.Risks[i].Category < c.Risks[j].Category
		})
		if cp.Fetched && cp.TotalUSD.IsPositive() {
			c.SharePct = c.RiskyUSD.Mul(decimal.NewFromInt(100)).Div(cp.TotalUSD)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].RiskyUSD.Equal(out[j].RiskyUSD) {
			return out[i].RiskyUSD.GreaterThan(out[j].RiskyUSD)
		}
		return out[i].Address < out[j].Address
	})
	if rules.MaxListed > 0 && len(out) > rules.MaxListed {
		out = out[:rules.MaxListed]
	}
	return out
}
