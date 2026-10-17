// Package books turns staged connector data + YAML config into balanced journal entries.
// Everything here is deterministic: same inputs and rules => same entries.
package books

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/p-ahl/personal-finance/internal/money"
)

// ---------- chart of accounts ----------

type AccountAttrs struct {
	ReportLine  string `yaml:"report_line"`
	CashFlow    string `yaml:"cash_flow"`
	Description string `yaml:"description"`
}

type AccountSpec struct {
	Name             string  `yaml:"name"`
	Key              string  `yaml:"key"` // logical key referenced by transfer hints
	ReportLine       string  `yaml:"report_line"`
	CashFlow         string  `yaml:"cash_flow"`
	Description      string  `yaml:"description"`
	ValuationOnly    bool    `yaml:"valuation_only"`
	Tracking         string  `yaml:"tracking"`           // derived | none (internal accounts)
	AssumeEndBalance *string `yaml:"assume_end_balance"` // back into an opening so the account ends here
}

type COA struct {
	Defaults map[string]AccountAttrs `yaml:"defaults"` // keyed by top-level segment
	Accounts []AccountSpec           `yaml:"accounts"`
}

// ---------- connector account mapping (real file lives outside the repo) ----------

type InstitutionSpec struct {
	ConnectorID string `yaml:"connector_id"`
	Name        string `yaml:"name"` // short name used in account paths and rules (ExampleBank, ...)
	Display     string `yaml:"display"`
}

type MappedAccount struct {
	ConnectorID   string `yaml:"connector_id"`
	Key           string `yaml:"key"`
	Name          string `yaml:"name"`
	Institution   string `yaml:"institution"`
	Tracking      string `yaml:"tracking"` // connector | holdings | valuation
	ValuationOnly bool   `yaml:"valuation_only"`
	ReportLine    string `yaml:"report_line"`
	CashFlow      string `yaml:"cash_flow"`
	Description   string `yaml:"description"`
	// PendingAccount receives the gap between the reported balance and the pinned opening +
	// posted activity (activity the institution counts that has not posted yet, e.g. Venmo
	// payments whose bank funding debit lags). Default: Liabilities:Pending:Unposted (REVIEW).
	PendingAccount string `yaml:"pending_account"`
}

type AccountMap struct {
	BooksStart   string            `yaml:"books_start"` // opening date for accounts with no activity
	Institutions []InstitutionSpec `yaml:"institutions"`
	Accounts     []MappedAccount   `yaml:"accounts"`
}

// ---------- categorization rules ----------

type Rule struct {
	Name         string   `yaml:"name"`
	Match        string   `yaml:"match"`       // regex over "name | merchant | description"
	Plaid        string   `yaml:"plaid"`       // regex over Plaid detailed category
	Direction    string   `yaml:"direction"`   // in | out
	Kind         string   `yaml:"kind"`        // cash | liability
	Institution  string   `yaml:"institution"` // regex over the source account's institution short name
	Account      string   `yaml:"account"`     // may contain {institution}
	Counterparty string   `yaml:"counterparty"`
	Dimensions   []string `yaml:"dimensions"` // kind:code
	AmountMin    string   `yaml:"amount_min"` // inclusive bounds on the absolute amount
	AmountMax    string   `yaml:"amount_max"`
	// OverrideTransfer: a matching row is categorized by this rule even if a transfer hint
	// matches it (e.g. a bank "XFER FROM DDA" that is really a gift from a third party).
	OverrideTransfer bool   `yaml:"override_transfer"`
	Origin           string `yaml:"-"`
	minAbs           *int64
	maxAbs           *int64
	re, plaidRe      *regexp.Regexp
	instRe           *regexp.Regexp
}

type Fallback struct {
	In       string `yaml:"in"`
	Out      string `yaml:"out"`
	Transfer string `yaml:"transfer"`
}

// TagRule adds dimensions (e.g. trip:2026-summer-sweden) to the categorized line of matching
// rows without changing their account. All set conditions must hold.
type TagRule struct {
	Name        string   `yaml:"name"`
	Match       string   `yaml:"match"`       // regex over "name | merchant | description"
	Exclude     string   `yaml:"exclude"`     // regex; matching rows are skipped
	Account     string   `yaml:"account"`     // regex over the categorized account (e.g. ^Expenses:)
	Institution string   `yaml:"institution"` // regex over the source account's institution
	From        string   `yaml:"from"`        // inclusive YYYY-MM-DD
	To          string   `yaml:"to"`          // inclusive YYYY-MM-DD
	Dimensions  []string `yaml:"dimensions"`  // kind:code (required)
	Origin      string   `yaml:"-"`
	re, exRe    *regexp.Regexp
	acctRe      *regexp.Regexp
	instRe      *regexp.Regexp
}

type RuleSet struct {
	Rules         []Rule            `yaml:"rules"`
	Tags          []TagRule         `yaml:"tags"`
	PlaidFallback map[string]string `yaml:"plaid_fallback"`
	Fallback      Fallback          `yaml:"fallback"`
}

