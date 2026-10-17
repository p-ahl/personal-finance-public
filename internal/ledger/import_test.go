package ledger_test

import (
	"path/filepath"
	"testing"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/ledger"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

func TestImportIdempotentAndReconciles(t *testing.T) {
	root := t.TempDir()
	dir := testfixture.Extract(t, root)
	p := testfixture.Config(t, root)
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
	defer db.Close()

	s1, err := ledger.Import(db, cfg, ex, dir)
	if err != nil {
		t.Fatal(err)
	}
	if s1.Sync.Inserted == 0 || s1.Stage.RawNew != 16 || s1.Stage.Pending != 1 {
		t.Fatalf("first run: %+v %+v", s1.Sync, s1.Stage)
	}
	s2, err := ledger.Import(db, cfg, ex, dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Sync.Inserted != 0 || s2.Sync.Updated != 0 || s2.Sync.Deleted != 0 || s2.Stage.RawNew != 0 || s2.Stage.RawChanged != 0 {
		t.Fatalf("second run not idempotent: %+v %+v", s2.Sync, s2.Stage)
	}

	var unbalanced, mismatched, entries int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM v_unbalanced_entries),
		(SELECT COUNT(*) FROM v_balance_assertion_check WHERE expected_minor <> actual_minor),
		(SELECT COUNT(*) FROM journal_entries)`).Scan(&unbalanced, &mismatched, &entries); err != nil {
		t.Fatal(err)
	}
	if unbalanced != 0 || mismatched != 0 || entries != s1.Sync.Inserted {
		t.Fatalf("unbalanced %d mismatched %d entries %d", unbalanced, mismatched, entries)
	}

	// rule change -> rebuild from staging updates only the affected entries
	cfg2, err := books.LoadConfig(p.COA, p.Map, p.Rules, "", p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	s3, err := ledger.Rebuild(db, cfg2, "categorize")
	if err != nil {
		t.Fatal(err)
	}
	if s3.Sync.Updated != 2 || s3.Sync.Inserted != 0 || s3.Sync.Deleted != 0 {
		t.Fatalf("rebuild after rule change: %+v (want 2 coffee entries updated)", s3.Sync)
	}
	var coffee int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(amount_minor),0) FROM v_ledger WHERE account = 'Expenses:Food:Restaurants' AND description LIKE 'COFFEE%'`).Scan(&coffee); err != nil {
		t.Fatal(err)
	}
	if coffee != 30 {
		t.Fatalf("coffee re-categorized total %d", coffee)
	}
}
