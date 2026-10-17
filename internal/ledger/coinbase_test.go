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

func coinbaseSetup(t *testing.T) (*sql.DB, *books.Config, string) {
	t.Helper()
	root := t.TempDir()
	dir := testfixture.Extract(t, root)
	if err := os.WriteFile(filepath.Join(dir, "cash_txns_page009.csv"), []byte(
		"id,account_id,date,amount,direction,currency,name,merchant_name,description,pending,category\n"+
			"cb-dep,acc-checking,2026-01-07,-200.0000,out,USD,COINBASE INC,,,false,\n"+
			"cb-wd,acc-checking,2026-01-26,60.0000,in,USD,COINBASE INC,,,false,\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := testfixture.Config(t, root)
	appendFile(t, p.COA, "  - {name: Assets:Crypto, report_line: Crypto, cash_flow: investing}\n"+
		"  - {name: Assets:Crypto:Exchange:Coinbase, key: coinbase, tracking: holdings}\n"+
		"  - {name: Assets:Crypto:SelfCustody, key: crypto_selfcustody, tracking: derived}\n")
	appendFile(t, p.Transfers, "  - {name: coinbase, match: '(?i)coinbase', on_kind: cash, to_key: coinbase}\n")
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
	return db, cfg, testfixture.CoinbasePull(t, root)
}

func TestCoinbaseImportBooksAndReconciles(t *testing.T) {
	db, cfg, pull := coinbaseSetup(t)
	const cb = "Assets:Crypto:Exchange:Coinbase"
	if got := sumWhere(t, db, "account = ?", cb); got != 14000 {
		t.Fatalf("net contributions before import = %d", got)
	}
	s, err := ledger.ImportCoinbase(db, cfg, pull)
	if err != nil {
		t.Fatal(err)
	}
	if s.Exchange.LedgerNew != 10 || s.Exchange.Trades != 1 || s.Exchange.Balances != 4 || s.Exchange.Ticks != 3 {
		t.Fatalf("staging: %+v", s.Exchange)
	}
	kinds := map[string]int{}
	for _, p := range s.Result.Pairs {
		if p.Hint == "coinbase" {
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
		{"coinbase total = 0.001 BTC at the mid 100,000", "account = ?", []any{cb}, 10000},
		{"coinbase USD cash", "account = ? AND commodity IS NULL", []any{cb}, 0},
		{"opening: 5 USDC at par", "external_id = 'opening:coinbase' AND account = ?", []any{cb}, 500},
		{"realized: 0.001 BTC cost 98 sold for 60", "account = ?", []any{books.RealizedST}, 3800},
		{"fees: 4.00 commission", "account = 'Expenses:Fees:Investment'", nil, 400},
		{"self-custody: 0.05 SOL at cost 5", "account = 'Assets:Crypto:SelfCustody'", nil, 500},
		{"mark to market", "external_id = 'valuation:coinbase:2026-02-01' AND account = ?", []any{cb}, 200},
		{"in transit nets to zero", "account = 'Assets:Transfers:InTransit'", nil, 0},
		{"nothing unmatched", "account = 'Assets:Transfers:Unmatched' AND category_source LIKE '%coinbase%'", nil, 0},
	}
	for _, c := range checks {
		if got := sumWhere(t, db, c.where, c.args...); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
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
	s2, err := ledger.ImportCoinbase(db, cfg, pull)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Sync.Inserted != 0 || s2.Sync.Updated != 0 || s2.Sync.Deleted != 0 || s2.Exchange.LedgerNew != 0 || s2.Exchange.LedgerChanged != 0 {
		t.Fatalf("second import not idempotent: %+v %+v", s2.Sync, s2.Exchange)
	}
	s3, err := ledger.Rebuild(db, cfg, "categorize")
	if err != nil {
		t.Fatal(err)
	}
	if s3.Sync.Inserted != 0 || s3.Sync.Updated != 0 || s3.Sync.Deleted != 0 {
		t.Fatalf("rebuild changed the journal: %+v", s3.Sync)
	}
}

func TestCoinbaseEventRemovedFromEvidenceIsUnstaged(t *testing.T) {
	db, cfg, pull := coinbaseSetup(t)
	if _, err := ledger.ImportCoinbase(db, cfg, pull); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(pull, "events.csv"))
	lines := strings.Split(string(b), "\n")
	var kept []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "p1,") {
			kept = append(kept, l)
		}
	}
	if err := os.WriteFile(filepath.Join(pull, "events.csv"), []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ledger.ImportCoinbase(db, cfg, pull)
	if err != nil {
		t.Fatal(err)
	}
	if s.Exchange.LedgerChanged != 1 {
		t.Fatalf("want the removed row counted, got %+v", s.Exchange)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM raw_exchange_ledger WHERE exchange='coinbase' AND ledger_id='ev:p1'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ev:p1 still staged: %d %v", n, err)
	}
}

func TestCoinbasePullRejectsEditedFiles(t *testing.T) {
	db, cfg, pull := coinbaseSetup(t)
	if err := os.WriteFile(filepath.Join(pull, "SHA256SUMS"), []byte("0000  events.csv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ImportCoinbase(db, cfg, pull); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("want checksum error, got %v", err)
	}
}