// ---------- transfer hints ----------

type Hint struct {
	Name          string `yaml:"name"`
	Match         string `yaml:"match"`
	OnKind        string `yaml:"on_kind"`
	OnInstitution string `yaml:"on_institution"`
	ToKey         string `yaml:"to_key"`
	ToInstitution string `yaml:"to_institution"`
	ToClass       string `yaml:"to_class"` // cash | liability | any
	MaskGroup     int    `yaml:"mask_group"`
	Funding       bool   `yaml:"funding"` // same-sign pair = this row funds a payment made from the target account
	re, onInstRe  *regexp.Regexp
	toInstRe      *regexp.Regexp
}

type TransferConfig struct {
	WindowHinted  int    `yaml:"window_days_hinted"`
	WindowGeneric int    `yaml:"window_days_generic"`
	InTransit     string `yaml:"in_transit_account"`
	Unmatched     string `yaml:"unmatched_account"`
	Hints         []Hint `yaml:"hints"`
}

type Config struct {
	COA       COA
	Map       AccountMap
	Rules     RuleSet
	Transfers TransferConfig
	Accruals  *AccrualConfig   // optional local accruals.yaml
	Splits    *ItemSplitConfig // optional local splits.local.yaml (item-level splits)
}

// LoadAccruals reads the optional local accrual specs (missing file = cash basis only).
func (c *Config) LoadAccruals(path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	ac := &AccrualConfig{}
	if err := loadYAML(path, ac); err != nil {
		return err
	}
	if err := ac.compile(); err != nil {
		return err
	}
	c.Accruals = ac
	return nil
}

// LoadCOAOverlay merges an optional local chart-of-accounts file (same format as config/coa.yaml)
// into the repo chart: its accounts are appended (a name already listed is replaced in place) and
// its defaults override the repo defaults. A missing file is not an error.
func (c *Config) LoadCOAOverlay(path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	var ov COA
	if err := loadYAML(path, &ov); err != nil {
		return err
	}
	for k, v := range ov.Defaults {
		if c.COA.Defaults == nil {
			c.COA.Defaults = map[string]AccountAttrs{}
		}
		c.COA.Defaults[k] = v
	}
	idx := map[string]int{}
	for i, a := range c.COA.Accounts {
		idx[a.Name] = i
	}
	for _, a := range ov.Accounts {
		if i, ok := idx[a.Name]; ok {
			c.COA.Accounts[i] = a
			continue
		}
		idx[a.Name] = len(c.COA.Accounts)
		c.COA.Accounts = append(c.COA.Accounts, a)
	}
	return nil
}

