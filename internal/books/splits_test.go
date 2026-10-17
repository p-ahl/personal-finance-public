package books_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

// buildSplits builds the synthetic fixture with an item-level splits file (SYNTHETIC data).
func buildSplits(t *testing.T, splitsYAML string) (*books.Result, error) {
	t.Helper()
	root := t.TempDir()
	ex, err := connector.Read(testfixture.Extract(t, root))
	if err != nil {
		t.Fatal(err)
	}
	p := testfixture.Config(t, root)
	cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, p.LocalRules, p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "splits.local.yaml")
	if splitsYAML != "" {
		if err := os.WriteFile(path, []byte(strings.TrimLeft(splitsYAML, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.LoadSplits(path); err != nil {
		return nil, err
	}
	return books.Build(books.Input{AsOf: ex.AsOf, Accounts: ex.Accounts, Txns: ex.Txns, PortfolioTxns: ex.PortfolioTxns, Holdings: ex.Holdings}, cfg)
}

const groSplit = `
splits:
  - txn: plaid:t-gro
    date: "2026-01-04"
    source: order-email
    note: synthetic order 000-0000000-0000000
    lines:
      - {account: Expenses:Food:Groceries, amount: "150.00", memo: Produce box}
      - {account: Expenses:Housing:Supplies, amount: "12.30", memo: Paper towels}
      - {account: Expenses:Healthcare, amount: "8.00", memo: Bandages, dimensions: ["hsa:eligible"]}
`

func TestSplitsItemizeCounterLeg(t *testing.T) {
	res, err := buildSplits(t, groSplit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Split != 1 {
		t.Fatalf("split rows = %d", res.Stats.Split)
	}
	e := entryByXID(t, res, "plaid:t-gro")
	if len(e.Lines) != 4 {
		t.Fatalf("want card leg + 3 split lines, got %+v", e.Lines)
	}
	var sum int64
	for _, l := range e.Lines {
		sum += l.Amount
	}
	if sum != 0 {
		t.Fatalf("unbalanced by %d", sum)
	}
	if e.Lines[0].RawRef != "t-gro" || e.Lines[0].Amount != -17030 {
		t.Fatalf("card leg changed: %+v", e.Lines[0])
	}
	hc := e.Lines[3]
	if hc.Account != "Expenses:Healthcare" || hc.Amount != 800 || hc.Memo != "Bandages" || hc.CategorySource != "split:order-email" ||
		len(hc.Dimensions) != 1 || hc.Dimensions[0] != "hsa:eligible" {
		t.Fatalf("split line: %+v", hc)
	}
	// connector balances still tie
	b := balances(res)
	for _, a := range res.Assertions {
		if b[a.Account] != a.Amount {
			t.Errorf("%s: ledger %d != connector %d", a.Account, b[a.Account], a.Amount)
		}
	}
	if b["Expenses:Housing:Supplies"] != 1230 {
		t.Fatalf("supplies = %d", b["Expenses:Housing:Supplies"])
	}
}

func TestSplitsMissingFileIsNoop(t *testing.T) {
	res, err := buildSplits(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Split != 0 || len(entryByXID(t, res, "plaid:t-gro").Lines) != 2 {
		t.Fatal("no splits file should leave entries untouched")
	}
}

func TestSplitsRejectBadInput(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"sum mismatch": {`
splits:
  - txn: t-gro
    lines:
      - {account: Expenses:Food:Groceries, amount: "170.29"}
`, "lines sum to 170.29"},
		"unknown txn": {`
splits:
  - txn: t-nope
    lines:
      - {account: Expenses:Misc, amount: "1.00"}
`, "match no categorized transaction"},
		"transfer row": {`
splits:
  - txn: t-xo
    lines:
      - {account: Expenses:Misc, amount: "100.00"}
`, "match no categorized transaction"},
		"date mismatch": {`
splits:
  - txn: t-gro
    date: "2026-01-05"
    lines:
      - {account: Expenses:Food:Groceries, amount: "170.30"}
`, "transaction date"},
		"duplicate": {`
splits:
  - txn: t-gro
    lines: [{account: Expenses:Misc, amount: "170.30"}]
  - txn: plaid:t-gro
    lines: [{account: Expenses:Misc, amount: "170.30"}]
`, "duplicate txn"},
		"bad dimension": {`
splits:
  - txn: t-gro
    lines: [{account: Expenses:Misc, amount: "170.30", dimensions: [hsa]}]
`, "kind:code"},
		"bad account": {`
splits:
  - txn: t-gro
    lines: [{account: Spending:Misc, amount: "170.30"}]
`, "top level"},
		"unknown field": {`
splits:
  - txn: t-gro
    total: "170.30"
    lines: [{account: Expenses:Misc, amount: "170.30"}]
`, "total"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := buildSplits(t, c.yaml)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}
