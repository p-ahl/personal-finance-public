package ledger_test

import (
	"path/filepath"
	"testing"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/ledger"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

func TestAccrualImportIdempotentWithViews(t *testing.T) {
	root := t.TempDir()
	dir := testfixture.Extract(t, root)
	p := testfixture.Config(t, root)
	cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, p.LocalRules, p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.LoadAccruals(p.Accruals); err != nil {
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
	defer db.Close()
	if _, err := ledger.Import(db, cfg, ex, dir); err != nil {
		t.Fatal(err)
	}
	s2, err := ledger.Import(db, cfg, ex, dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Sync.Inserted != 0 || s2.Sync.Updated != 0 || s2.Sync.Deleted != 0 {
		t.Fatalf("second run not idempotent: %+v", s2.Sync)
	}
	var unbalanced, mismatched, docs, links int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM v_unbalanced_entries),
		(SELECT COUNT(*) FROM v_balance_assertion_check WHERE expected_minor <> actual_minor),
		(SELECT COUNT(*) FROM documents), (SELECT COUNT(*) FROM entry_documents)`).Scan(&unbalanced, &mismatched, &docs, &links); err != nil {
		t.Fatal(err)
	}
	if unbalanced != 0 || mismatched != 0 || docs != 1 || links != 2 {
		t.Fatalf("unbalanced %d mismatched %d docs %d links %d", unbalanced, mismatched, docs, links)
	}
	// open payables: only the accrued interest (payables were settled)
	var key string
	var open int64
	if err := db.QueryRow(`SELECT accrual_key, open_minor FROM v_open_payables`).Scan(&key, &open); err != nil {
		t.Fatal(err)
	}
	if key != "family-loan-interest" || open != 150 {
		t.Fatalf("open payable %s %d", key, open)
	}
	// cash basis: the pre-books grocery bill hits January (payment date); accrual basis: not at all
	var accr, cash int64
	if err := db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN basis='accrual' THEN natural_minor END),0),
		COALESCE(SUM(CASE WHEN basis='cash' THEN natural_minor END),0)
		FROM v_pnl_lines WHERE account = 'Expenses:Food:Groceries' AND month = '2026-01'`).Scan(&accr, &cash); err != nil {
		t.Fatal(err)
	}
	if accr != 0 || cash != 15000 {
		t.Fatalf("groceries accrual %d cash %d", accr, cash)
	}
	// removing the accrual file reverts to cash basis and removes accrual rows
	cfg.Accruals = nil
	s3, err := ledger.Rebuild(db, cfg, "categorize")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM accruals) + (SELECT COUNT(*) FROM journal_entries WHERE source='accrual') + (SELECT COUNT(*) FROM documents)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 || s3.Sync.Deleted == 0 {
		t.Fatalf("accrual rows left: %d (%+v)", n, s3.Sync)
	}
}
