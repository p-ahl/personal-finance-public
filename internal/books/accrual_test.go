package books_test

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

func buildAccrual(t *testing.T) *books.Result {
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
	if err := cfg.LoadAccruals(p.Accruals); err != nil {
		t.Fatal(err)
	}
	res, err := books.Build(books.Input{AsOf: ex.AsOf, Accounts: ex.Accounts, Txns: ex.Txns, PortfolioTxns: ex.PortfolioTxns, Holdings: ex.Holdings}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func entryByXID(t *testing.T, res *books.Result, xid string) books.Entry {
	t.Helper()
	for _, e := range res.Entries {
		if e.ExternalID == xid {
			return e
		}
	}
	t.Fatalf("no entry %s", xid)
	return books.Entry{}
}

func TestAccrualsBalanceAndConnectorStillTies(t *testing.T) {
	res := buildAccrual(t)
	for _, e := range res.Entries {
		var s int64
		for _, l := range e.Lines {
			s += l.Amount
		}
		if s != 0 {
			t.Fatalf("entry %s unbalanced by %d", e.ExternalID, s)
		}
	}
	b := balances(res)
	for _, a := range res.Assertions {
		if b[a.Account] != a.Amount {
			t.Errorf("%s: ledger %d != connector %d", a.Account, b[a.Account], a.Amount)
		}
	}
	// payables are cleared by their settlements
	if b["Liabilities:Payable:Grocer"] != 0 || b["Liabilities:Payable:Mystery"] != 0 {
		t.Fatalf("payables not cleared: %d %d", b["Liabilities:Payable:Grocer"], b["Liabilities:Payable:Mystery"])
	}
	// split carved out of the settlement, remainder settles the payable
	if b["Expenses:Fees"] != 2030 {
		t.Fatalf("fee split %d", b["Expenses:Fees"])
	}
	// pre-books service is recognized in the opening, not in January expenses
	op := entryByXID(t, res, "accrual:pre-books-bill")
	if op.Type != "opening" || op.Date != "2025-12-31" || b["Expenses:Food:Groceries"] != 0 {
		t.Fatalf("pre-books payable: %+v groceries=%d", op, b["Expenses:Food:Groceries"])
	}
	in := entryByXID(t, res, "accrual:in-books-bill")
	if in.Date != "2026-01-02" || b["Expenses:Misc"] != 777 {
		t.Fatalf("in-books payable %+v misc=%d", in, b["Expenses:Misc"])
	}
	// dimensions tag the recognized line only, not the payable balance
	for _, l := range in.Lines {
		tagged := len(l.Dimensions) == 1 && l.Dimensions[0] == "project:Demo"
		if (l.Account == "Expenses:Misc") != tagged {
			t.Fatalf("dimension on %s: %v", l.Account, l.Dimensions)
		}
	}
}

func TestAllocateInterestLoanJournal(t *testing.T) {
	res := buildAccrual(t)
	b := balances(res)
	// 1200 over 12 months: Jul-Nov (500) before books, prior payment 600 -> opening prepaid 100;
	// Dec + Jan recognized (200); Feb is after as-of. Ends accrued (-100).
	if got := entryByXID(t, res, "accrual:annual-premium:opening"); got.Lines[1].Amount != 10000 {
		t.Fatalf("opening prepaid %+v", got)
	}
	if b["Expenses:Insurance"] != 20000 || b["Assets:Prepaid:Insurance"] != -10000 {
		t.Fatalf("allocate: expense %d prepaid %d", b["Expenses:Insurance"], b["Assets:Prepaid:Insurance"])
	}
	// interest: 15 days of January, actual/365: 1000 * 3.65% * 15/365 = 1.50
	if b["Expenses:Interest"] != 150 || b["Liabilities:Accrued:Interest"] != -150 {
		t.Fatalf("interest %d", b["Expenses:Interest"])
	}
	// loan: owed 120.60 after a 50.00 payment at 1%/month -> before = 170.60/1.01, interest 1.69
	if b["Expenses:Interest:Card"] != 169 {
		t.Fatalf("loan interest %d", b["Expenses:Interest:Card"])
	}
	if b["Assets:Land"] != 50000 || b["Liabilities:Loan:Family"] != -50000 {
		t.Fatal("journal not booked")
	}
	if len(res.Documents) != 1 || len(res.EntryDocs) == 0 {
		t.Fatalf("documents %d links %d", len(res.Documents), len(res.EntryDocs))
	}
}

func TestAccrualSelectorMustBeUnique(t *testing.T) {
	ac := &books.AccrualConfig{Accruals: []books.AccrualSpec{{ID: "x", Kind: "payable", Account: "Expenses:A", Balance: "Liabilities:P",
		Settlements: []books.SettlementSpec{{Txn: books.TxnSelector{Account: "card", Match: "COFFEE"}}}, ServiceEnd: "2026-01-01"}}}
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
	cfg.Accruals = ac
	if err := books.CompileAccruals(ac); err != nil {
		t.Fatal(err)
	}
	if _, err := books.Build(books.Input{AsOf: ex.AsOf, Accounts: ex.Accounts, Txns: ex.Txns}, cfg); err == nil {
		t.Fatal("ambiguous selector (two coffee rows) should fail")
	}
}

func TestAccrualsExampleCompiles(t *testing.T) {
	var c books.AccrualConfig
	b, err := os.ReadFile("../../config/accruals.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if err := books.CompileAccruals(&c); err != nil {
		t.Fatal(err)
	}
	if len(c.Accruals) != 7 || len(c.Documents) != 2 {
		t.Fatalf("example: %d accruals, %d documents", len(c.Accruals), len(c.Documents))
	}
}

func TestOptionalSettlementMayMatchNothing(t *testing.T) {
	mk := func(optional bool) error {
		ac := &books.AccrualConfig{Accruals: []books.AccrualSpec{{ID: "contract", Kind: "payable", Account: "Expenses:A", Balance: "Liabilities:P",
			Amount: "100.00", Date: "2026-01-02",
			Settlements: []books.SettlementSpec{
				{Txn: books.TxnSelector{Account: "checking", From: "2026-01-09", Match: "MYSTERY", All: true, Optional: optional}},
				{Txn: books.TxnSelector{Account: "checking", From: "2026-01-25", Match: "NOT YET PAID", All: true, Optional: optional}},
			}}}}
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
		cfg.Accruals = ac
		if err := books.CompileAccruals(ac); err != nil {
			t.Fatal(err)
		}
		res, err := books.Build(books.Input{AsOf: ex.AsOf, Accounts: ex.Accounts, Txns: ex.Txns}, cfg)
		if err != nil {
			return err
		}
		var open int64
		for _, e := range res.Entries {
			for _, l := range e.Lines {
				if l.Account == "Liabilities:P" {
					open += l.Amount
				}
			}
		}
		if open != -10000+777 { // 100.00 accrued, the 7.77 row settled, the future installment not yet
			t.Fatalf("open payable %d", open)
		}
		return nil
	}
	if err := mk(true); err != nil {
		t.Fatalf("optional selector with no match should build: %v", err)
	}
	if err := mk(false); err == nil {
		t.Fatal("required selector with no match should fail")
	}
}

// A loan with lender-held escrow: payments from escrow_from carry escrow = payment - P&I (booked to
// the escrow account, principal = P&I - interest), and escrow disbursements on the loan account are
// moved off the loan into escrow. The connector account still ties to its balance.
func TestLoanEscrowSplit(t *testing.T) {
	root := t.TempDir()
	ex, err := connector.Read(testfixture.Extract(t, root))
	if err != nil {
		t.Fatal(err)
	}
	p := testfixture.Config(t, root)
	raw, err := os.ReadFile(p.Accruals)
	if err != nil {
		t.Fatal(err)
	}
	var ac books.AccrualConfig
	if err := yaml.Unmarshal(raw, &ac); err != nil {
		t.Fatal(err)
	}
	for i := range ac.Accruals {
		if ac.Accruals[i].ID == "card-as-loan" {
			a := &ac.Accruals[i]
			a.PIPayment, a.Escrow, a.EscrowFrom = "40.00", "Assets:Escrow", "2026-01-01"
			a.EscrowDisbursements = []books.TxnSelector{{Account: "card", Match: "COFFEE SHOP", All: true}}
		}
	}
	if err := books.CompileAccruals(&ac); err != nil {
		t.Fatal(err)
	}
	cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, p.LocalRules, p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Accruals = &ac
	res, err := books.Build(books.Input{AsOf: ex.AsOf, Accounts: ex.Accounts, Txns: ex.Txns, PortfolioTxns: ex.PortfolioTxns, Holdings: ex.Holdings}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := balances(res)
	// owed 120.60 after the payment: before = (120.60 + 40) / 1.01 -> interest 1.59; escrow 50 - 40 = 10.00;
	// coffee 0.10 + 0.20 paid from escrow -> escrow 9.70
	if b["Expenses:Interest:Card"] != 159 {
		t.Fatalf("interest %d", b["Expenses:Interest:Card"])
	}
	if b["Assets:Escrow"] != 970 {
		t.Fatalf("escrow %d", b["Assets:Escrow"])
	}
	pay := entryFor(t, res, "t-cr")
	var loan int64
	for _, l := range pay.Lines {
		if l.Account == "Liabilities:Card:TestCard" {
			loan += l.Amount
		}
	}
	if loan != 3841 { // 50.00 - 1.59 interest - 10.00 escrow
		t.Fatalf("principal %d", loan)
	}
	for _, a := range res.Assertions {
		if b[a.Account] != a.Amount {
			t.Errorf("%s: ledger %d != connector %d", a.Account, b[a.Account], a.Amount)
		}
	}
	// escrow without an escrow account is rejected
	bad := books.AccrualConfig{Accruals: []books.AccrualSpec{{ID: "l", Kind: "loan", Account: "Expenses:I", RatePct: "1", PIPayment: "1", EscrowFrom: "2026-01-01"}}}
	if err := books.CompileAccruals(&bad); err == nil {
		t.Fatal("escrow_from without escrow accepted")
	}
}
