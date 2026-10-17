package ledger_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/ledger"
	"github.com/p-ahl/personal-finance/internal/report"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// krakenSetup: the synthetic connector extract plus two bank rows to/from Kraken, a config
// with the Kraken account and hint, the connector import, then the synthetic Kraken pull.
func krakenSetup(t *testing.T) (*sql.DB, *books.Config, string, string) {
	t.Helper()
	root := t.TempDir()
	dir := testfixture.Extract(t, root)
	if err := os.WriteFile(filepath.Join(dir, "cash_txns_page009.csv"), []byte(
		"id,account_id,date,amount,direction,currency,name,merchant_name,description,pending,category\n"+
			"k-dep,acc-checking,2026-01-07,-200.0000,out,USD,KRAKEN,,,false,\n"+
			"k-wd,acc-checking,2026-01-26,50.0000,in,USD,KRAKEN,,,false,\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := testfixture.Config(t, root)
	appendFile(t, p.COA, "  - {name: Assets:Crypto, report_line: Crypto, cash_flow: investing}\n"+
		"  - {name: Assets:Crypto:Exchange:Kraken, key: kraken, tracking: holdings}\n"+
		"  - {name: Assets:Crypto:SelfCustody, key: crypto_selfcustody, tracking: derived}\n")
	appendFile(t, p.Transfers, "  - {name: kraken, match: '(?i)kraken', on_kind: cash, to_key: kraken}\n")
	cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, p.LocalRules, p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := connector.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := ledger.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := ledger.Import(db, cfg, ex, dir); err != nil {
		t.Fatal(err)
	}
	return db, cfg, testfixture.KrakenPull(t, root), root
}

func sumWhere(t *testing.T, db *sql.DB, where string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(amount_minor),0) FROM v_ledger WHERE `+where, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestKrakenImportBooksAndReconciles(t *testing.T) {
	db, cfg, pull, _ := krakenSetup(t)
	const kr = "Assets:Crypto:Exchange:Kraken"
	// before the Kraken import the bank rows are one-sided to the Kraken account (net contributions)
	if got := sumWhere(t, db, "account = ?", kr); got != 15000 {
		t.Fatalf("net contributions before import = %d", got)
	}
	s, err := ledger.ImportKraken(db, cfg, pull)
	if err != nil {
		t.Fatal(err)
	}
	if s.Exchange.LedgerNew != 13 || s.Exchange.Trades != 2 || s.Exchange.Balances != 4 {
		t.Fatalf("staging: %+v", s.Exchange)
	}
	kinds := map[string]int{}
	for _, p := range s.Result.Pairs {
		if p.Hint == "kraken" {
			kinds[p.Kind]++
		}
	}
	if kinds["in-transit"] != 1 || kinds["same-day"] != 1 {
		t.Fatalf("USD transfer matching: %v", kinds)
	}

	checks := []struct {
		name, where string
		args        []any
		want        int64
	}{
		{"kraken total = USD 29.28 + BTC 199.00 + KAS 36.00", "account = ?", []any{kr}, 26428},
		{"kraken USD cash", "account = ? AND commodity IS NULL", []any{kr}, 2928},
		{"opening: 499 KAS at FIFO cost", "external_id = 'opening:kraken' AND account = ?", []any{kr}, 4940},
		{"staking income 10 KAS x 0.05", "account = ?", []any{books.StakingIncome}, -50},
		{"realized: 509 KAS cost 49.90 sold for 30.00", "account = ?", []any{books.RealizedST}, 1990},
		{"fees: 0.60 USD + 0.00001 BTC at cost + 0.12 USD", "account = 'Expenses:Fees:Investment'", nil, 60 + 50 + 12},
		{"self-custody: 0.001 BTC at cost", "account = 'Assets:Crypto:SelfCustody'", nil, 5000},
		{"pre-books self-custody lot brought back at evidenced cost", "account = ? AND external_id LIKE 'kraken:%'", []any{books.OpeningEquity}, -4950},
		{"mark to market", "external_id = 'valuation:kraken:2026-02-01' AND account = ?", []any{kr}, 8600},
		{"in transit nets to zero", "account = 'Assets:Transfers:InTransit'", nil, 0},
		{"nothing unmatched", "account = 'Assets:Transfers:Unmatched' AND category_source LIKE '%kraken%'", nil, 0},
	}
	for _, c := range checks {
		if got := sumWhere(t, db, c.where, c.args...); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
	var q int64
	if err := db.QueryRow(`SELECT SUM(quantity) FROM v_ledger WHERE account = 'Assets:Crypto:SelfCustody' AND commodity = 'BTC'`).Scan(&q); err != nil || q != 10000000 {
		t.Fatalf("self-custody BTC quantity %d %v", q, err)
	}
	// the bank row on 2026-01-07 now clears through in-transit, linked to the Kraken deposit
	var links int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT link_id) FROM journal_entries WHERE link_id LIKE 'xfer:k-dep+kraken:%'`).Scan(&links); err != nil || links != 1 {
		t.Fatalf("in-transit link: %d %v", links, err)
	}
	var unbalanced, mismatched int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM v_unbalanced_entries),
		(SELECT COUNT(*) FROM v_balance_assertion_check WHERE expected_minor <> actual_minor)`).Scan(&unbalanced, &mismatched); err != nil {
		t.Fatal(err)
	}
	if unbalanced != 0 || mismatched != 0 {
		t.Fatalf("unbalanced %d, assertion mismatches %d", unbalanced, mismatched)
	}
	tb, bad, err := report.ReconcileExchange(db)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		var sb strings.Builder
		_ = tb.Render(&sb, "text")
		t.Fatalf("exchange reconciliation failed:\n%s", sb.String())
	}

	// idempotent
	s2, err := ledger.ImportKraken(db, cfg, pull)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Sync.Inserted != 0 || s2.Sync.Updated != 0 || s2.Sync.Deleted != 0 || s2.Exchange.LedgerNew != 0 || s2.Exchange.LedgerChanged != 0 {
		t.Fatalf("second import not idempotent: %+v %+v", s2.Sync, s2.Exchange)
	}
	// a connector rebuild keeps the Kraken entries
	s3, err := ledger.Rebuild(db, cfg, "categorize")
	if err != nil {
		t.Fatal(err)
	}
	if s3.Sync.Inserted != 0 || s3.Sync.Updated != 0 || s3.Sync.Deleted != 0 {
		t.Fatalf("rebuild changed the journal: %+v", s3.Sync)
	}
}

func TestKrakenReconciliationCatchesDifferences(t *testing.T) {
	db, cfg, pull, root := krakenSetup(t)
	if _, err := ledger.ImportKraken(db, cfg, pull); err != nil {
		t.Fatal(err)
	}
	// a later pull reports one KAS more than the ledger history explains
	later := filepath.Join(root, "kraken", "2026-02-02")
	if err := os.MkdirAll(later, 0o700); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(pull)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(pull, e.Name()))
		if e.Name() == "Balance.json" {
			b = []byte(strings.Replace(string(b), `"KAS":"600.00000"`, `"KAS":"601.00000"`, 1))
		}
		if err := os.WriteFile(filepath.Join(later, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ledger.ImportKraken(db, cfg, later); err != nil {
		t.Fatal(err)
	}
	_, bad, err := report.ReconcileExchange(db)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 2 { // KAS quantity and total USD value
		t.Fatalf("want 2 differences, got %d", bad)
	}
}

func TestKrakenPullRejectsEditedFiles(t *testing.T) {
	db, cfg, pull, _ := krakenSetup(t)
	if err := os.WriteFile(filepath.Join(pull, "SHA256SUMS"), []byte("0000  Balance.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ImportKraken(db, cfg, pull); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("want checksum error, got %v", err)
	}
}