func loadYAML(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// LoadConfig reads repo config (coa, rules, transfers) plus the local, data-side account map and
// optional local rule overlay. Local rules are evaluated before repo rules.
func LoadConfig(coaPath, mapPath, rulesPath, localRulesPath, transfersPath string) (*Config, error) {
	c := &Config{}
	if err := loadYAML(coaPath, &c.COA); err != nil {
		return nil, err
	}
	if err := loadYAML(mapPath, &c.Map); err != nil {
		return nil, err
	}
	if err := loadYAML(rulesPath, &c.Rules); err != nil {
		return nil, err
	}
	for i := range c.Rules.Rules {
		c.Rules.Rules[i].Origin = "repo"
	}
	if localRulesPath != "" {
		if _, err := os.Stat(localRulesPath); err == nil {
			var local RuleSet
			if err := loadYAML(localRulesPath, &local); err != nil {
				return nil, err
			}
			for i := range local.Rules {
				local.Rules[i].Origin = "local"
			}
			c.Rules.Rules = append(local.Rules, c.Rules.Rules...)
			for i := range local.Tags {
				local.Tags[i].Origin = "local"
			}
			c.Rules.Tags = append(c.Rules.Tags, local.Tags...)
		}
	}
	if err := loadYAML(transfersPath, &c.Transfers); err != nil {
		return nil, err
	}
	return c, c.compile()
}

func (c *Config) compile() error {
	for i := range c.Rules.Rules {
		r := &c.Rules.Rules[i]
		var err error
		if r.Account == "" {
			return fmt.Errorf("rule %q: account required", r.Name)
		}
		if r.Match != "" {
			if r.re, err = regexp.Compile(r.Match); err != nil {
				return fmt.Errorf("rule %q: %w", r.Name, err)
			}
		}
		if r.Plaid != "" {
			if r.plaidRe, err = regexp.Compile(r.Plaid); err != nil {
				return fmt.Errorf("rule %q: %w", r.Name, err)
			}
		}
		if r.Institution != "" {
			if r.instRe, err = regexp.Compile(r.Institution); err != nil {
				return fmt.Errorf("rule %q: %w", r.Name, err)
			}
		}
		for _, b := range []struct {
			s   string
			dst **int64
		}{{r.AmountMin, &r.minAbs}, {r.AmountMax, &r.maxAbs}} {
			if b.s == "" {
				continue
			}
			v, err := money.ParseMinor(b.s, 2)
			if err != nil {
				return fmt.Errorf("rule %q: %w", r.Name, err)
			}
			*b.dst = &v
		}
	}
	for i := range c.Rules.Tags {
		t := &c.Rules.Tags[i]
		if len(t.Dimensions) == 0 {
			return fmt.Errorf("tag %q: dimensions required", t.Name)
		}
		for _, d := range t.Dimensions {
			if k, v, ok := strings.Cut(d, ":"); !ok || k == "" || v == "" {
				return fmt.Errorf("tag %q: dimension %q must be kind:code", t.Name, d)
			}
		}
		for _, b := range []struct {
			s   string
			dst **regexp.Regexp
		}{{t.Match, &t.re}, {t.Exclude, &t.exRe}, {t.Account, &t.acctRe}, {t.Institution, &t.instRe}} {
			if b.s == "" {
				continue
			}
			re, err := regexp.Compile(b.s)
			if err != nil {
				return fmt.Errorf("tag %q: %w", t.Name, err)
			}
			*b.dst = re
		}
	}
	for i := range c.Transfers.Hints {
		h := &c.Transfers.Hints[i]
		var err error
		if h.re, err = regexp.Compile(h.Match); err != nil {
			return fmt.Errorf("hint %q: %w", h.Name, err)
		}
		if h.OnInstitution != "" {
			if h.onInstRe, err = regexp.Compile(h.OnInstitution); err != nil {
				return fmt.Errorf("hint %q: %w", h.Name, err)
			}
		}
		if h.ToInstitution != "" {
			if h.toInstRe, err = regexp.Compile(h.ToInstitution); err != nil {
				return fmt.Errorf("hint %q: %w", h.Name, err)
			}
		}
	}
	if c.Transfers.WindowHinted == 0 {
		c.Transfers.WindowHinted = 10
	}
	if c.Transfers.WindowGeneric == 0 {
		c.Transfers.WindowGeneric = 4
	}
	if c.Transfers.InTransit == "" {
		c.Transfers.InTransit = "Assets:Transfers:InTransit"
	}
	if c.Transfers.Unmatched == "" {
		c.Transfers.Unmatched = "Assets:Transfers:Unmatched"
	}
	if c.Rules.Fallback.In == "" {
		c.Rules.Fallback.In = "Income:Uncategorized"
	}
	if c.Rules.Fallback.Out == "" {
		c.Rules.Fallback.Out = "Expenses:Uncategorized"
	}
	if c.Rules.Fallback.Transfer == "" {
		c.Rules.Fallback.Transfer = c.Transfers.Unmatched
	}
	if c.Map.BooksStart == "" {
		c.Map.BooksStart = "2025-10-06"
	}
	return nil
}

// Attrs resolves presentation attributes for an account name by inheritance from the nearest
// configured ancestor (COA entries, then top-level defaults).
type Attrs struct {
	Type, Normal, Statement, ReportLine, CashFlow, Description string
	ValuationOnly                                              bool
	Tracking                                                   string
}

var topTypes = map[string][3]string{
	"Assets":      {"asset", "debit", "balance_sheet"},
	"Liabilities": {"liability", "credit", "balance_sheet"},
	"Equity":      {"equity", "credit", "balance_sheet"},
	"Income":      {"income", "credit", "income_statement"},
	"Expenses":    {"expense", "debit", "income_statement"},
}

func (c *Config) Resolve(name string) (Attrs, error) {
	segs := strings.Split(name, ":")
	tt, ok := topTypes[segs[0]]
	if !ok {
		return Attrs{}, fmt.Errorf("account %q: top level must be Assets|Liabilities|Equity|Income|Expenses", name)
	}
	a := Attrs{Type: tt[0], Normal: tt[1], Statement: tt[2], Tracking: "none"}
	specs := map[string]AccountSpec{}
	for _, s := range c.COA.Accounts {
		specs[s.Name] = s
	}
	for _, m := range c.Map.Accounts {
		specs[m.Name] = AccountSpec{Name: m.Name, ReportLine: m.ReportLine, CashFlow: m.CashFlow, Description: m.Description, ValuationOnly: m.ValuationOnly}
	}
	for i := len(segs); i >= 1; i-- {
		p := strings.Join(segs[:i], ":")
		s, ok := specs[p]
		if !ok {
			continue
		}
		if a.ReportLine == "" {
			a.ReportLine = s.ReportLine
		}
		if a.CashFlow == "" {
			a.CashFlow = s.CashFlow
		}
		if i == len(segs) {
			a.Description = s.Description
			a.ValuationOnly = s.ValuationOnly
			if s.Tracking != "" {
				a.Tracking = s.Tracking
			}
		}
	}
	d := c.COA.Defaults[segs[0]]
	if a.ReportLine == "" {
		a.ReportLine = d.ReportLine
	}
	if a.CashFlow == "" {
		a.CashFlow = d.CashFlow
	}
	if a.ReportLine == "" || a.CashFlow == "" {
		return a, fmt.Errorf("account %q: no report_line/cash_flow (add a default for %s)", name, segs[0])
	}
	return a, nil
}
