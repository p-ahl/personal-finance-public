package ledger

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMigrateIdempotent(t *testing.T) {
	db := testDB(t)
	again, err := Migrate(db)
	if err != nil || len(again) != 0 {
		t.Fatalf("second migrate applied %v err %v", again, err)
	}
}

// The zero-sum trigger rejects posting an unbalanced entry and edits to posted entries.
func TestZeroSumTrigger(t *testing.T) {
	db := testDB(t)
	mustExec(t, db, `INSERT INTO commodities(code,scale,kind) VALUES ('USD',2,'currency')`)
	mustExec(t, db, `INSERT INTO accounts(name,depth,level1,account_type,normal_balance,statement,report_line,cash_flow_class) VALUES
		('Assets:Cash',2,'Assets','asset','debit','balance_sheet','cash','cash'),
		('Expenses:Food',2,'Expenses','expense','debit','income_statement','living','operating')`)

	if _, err := db.Exec(`INSERT INTO journal_entries(entry_date,source,status) VALUES ('2026-01-01','manual','posted')`); err == nil {
		t.Fatal("inserting a posted entry directly should fail")
	}
	mustExec(t, db, `INSERT INTO journal_entries(id,entry_date,source) VALUES (1,'2026-01-01','manual')`)
	mustExec(t, db, `INSERT INTO journal_lines(entry_id,line_no,account_id,amount) VALUES (1,1,1,-1000),(1,2,2,999)`)
	_, err := db.Exec(`UPDATE journal_entries SET status='posted' WHERE id=1`)
	if err == nil || !strings.Contains(err.Error(), "sum to zero") {
		t.Fatalf("unbalanced post should fail, got %v", err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM v_unbalanced_entries`).Scan(&n)
	if n != 1 {
		t.Fatalf("v_unbalanced_entries = %d, want 1", n)
	}
	mustExec(t, db, `UPDATE journal_lines SET amount=1000 WHERE entry_id=1 AND line_no=2`)
	mustExec(t, db, `UPDATE journal_entries SET status='posted' WHERE id=1`)
	if _, err := db.Exec(`UPDATE journal_lines SET amount=5 WHERE entry_id=1 AND line_no=2`); err == nil {
		t.Fatal("editing a posted line should fail")
	}
	if _, err := db.Exec(`INSERT INTO journal_lines(entry_id,line_no,account_id,amount) VALUES (1,3,2,0)`); err == nil {
		t.Fatal("adding a line to a posted entry should fail")
	}
	if _, err := db.Exec(`DELETE FROM journal_entries WHERE id=1`); err == nil {
		t.Fatal("deleting a posted entry should fail")
	}
	db.QueryRow(`SELECT COUNT(*) FROM v_unbalanced_entries`).Scan(&n)
	if n != 0 {
		t.Fatalf("v_unbalanced_entries = %d, want 0", n)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}
