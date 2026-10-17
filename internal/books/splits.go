package books

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/p-ahl/personal-finance/internal/money"
)

// Item-level splits (local splits.local.yaml, data-side; never committed).
//
// A split replaces the single categorized counter-leg of one connector transaction with several
// lines, e.g. an online-store charge itemized from its order email. The card/bank leg is not
// touched, so connector balances still tie. Line amounts use the ledger's sign convention for
// the counter-leg (debit-positive: a purchase is positive, a refund negative) and must sum to it
// exactly, to the cent. Splits apply only to rows that end up categorized (not transfers); a split
// that matches no categorized row is an error, so stale ids surface instead of silently dropping.

// ItemSplitLine is one replacement posting.
type ItemSplitLine struct {
	Account    string   `yaml:"account"`
	Amount     string   `yaml:"amount"` // decimal, debit-positive
	Memo       string   `yaml:"memo"`   // e.g. the item name
	Dimensions []string `yaml:"dimensions"`
	minor      int64
}

// ItemSplit itemizes one connector transaction.
type ItemSplit struct {
	Txn    string          `yaml:"txn"`    // connector transaction id (a "plaid:" prefix is accepted)
	Date   string          `yaml:"date"`   // optional sanity check against the transaction date
	Source string          `yaml:"source"` // short provenance tag, recorded as category_source split:<source>
	Note   string          `yaml:"note"`   // free text (order number, evidence); not posted
	Lines  []ItemSplitLine `yaml:"lines"`
	total  int64
}

type ItemSplitConfig struct {
	Splits []ItemSplit `yaml:"splits"`
	byTxn  map[string]*ItemSplit
}

// LoadSplits reads the optional local item-level splits (missing file = no splits).
func (c *Config) LoadSplits(path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	sc := &ItemSplitConfig{}
	if err := loadYAML(path, sc); err != nil {
		return err
	}
	if err := sc.compile(c); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	c.Splits = sc
	return nil
}

// CompileSplits validates splits built in code (LoadSplits does this for YAML).
func (c *Config) CompileSplits(sc *ItemSplitConfig) error {
	if err := sc.compile(c); err != nil {
		return err
	}
	c.Splits = sc
	return nil
}

func (sc *ItemSplitConfig) compile(c *Config) error {
	sc.byTxn = map[string]*ItemSplit{}
	for i := range sc.Splits {
		s := &sc.Splits[i]
		s.Txn = strings.TrimPrefix(s.Txn, "plaid:")
		if s.Txn == "" {
			return fmt.Errorf("split #%d: txn required", i+1)
		}
		if _, dup := sc.byTxn[s.Txn]; dup {
			return fmt.Errorf("split %s: duplicate txn", s.Txn)
		}
		if len(s.Lines) == 0 {
			return fmt.Errorf("split %s: at least one line required", s.Txn)
		}
		if s.Source == "" {
			s.Source = "local"
		}
		s.total = 0
		for j := range s.Lines {
			l := &s.Lines[j]
			if l.Account == "" {
				return fmt.Errorf("split %s line %d: account required", s.Txn, j+1)
			}
			if _, err := c.Resolve(l.Account); err != nil {
				return fmt.Errorf("split %s line %d: %w", s.Txn, j+1, err)
			}
			v, err := money.ParseMinor(l.Amount, 2)
			if err != nil {
				return fmt.Errorf("split %s line %d: %w", s.Txn, j+1, err)
			}
			l.minor = v
			s.total += v
			for _, d := range l.Dimensions {
				if k, v, ok := strings.Cut(d, ":"); !ok || k == "" || v == "" {
					return fmt.Errorf("split %s line %d: dimension %q must be kind:code", s.Txn, j+1, d)
				}
			}
		}
		sc.byTxn[s.Txn] = s
	}
	return nil
}

// splitLines returns the replacement counter-legs for a categorized row, or nil when the row has
// no split. base carries the categorized line (category source, counterparty, rule dimensions).
func splitLines(cfg *Config, r *row, base Line, used map[string]bool) ([]Line, error) {
	if cfg.Splits == nil {
		return nil, nil
	}
	s, ok := cfg.Splits.byTxn[r.t.ID]
	if !ok {
		return nil, nil
	}
	if s.Date != "" && s.Date != r.t.Date {
		return nil, fmt.Errorf("split %s: date %s != transaction date %s", s.Txn, s.Date, r.t.Date)
	}
	if s.total != base.Amount {
		return nil, fmt.Errorf("split %s: lines sum to %s, transaction counter-leg is %s",
			s.Txn, money.Format(s.total, 2), money.Format(base.Amount, 2))
	}
	used[s.Txn] = true
	out := make([]Line, 0, len(s.Lines))
	for _, sl := range s.Lines {
		l := base
		l.Account, l.Amount, l.Memo = sl.Account, sl.minor, sl.Memo
		l.CategorySource = "split:" + s.Source
		l.Dimensions = mergeDims(applyTags(cfg, r, sl.Account, base.Dimensions), sl.Dimensions)
		out = append(out, l)
	}
	return out, nil
}

func mergeDims(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	out := append([]string(nil), a...)
	seen := map[string]bool{}
	for _, d := range out {
		seen[d] = true
	}
	for _, d := range b {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// unusedSplits reports splits that matched no categorized row.
func unusedSplits(cfg *Config, used map[string]bool) error {
	if cfg.Splits == nil {
		return nil
	}
	var miss []string
	for id := range cfg.Splits.byTxn {
		if !used[id] {
			miss = append(miss, id)
		}
	}
	if len(miss) == 0 {
		return nil
	}
	sort.Strings(miss)
	return fmt.Errorf("splits: %d split(s) match no categorized transaction (unknown id, pending, or matched as a transfer): %s",
		len(miss), strings.Join(miss, ", "))
}
