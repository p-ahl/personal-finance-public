package books_test

import (
	"os"
	"strings"
	"testing"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

func build(t *testing.T, withLocal bool) *books.Result {
	t.Helper()
	root := t.TempDir()
	ex, err := connector.Read(testfixture.Extract(t, root))
	if err != nil {
		t.Fatal(err)
	}
	p := testfixture.Config(t, root)
	local := p.LocalRules
	if !withLocal {
		local = ""
	}
	cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, local, p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	res, err := books.Build(books.Input{AsOf: ex.AsOf, Accounts: ex.Accounts, Txns: ex.Txns, PortfolioTxns: ex.PortfolioTxns, Holdings: ex.Holdings}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func balances(res *books.Result) map[string]int64 {
	b := map[string]int64{}
	for _, e := range res.Entries {
		for _, l := range e.Lines {
			b[l.Account] += l.Amount
		}
	}
	return b
}

func entryFor(t *testing.T, res *books.Result, rawID string) books.Entry {
	t.Helper()
	for _, e := range res.Entries {
		for _, l := range e.Lines {
			if l.RawRef == rawID {
				return e
			}
		}
	}
	t.Fatalf("no entry for %s", rawID)
	return books.Entry{}
}

func TestEveryEntryBalancesAndEndsAtConnectorBalance(t *testing.T) {
	res := build(t, true)
	for _, e := range res.Entries {
		var s int64
		for _, l := range e.Lines {
			s += l.Amount
		}
		if s != 0 || len(e.Lines) < 2 {
			t.Fatalf("entry %s unbalanced (%d) or short", e.ExternalID, s)
		}
	}
	b := balances(res)
	if len(res.Assertions) != 6 {
		t.Fatalf("assertions = %d, want one per connector account", len(res.Assertions))
	}
	for _, a := range res.Assertions {
		if b[a.Account] != a.Amount {
			t.Errorf("%s: ledger %d != connector %d", a.Account, b[a.Account], a.Amount)
		}
	}
	// liability: connector 120.60 owed -> ledger -12060 (credit)
	if b["Liabilities:Card:TestCard"] != -12060 {
		t.Fatalf("card balance %d", b["Liabilities:Card:TestCard"])
	}
}

func TestOpeningEntries(t *testing.T) {
	res := build(t, true)
	want := map[string]struct {
		date string
		amt  int64
	}{
		"Assets:Bank:TestBank:Checking": {"2026-01-01", 1},     // 1517.23 - 1517.22 of activity
		"Assets:Bank:TestBank:Savings":  {"2026-01-02", 49999}, // 600.00 - 100.01
		"Liabilities:Card:TestCard":     {"2026-01-03", 0},     // explicit zero opening
		"Assets:Bank:Wallet":            {"2026-01-07", 0},
		"Assets:Investments:IRA":        {"2025-12-31", 200000}, // no activity -> books_start
	}
	got := map[string]books.Opening{}
	for _, o := range res.Openings {
		got[o.Account] = o
	}
	for acct, w := range want {
		o, ok := got[acct]
		if !ok {
			t.Errorf("%s: no opening entry", acct)
			continue
		}
		if o.Date != w.date || o.Amount != w.amt {
			t.Errorf("%s: opening %s %d, want %s %d", acct, o.Date, o.Amount, w.date, w.amt)
		}
	}
	for _, e := range res.Entries {
		if e.Type == "opening" && e.Lines[len(e.Lines)-1].Account != books.OpeningEquity {
			t.Errorf("opening %s not against %s", e.ExternalID, books.OpeningEquity)
		}
	}
}

func TestSignHandling(t *testing.T) {
	res := build(t, true)
	e := entryFor(t, res, "t-gro") // card charge -170.30
	if e.Lines[0].Account != "Liabilities:Card:TestCard" || e.Lines[0].Amount != -17030 {
		t.Fatalf("card leg: %+v", e.Lines[0])
	}
	if e.Lines[1].Account != "Expenses:Food:Groceries" || e.Lines[1].Amount != 17030 {
		t.Fatalf("expense leg: %+v", e.Lines[1])
	}
	e = entryFor(t, res, "t-pay") // deposit +2000
	if e.Lines[0].Amount != 200000 || e.Lines[1].Account != "Income:Salary" || e.Lines[1].Amount != -200000 {
		t.Fatalf("payroll: %+v", e.Lines)
	}
}

func TestTransferMatching(t *testing.T) {
	res := build(t, true)
	kinds := map[string]int{}
	for _, p := range res.Pairs {
		kinds[p.Kind]++
	}
	if kinds["same-day"] != 1 || kinds["in-transit"] != 1 || kinds["funding"] != 1 {
		t.Fatalf("pairs %v", kinds)
	}
	// same-day transfer: one entry, two legs, no clearing
	e := entryFor(t, res, "t-xo")
	if len(e.Lines) != 2 || e.Lines[1].Account != "Assets:Bank:TestBank:Savings" || e.Type != "transfer" {
		t.Fatalf("same-day transfer: %+v", e)
	}
	// in-transit: two entries linked through the clearing account, which nets to zero
	a, b := entryFor(t, res, "t-cp"), entryFor(t, res, "t-cr")
	if a.LinkID == "" || a.LinkID != b.LinkID || a.Lines[1].Account != "Assets:Transfers:InTransit" {
		t.Fatalf("in-transit: %+v / %+v", a, b)
	}
	if bal := balances(res)["Assets:Transfers:InTransit"]; bal != 0 {
		t.Fatalf("in-transit clearing balance %d", bal)
	}
	// funding: bank pays the wallet, the wallet payment itself is still categorized
	f := entryFor(t, res, "t-wf")
	if f.Lines[1].Account != "Assets:Bank:Wallet" {
		t.Fatalf("funding: %+v", f)
	}
	w := entryFor(t, res, "t-wp")
	if w.Lines[1].Account != "Expenses:Food:Restaurants" {
		t.Fatalf("wallet payment category: %+v", w)
	}
	// unmatched transfer-like row
	u := entryFor(t, res, "t-un")
	if u.Lines[1].Account != "Assets:Transfers:Unmatched" {
		t.Fatalf("unmatched: %+v", u)
	}
}

func TestCategorizationOrderAndFallback(t *testing.T) {
	res := build(t, true)
	e := entryFor(t, res, "t-c1")
	if e.Lines[1].Account != "Expenses:Food:Coffee" || !strings.HasPrefix(e.Lines[1].CategorySource, "rule:local:") {
		t.Fatalf("local overlay should win: %+v", e.Lines[1])
	}
	if e := entryFor(t, res, "t-gro"); e.Lines[1].CategorySource != "plaid:FOOD_AND_DRINK_GROCERIES" {
		t.Fatalf("plaid fallback: %+v", e.Lines[1])
	}
	if e := entryFor(t, res, "t-dup"); e.Lines[1].Account != "Expenses:Uncategorized" {
		t.Fatalf("uncategorized: %+v", e.Lines[1])
	}
	if e := entryFor(t, res, "t-int"); e.Lines[1].Account != "Income:Interest:TestBank" {
		t.Fatalf("{institution} template: %+v", e.Lines[1])
	}
	res = build(t, false)
	if e := entryFor(t, res, "t-c1"); e.Lines[1].Account != "Expenses:Food:Restaurants" {
		t.Fatalf("without overlay the repo rule applies: %+v", e.Lines[1])
	}
	if res.Stats.PendingRows != 1 {
		t.Fatalf("pending rows %d, want 1 (kept out of the journal)", res.Stats.PendingRows)
	}
}

func TestDecimalPrecision(t *testing.T) {
	res := build(t, true)
	if got := balances(res)["Expenses:Food:Coffee"]; got != 30 { // 0.10 + 0.20 == 0.30 exactly
		t.Fatalf("coffee = %d cents", got)
	}
}

func TestHoldingsOpeningAndMarkToMarket(t *testing.T) {
	res := build(t, true)
	var qty, cost int64
	var mtm int64
	for _, e := range res.Entries {
		for _, l := range e.Lines {
			if l.Account != "Assets:Investments:Brokerage" {
				continue
			}
			if l.Commodity == "FUNDX" && e.Type == "opening" {
				qty += *l.Quantity
				cost += l.Amount
			}
			if e.Type == "valuation" {
				mtm += l.Amount
			}
		}
	}
	if qty != 10_500000 || cost != 100000 { // cost basis 1000.004 rounds to 1000.00
		t.Fatalf("FUNDX opening qty %d cost %d", qty, cost)
	}
	if mtm != 5000 {
		t.Fatalf("mark to market %d, want 5000", mtm)
	}
	if b := balances(res)["Assets:Investments:Brokerage"]; b != 110000 {
		t.Fatalf("brokerage %d", b)
	}
}

func TestRuleOverrideTransfer(t *testing.T) {
	root := t.TempDir()
	ex, err := connector.Read(testfixture.Extract(t, root))
	if err != nil {
		t.Fatal(err)
	}
	p := testfixture.Config(t, root)
	// A bank "XFER FROM DDA" that is really money from a third party: without the flag the
	// transfer hint wins; with override_transfer the rule categorizes it.
	rules := func(flag bool) *books.Result {
		t.Helper()
		f := "false"
		if flag {
			f = "true"
		}
		if err := os.WriteFile(p.LocalRules, []byte("rules:\n  - {name: gift, match: 'XFER FROM DDA', direction: in, account: Income:Gifts, override_transfer: "+f+"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, p.LocalRules, p.Transfers)
		if err != nil {
			t.Fatal(err)
		}
		res, err := books.Build(books.Input{AsOf: ex.AsOf, Accounts: ex.Accounts, Txns: ex.Txns, PortfolioTxns: ex.PortfolioTxns, Holdings: ex.Holdings}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if e := entryFor(t, rules(false), "t-xi"); e.Type != "transfer" {
		t.Fatalf("without override the transfer hint should win: %+v", e)
	}
	res := rules(true)
	e := entryFor(t, res, "t-xi")
	if e.Type == "transfer" || e.Lines[1].Account != "Income:Gifts" {
		t.Fatalf("override_transfer: %+v", e)
	}
	// the other leg lost its partner and is now an unmatched transfer, still balanced
	if o := entryFor(t, res, "t-xo"); o.Lines[1].Account != "Assets:Transfers:Unmatched" {
		t.Fatalf("orphaned leg: %+v", o)
	}
}

func TestTagRulesAddDimensionsOnly(t *testing.T) {
	root := t.TempDir()
	ex, err := connector.Read(testfixture.Extract(t, root))
	if err != nil {
		t.Fatal(err)
	}
	p := testfixture.Config(t, root)
	local := "rules:\n  - {name: coffee-local, match: '(?i)coffee shop', account: Expenses:Food:Coffee}\n" +
		"tags:\n  - {name: trip, account: '^Expenses:Food', from: '2026-01-05', to: '2026-01-06', exclude: 'GROCER', dimensions: ['trip:2026-test']}\n"
	if err := os.WriteFile(p.LocalRules, []byte(local), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, p.LocalRules, p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	res, err := books.Build(books.Input{AsOf: ex.AsOf, Accounts: ex.Accounts, Txns: ex.Txns, PortfolioTxns: ex.PortfolioTxns, Holdings: ex.Holdings}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := entryFor(t, res, "t-c1")
	if c.Lines[1].Account != "Expenses:Food:Coffee" || len(c.Lines[1].Dimensions) != 1 || c.Lines[1].Dimensions[0] != "trip:2026-test" {
		t.Fatalf("coffee in window should be tagged, account unchanged: %+v", c.Lines[1])
	}
	if g := entryFor(t, res, "t-gro"); len(g.Lines[1].Dimensions) != 0 {
		t.Fatalf("out-of-window / excluded row tagged: %+v", g.Lines[1])
	}
	if len(c.Lines[0].Dimensions) != 0 {
		t.Fatalf("source (card) line must not be tagged: %+v", c.Lines[0])
	}
}

func TestBankCashIntoHoldingsAccountKeepsReconciliation(t *testing.T) {
	root := t.TempDir()
	ex, err := connector.Read(testfixture.Extract(t, root))
	if err != nil {
		t.Fatal(err)
	}
	p := testfixture.Config(t, root)
	// a bank debit categorized straight into the holdings-tracked brokerage (e.g. a contribution)
	if err := os.WriteFile(p.LocalRules, []byte("rules:\n  - {name: contrib, match: 'MYSTERY THING', direction: out, account: Assets:Investments:Brokerage}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, p.LocalRules, p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	res, err := books.Build(books.Input{AsOf: ex.AsOf, Accounts: ex.Accounts, Txns: ex.Txns, PortfolioTxns: ex.PortfolioTxns, Holdings: ex.Holdings}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if b := balances(res)["Assets:Investments:Brokerage"]; b != 110000 {
		t.Fatalf("brokerage %d, want connector 110000 (opening must net out the bank-side contribution)", b)
	}
	for _, o := range res.Openings {
		if o.Account == "Assets:Investments:Brokerage" && o.Date != "2026-01-08" {
			t.Fatalf("opening dated %s, want the day before the first bank-side line (2026-01-08)", o.Date)
		}
	}
}
