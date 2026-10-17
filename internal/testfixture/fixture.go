// Package testfixture writes a small SYNTHETIC connector extract and config for tests.
// All names, ids and amounts are made up.
package testfixture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const AsOf = "2026-02-01"

// Synthetic connector account ids.
const (
	Checking  = "acc-checking"
	Savings   = "acc-savings"
	Card      = "acc-card"
	Wallet    = "acc-wallet"
	Brokerage = "acc-brokerage"
	IRA       = "acc-ira"
)

func write(t testing.TB, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimLeft(body, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

const cashHeader = "id,account_id,date,amount,direction,currency,name,merchant_name,description,pending,category\n"

func cat(primary, detailed string) string {
	if primary == "" {
		return ""
	}
	return `"{""primary"":""` + primary + `"",""detailed"":""` + detailed + `""}"`
}

// Extract writes the raw folder (named by AsOf) under root and returns its path.
func Extract(t testing.TB, root string) string {
	dir := filepath.Join(root, AsOf)
	write(t, filepath.Join(dir, "connections.json"), `{"connections":[
 {"id":"conn-bank","provider":"plaid","institution_id":"ins_1","institution_name":"Test Bank","status":"ok"},
 {"id":"conn-card","provider":"plaid","institution_id":"ins_2","institution_name":"Test Card","status":"ok"}]}`)
	write(t, filepath.Join(dir, "accounts.json"), `{"accounts":[
 {"id":"acc-checking","connection_id":"conn-bank","class":"cash","mask":"1111","name":"Checking","type":"depository","subtype":"checking","current_balance":"1517.23","available_balance":"1517.23","currency":"USD"},
 {"id":"acc-savings","connection_id":"conn-bank","class":"cash","mask":"2222","name":"Savings","type":"depository","subtype":"savings","current_balance":"600.00","available_balance":"600.00","currency":"USD"},
 {"id":"acc-card","connection_id":"conn-card","class":"liability","mask":"3333","name":"Card","type":"credit","subtype":"credit card","current_balance":"120.60","available_balance":null,"currency":"USD"},
 {"id":"acc-wallet","connection_id":"conn-bank","class":"cash","mask":null,"name":"Wallet","type":"depository","subtype":"other","current_balance":"0","available_balance":"0","currency":"USD"},
 {"id":"acc-brokerage","connection_id":"conn-bank","class":"investment","mask":"4444","name":"Brokerage","type":"investment","subtype":"ira","current_balance":"1100.00","available_balance":null,"currency":"USD"},
 {"id":"acc-ira","connection_id":"conn-bank","class":"investment","mask":"5555","name":"IRA","type":"investment","subtype":"ira","current_balance":"2000.00","available_balance":null,"currency":"USD"}]}`)
	// page 1 and page 2 overlap: t-dup appears twice (identical), t-pend appears pending then posted
	write(t, filepath.Join(dir, "cash_txns_page001.csv"), cashHeader+
		"t-pay,acc-checking,2026-01-02,2000.0000,in,USD,ACME PAYROLL,,,false,"+cat("INCOME", "INCOME_SALARY")+"\n"+
		"t-xo,acc-checking,2026-01-03,-100.0000,out,USD,XFER TO SAV ***2222,,,false,"+cat("TRANSFER_OUT", "TRANSFER_OUT_SAVINGS")+"\n"+
		"t-xi,acc-savings,2026-01-03,100.0000,in,USD,XFER FROM DDA ***1111,,,false,"+cat("TRANSFER_IN", "TRANSFER_IN_ACCOUNT_TRANSFER")+"\n"+
		"t-cp,acc-checking,2026-01-05,-50.0000,out,USD,CARD CO EPAYMENT,,,false,"+cat("LOAN_PAYMENTS", "LOAN_PAYMENTS_CREDIT_CARD_PAYMENT")+"\n"+
		"t-dup,acc-checking,2026-01-09,-7.7700,out,USD,MYSTERY THING,,,false,\n"+
		"t-pend,acc-checking,2026-01-10,-0.0100,out,USD,PENDING CHANGE,,,true,\n")
	write(t, filepath.Join(dir, "cash_txns_page002.csv"), cashHeader+
		"t-dup,acc-checking,2026-01-09,-7.7700,out,USD,MYSTERY THING,,,false,\n"+
		"t-pend,acc-checking,2026-01-10,-0.0100,out,USD,PENDING CHANGE,,,false,\n"+
		"t-wf,acc-checking,2026-01-08,-25.0000,out,USD,WALLETPAY,,,false,"+cat("TRANSFER_OUT", "TRANSFER_OUT_TRANSFER_OUT_FROM_APPS")+"\n"+
		"t-wp,acc-wallet,2026-01-08,-25.0000,out,USD,Pizza with friend,,,false,"+cat("FOOD_AND_DRINK", "FOOD_AND_DRINK_RESTAURANT")+"\n"+
		"t-un,acc-checking,2026-01-12,-300.0000,out,USD,OUTGOING TRANSFER,,,false,"+cat("TRANSFER_OUT", "TRANSFER_OUT_ACCOUNT_TRANSFER")+"\n"+
		"t-pp,acc-checking,2026-01-20,-9.9900,out,USD,STILL PENDING,,,true,\n"+
		"t-int,acc-savings,2026-01-31,0.0100,in,USD,Interest earned,,,false,"+cat("INCOME", "INCOME_INTEREST_EARNED")+"\n")
	write(t, filepath.Join(dir, "liability_txns_page001.csv"), "id,liability_id,date,amount,direction,currency,name,merchant_name,description,pending,category\n"+
		"t-gro,acc-card,2026-01-04,-170.3000,out,USD,GROCER 42,Grocer,,false,"+cat("FOOD_AND_DRINK", "FOOD_AND_DRINK_GROCERIES")+"\n"+
		"t-c1,acc-card,2026-01-06,-0.1000,out,USD,COFFEE SHOP,,,false,"+cat("FOOD_AND_DRINK", "FOOD_AND_DRINK_COFFEE")+"\n"+
		"t-c2,acc-card,2026-01-06,-0.2000,out,USD,COFFEE SHOP,,,false,"+cat("FOOD_AND_DRINK", "FOOD_AND_DRINK_COFFEE")+"\n"+
		"t-cr,acc-card,2026-01-07,50.0000,in,USD,PAYMENT - THANK YOU,,,false,"+cat("LOAN_PAYMENTS", "LOAN_PAYMENTS_CREDIT_CARD_PAYMENT")+"\n")
	write(t, filepath.Join(dir, "portfolio_txns_page001.csv"), "id,portfolio_id,date,type,subtype,ticker_symbol,security_name,amount,price,quantity,fees,currency\n"+
		"p-div,acc-brokerage,2026-01-15,cash,dividend,FUNDX,Fund X,1.0000,,0,,USD\n")
	write(t, filepath.Join(dir, "holdings.csv"), "id,portfolio_id,ticker_symbol,security_name,security_type,quantity,cost_basis,institution_price,institution_value,currency\n"+
		"h-1,acc-brokerage,FUNDX,Fund X,mutual fund,10.5000000000,1000.004,100.00,1050.00,USD\n"+
		"h-2,acc-brokerage,CUR:USD,US Dollar,cash,50.00,,1,50.00,USD\n")
	return dir
}

// Paths of a written config set.
type Paths struct{ COA, Map, Rules, LocalRules, Transfers, Accruals string }

// Config writes coa/map/rules/transfers YAML under root.
func Config(t testing.TB, root string) Paths {
	p := Paths{
		COA: filepath.Join(root, "coa.yaml"), Map: filepath.Join(root, "accounts.yaml"),
		Rules: filepath.Join(root, "categories.yaml"), LocalRules: filepath.Join(root, "rules.local.yaml"),
		Transfers: filepath.Join(root, "transfers.yaml"), Accruals: filepath.Join(root, "accruals.yaml"),
	}
	write(t, p.COA, `
defaults:
  Assets:      {report_line: Other assets, cash_flow: investing}
  Liabilities: {report_line: Other liabilities, cash_flow: financing}
  Equity:      {report_line: Equity, cash_flow: opening}
  Income:      {report_line: Other income, cash_flow: operating}
  Expenses:    {report_line: Other expenses, cash_flow: operating}
accounts:
  - {name: Assets:Bank, report_line: Cash, cash_flow: cash}
  - {name: Assets:Transfers, report_line: Transfers, cash_flow: unclassified}
  - {name: Liabilities:Card, report_line: Credit cards, cash_flow: operating}
  - {name: Expenses:Food}
`)
	write(t, p.Map, `
books_start: "2025-12-31"
institutions:
  - {name: TestBank, connector_id: conn-bank}
  - {name: TestCard, connector_id: conn-card}
accounts:
  - {connector_id: acc-checking, key: checking, name: "Assets:Bank:TestBank:Checking", institution: TestBank, tracking: connector}
  - {connector_id: acc-savings, key: savings, name: "Assets:Bank:TestBank:Savings", institution: TestBank, tracking: connector}
  - {connector_id: acc-card, key: card, name: "Liabilities:Card:TestCard", institution: TestCard, tracking: connector}
  - {connector_id: acc-wallet, key: wallet, name: "Assets:Bank:Wallet", institution: TestBank, tracking: connector}
  - {connector_id: acc-brokerage, key: brokerage, name: "Assets:Investments:Brokerage", institution: TestBank, tracking: holdings}
  - {connector_id: acc-ira, key: ira, name: "Assets:Investments:IRA", institution: TestBank, tracking: valuation, valuation_only: true}
`)
	write(t, p.Rules, `
rules:
  - {name: payroll, match: '(?i)payroll', direction: in, account: Income:Salary}
  - {name: coffee-generic, match: '(?i)coffee', account: Expenses:Food:Restaurants}
  - {name: interest, match: '(?i)interest earned', account: 'Income:Interest:{institution}'}
plaid_fallback:
  FOOD_AND_DRINK_GROCERIES: Expenses:Food:Groceries
  FOOD_AND_DRINK: Expenses:Food:Restaurants
`)
	write(t, p.LocalRules, `
rules:
  - {name: coffee-local, match: '(?i)coffee shop', account: Expenses:Food:Coffee}
`)
	write(t, p.Transfers, `
hints:
  - {name: mask, match: '(?i)XFER (?:TO|FROM) (?:SAV|DDA) \*+(\d{4})', mask_group: 1}
  - {name: card-payment-bank, match: '(?i)EPAYMENT', to_key: card}
  - {name: card-payment-card, match: '(?i)PAYMENT - THANK YOU', on_kind: liability, to_class: cash}
  - {name: wallet-funding, match: '(?i)^WALLETPAY', to_key: wallet, funding: true}
`)
	// Synthetic accrual specs exercising every kind (amounts are made up).
	write(t, p.Accruals, `
documents:
  - {id: doc-bill, kind: bill, source: manual, doc_date: "2026-01-03", counterparty: Grocer, amount: "170.30", title: Test bill}
accruals:
  - id: pre-books-bill
    kind: payable
    description: Service before books start, paid by card in January
    counterparty: Grocer
    account: Expenses:Food:Groceries
    balance: Liabilities:Payable:Grocer
    service_start: "2025-12-20"
    service_end: "2025-12-20"
    documents: [doc-bill]
    settlements:
      - txn: {account: card, date: "2026-01-04", amount: "-170.30"}
        splits: [{account: Expenses:Fees, amount: "20.30", memo: late fee}]
  - id: in-books-bill
    kind: payable
    account: Expenses:Misc
    balance: Liabilities:Payable:Mystery
    service_end: "2026-01-02"
    dimensions: ['project:Demo']
    settlements:
      - txn: {account: checking, match: MYSTERY}
  - id: annual-premium
    kind: allocate
    description: Annual premium
    account: Expenses:Insurance
    balance: Assets:Prepaid:Insurance
    amount: "1200.00"
    start: "2025-07-01"
    end: "2026-06-30"
    prior_settled: "600.00"
  - id: family-loan-interest
    kind: interest
    description: Interest
    account: Expenses:Interest
    balance: Liabilities:Accrued:Interest
    principal: "1000.00"
    rate_pct: "3.65"
    start: "2026-01-17"
  - id: card-as-loan
    kind: loan
    account: Expenses:Interest:Card
    rate_pct: "12"
    pi_payment: "50.00"
    settlements:
      - txn: {account: card, match: 'PAYMENT - THANK YOU'}
  - id: land
    kind: journal
    description: Land funded directly by lender
    date: "2026-01-15"
    lines:
      - {account: Assets:Land, amount: "500.00"}
      - {account: Liabilities:Loan:Family, amount: "-500.00"}
`)
	return p
}

// NightlyAsOf is the date of the incremental extract written by Nightly.
const NightlyAsOf = "2026-02-10"

// Nightly writes a later, INCREMENTAL extract (only rows from 2026-01-20 on, as a nightly pull
// with a look-back window would return) under root and returns its path. Compared with
// Extract:
//   - t-int re-appears unchanged (overlap with the previous extract);
//   - t-pp (pending on 2026-01-20) is gone and posted under a new id, t-pp2;
//   - t-new is a new posted row;
//   - no liability, portfolio or holdings pages;
//   - accounts.json omits the IRA (e.g. that connection failed tonight);
//   - the checking balance moved by t-pp2 + t-new, so it still ties out.
//
// The pages are saved as verbatim connector tool results (JSON with a "csv" field) to cover
// the JSON page reader.
func Nightly(t testing.TB, root string) string {
	dir := filepath.Join(root, NightlyAsOf)
	write(t, filepath.Join(dir, "connections.json"), `{"connections":[
 {"id":"conn-bank","provider":"plaid","institution_id":"ins_1","institution_name":"Test Bank","status":"ok"},
 {"id":"conn-card","provider":"plaid","institution_id":"ins_2","institution_name":"Test Card","status":"ok"}]}`)
	write(t, filepath.Join(dir, "accounts.json"), `{"accounts":[
 {"id":"acc-checking","connection_id":"conn-bank","class":"cash","mask":"1111","name":"Checking","type":"depository","subtype":"checking","current_balance":"1467.24","available_balance":"1467.24","currency":"USD"},
 {"id":"acc-savings","connection_id":"conn-bank","class":"cash","mask":"2222","name":"Savings","type":"depository","subtype":"savings","current_balance":"600.00","available_balance":"600.00","currency":"USD"},
 {"id":"acc-card","connection_id":"conn-card","class":"liability","mask":"3333","name":"Card","type":"credit","subtype":"credit card","current_balance":"120.60","available_balance":null,"currency":"USD"},
 {"id":"acc-wallet","connection_id":"conn-bank","class":"cash","mask":null,"name":"Wallet","type":"depository","subtype":"other","current_balance":"0","available_balance":"0","currency":"USD"},
 {"id":"acc-brokerage","connection_id":"conn-bank","class":"investment","mask":"4444","name":"Brokerage","type":"investment","subtype":"ira","current_balance":"1100.00","available_balance":null,"currency":"USD"}]}`)
	csvText := cashHeader +
		"t-new,acc-checking,2026-02-05,-40.0000,out,USD,HARDWARE STORE,,,false,\n" +
		"t-int,acc-savings,2026-01-31,0.0100,in,USD,Interest earned,,,false," + cat("INCOME", "INCOME_INTEREST_EARNED") + "\n" +
		"t-pp2,acc-checking,2026-01-20,-9.9900,out,USD,STILL PENDING,,,false,\n"
	b, err := json.Marshal(map[string]any{"format": "csv", "csv": csvText, "row_count": 3, "next_cursor": nil})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "cash_txns_page001.json"), string(b))
	return dir
}
