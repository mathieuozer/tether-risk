// Package billing holds the bot's subscription plans, who is entitled to
// what, and how payments turn into access (docs/DECISIONS.md D26).
//
// It knows nothing about Telegram or TronGrid. The bot drives it; the store
// persists it; the decisions — which plan applies, whether a trial is due,
// which amount identifies an invoice — are plain functions tested without a
// database.
package billing

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"
)

// StarsMaxSubscription is the Bot API's ceiling on a Stars subscription
// price (createInvoiceLink, subscription_period).
const StarsMaxSubscription = 10000

// StarsPeriod is the only subscription period the Bot API accepts: 30 days.
const StarsPeriod = 30 * 24 * time.Hour

// USDTDecimals is TRC-20 USDT's precision: amounts are stored in micro-USDT.
const USDTDecimals = 6

// Plan is one subscription tier.
type Plan struct {
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	DailyScreens int    `yaml:"daily_screens"`
	Details      bool   `yaml:"details"`
	PDF          bool   `yaml:"pdf"`
	Watches      int    `yaml:"watches"`
	Batch        int    `yaml:"batch"`
	API          bool   `yaml:"api"`
	PriceStars   int    `yaml:"price_stars"`
	PriceUSDT    string `yaml:"price_usdt"`

	// Rank orders plans: a higher rank is a better plan. Set from the
	// position in billing.yaml.
	Rank int `yaml:"-"`
	// PriceMicroUSDT is PriceUSDT in micro-USDT.
	PriceMicroUSDT int64 `yaml:"-"`
}

// Config is billing.yaml.
type Config struct {
	Version int    `yaml:"version"`
	Plans   []Plan `yaml:"plans"`
	Trial   struct {
		Days int    `yaml:"days"`
		Plan string `yaml:"plan"`
	} `yaml:"trial"`
	USDT struct {
		InvoiceTTL time.Duration `yaml:"invoice_ttl"`
		Grace      time.Duration `yaml:"grace"`
		Poll       time.Duration `yaml:"poll"`
	} `yaml:"usdt"`
	PeriodDays int `yaml:"period_days"`
	Monitor    struct {
		Interval time.Duration `yaml:"interval"`
		PerPass  int           `yaml:"per_pass"`
	} `yaml:"monitor"`

	byID map[string]*Plan
}

// Load reads and validates dir/billing.yaml.
func Load(dir string) (*Config, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "billing.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read billing.yaml: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse billing.yaml: %w", err)
	}
	if err := c.init(); err != nil {
		return nil, fmt.Errorf("invalid billing.yaml: %w", err)
	}
	return &c, nil
}

func (c *Config) init() error {
	if len(c.Plans) == 0 {
		return fmt.Errorf("no plans")
	}
	c.byID = map[string]*Plan{}
	for i := range c.Plans {
		p := &c.Plans[i]
		p.Rank = i + 1
		if p.ID == "" || p.Name == "" {
			return fmt.Errorf("plan %d: id and name are required", i+1)
		}
		if _, dup := c.byID[p.ID]; dup {
			return fmt.Errorf("duplicate plan %q", p.ID)
		}
		if p.DailyScreens < 1 {
			return fmt.Errorf("plan %s: daily_screens must be at least 1", p.ID)
		}
		if p.Watches < 0 || p.Batch < 0 {
			return fmt.Errorf("plan %s: watches and batch must not be negative", p.ID)
		}
		if p.PriceStars < 1 || p.PriceStars > StarsMaxSubscription {
			return fmt.Errorf("plan %s: price_stars must be 1-%d, got %d", p.ID, StarsMaxSubscription, p.PriceStars)
		}
		usdt, err := decimal.NewFromString(p.PriceUSDT)
		if err != nil || !usdt.IsPositive() {
			return fmt.Errorf("plan %s: price_usdt %q is not a positive amount", p.ID, p.PriceUSDT)
		}
		if !usdt.Equal(usdt.Round(2)) {
			// Invoices add cents to identify a payment; the base price
			// must leave them free.
			return fmt.Errorf("plan %s: price_usdt %q must have at most 2 decimals", p.ID, p.PriceUSDT)
		}
		p.PriceMicroUSDT = usdt.Shift(USDTDecimals).IntPart()
		c.byID[p.ID] = p
	}
	if c.Trial.Days < 0 {
		return fmt.Errorf("trial.days must not be negative")
	}
	if c.Trial.Days > 0 {
		if _, ok := c.byID[c.Trial.Plan]; !ok {
			return fmt.Errorf("trial.plan %q is not a plan", c.Trial.Plan)
		}
	}
	if c.USDT.InvoiceTTL <= 0 || c.USDT.Poll <= 0 || c.USDT.Grace < 0 {
		return fmt.Errorf("usdt: invoice_ttl and poll must be positive, grace not negative")
	}
	if c.PeriodDays < 1 {
		return fmt.Errorf("period_days must be at least 1")
	}
	if c.Monitor.Interval < time.Hour || c.Monitor.PerPass < 1 {
		return fmt.Errorf("monitor: interval must be at least 1h and per_pass at least 1")
	}
	return nil
}

// Plan looks a plan up by id.
func (c *Config) Plan(id string) (*Plan, bool) {
	p, ok := c.byID[id]
	return p, ok
}

// Period is one paid period.
func (c *Config) Period() time.Duration {
	return time.Duration(c.PeriodDays) * 24 * time.Hour
}

// FormatUSDT renders micro-USDT as a decimal amount, e.g. 10370000 -> "10.37".
func FormatUSDT(micro int64) string {
	return decimal.New(micro, -USDTDecimals).StringFixed(2)
}
