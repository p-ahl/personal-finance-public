package ledger_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/ledger"
	"github.com/p-ahl/personal-finance/internal/report"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

// snapshot is a canonical dump of the posted journal (external id, date, lines) plus account
// balances; two snapshots are equal iff the books are identical.
func snapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT external_id, date, account, amount_minor, COALESCE(quantity,0), COALESCE(commodity,'') FROM v_ledger
		ORDER BY external_id, account, amount_minor, line_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var x, d, a, c string
		var amt, q int64
		if err := rows.Scan(&x, &d, &a, &amt, &q, &c); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s|%s|%s|%d|%d|%s\n", x, d, a, amt, q, c)
	}
	bal, err := db.Query(`SELECT account, SUM(amount_minor) FROM v_ledger GROUP BY account ORDER BY account`)
	if err != nil {
		t.Fatal(err)
	}
	defer bal.Close()
	for bal.Next() {
		var a string
		var s int64
		if err := bal.Scan(&a, &s); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "BAL %s %d\n", a, s)
	}
	return b.String()
}

func copyDir(t *testing.T, src, dst string, edit func(name, body string) string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		body := string(b)
		if edit != nil {
			body = edit(e.Name(), body)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func checkDuplicates(t *testing.T, db *sql.DB) report.DupStats {
	t.Helper()
	_, ds, err := report.Duplicates(db, nil, "Assets:Transfers:InTransit", "Assets:Transfers:Unmatched")
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// Idempotency: re-importing the same extract, an extract with overlapping pages, an older
// extract after a newer one, and an extract in which the bank re-issued a posted row under a new
// id never double counts; legit identical same-day rows (two tickets) are both kept.
func TestReimportIdempotentAndRekeyed(t *testing.T) {
	root := t.TempDir()
	full := testfixture.Extract(t, root)
	p := testfixture.Config(t, root)
	cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, p.LocalRules, p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	db, err := ledger.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	imp := func(dir string) *ledger.Summary {
		t.Helper()
		ex, err := connector.Read(dir)
		if err != nil {
			t.Fatal(err)
		}
		s, err := ledger.Import(db, cfg, ex, dir)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	noop := func(what string, s *ledger.Summary) {
		t.Helper()
		if s.Stage.RawNew != 0 || s.Stage.RawChanged != 0 || s.Stage.Superseded != 0 || s.Sync.Inserted != 0 || s.Sync.Updated != 0 || s.Sync.Deleted != 0 {
			t.Fatalf("%s is not a no-op: stage %+v sync %+v", what, s.Stage, s.Sync)
		}
	}

	imp(full)
	base := snapshot(t, db)

	// 1. the same extract twice
	noop("second import of the same extract", imp(full))
	if snapshot(t, db) != base {
		t.Fatal("books changed on a re-import of the same extract")
	}

	// 2. overlapping re-import: same rows, page 1 repeated as page 3
	over := filepath.Join(t.TempDir(), testfixture.AsOf)
	copyDir(t, full, over, nil)
	b, err := os.ReadFile(filepath.Join(full, "cash_txns_page001.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(over, "cash_txns_page003.csv"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	noop("overlapping re-import", imp(over))
	if snapshot(t, db) != base {
		t.Fatal("books changed on an overlapping re-import")
	}

	// 3. a later extract: the card re-issued t-gro as t-gro-v2 (same fields), and two genuinely
	// identical airline tickets were charged (distinct ids, same date/amount/name)
	later := filepath.Join(root, "2026-02-11")
	copyDir(t, full, later, func(name, body string) string {
		switch name {
		case "liability_txns_page001.csv":
			body = strings.Replace(body, "t-gro,", "t-gro-v2,", 1)
			body += "t-tk1,acc-card,2026-01-25,-100.0000,out,USD,AIRLINE,,,false,\n" +
				"t-tk2,acc-card,2026-01-25,-100.0000,out,USD,AIRLINE,,,false,\n"
		case "accounts.json":
			body = strings.Replace(body, `"current_balance":"120.60"`, `"current_balance":"320.60"`, 1)
		}
		return body
	})
	s3 := imp(later)
	if s3.Stage.Superseded != 1 || s3.Stage.RawNew != 3 {
		t.Fatalf("re-keyed import: %+v", s3.Stage)
	}
	if len(s3.OpeningDrift) != 0 {
		t.Fatalf("a re-keyed row must not move the opening balance: %+v", s3.OpeningDrift)
	}
	var gro int64
	var tickets int
	if err := db.QueryRow(`SELECT COALESCE(SUM(amount_minor),0) FROM v_ledger WHERE account = 'Expenses:Food:Groceries'`).Scan(&gro); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE external_id IN ('plaid:t-tk1','plaid:t-tk2') AND status = 'posted'`).Scan(&tickets); err != nil {
		t.Fatal(err)
	}
	if gro != 17030 || tickets != 2 {
		t.Fatalf("groceries %d (want 17030: re-issued row booked once), tickets %d (want 2: identical same-day rows kept)", gro, tickets)
	}
	if ds := checkDuplicates(t, db); ds.SameRawMulti != 0 || ds.CrossSource != 0 || ds.Rekeyed != 0 || ds.Superseded != 1 || ds.IdenticalKept != 1 {
		t.Fatalf("duplicate check after the re-keyed import: %+v", ds)
	}
	if _, bad, err := report.Reconcile(db); err != nil || bad != 0 {
		t.Fatalf("reconcile: %d differences (%v)", bad, err)
	}
	after := snapshot(t, db)

	// 4. the later extract again, 5. the OLDER extract again (stale), 6. the later one again
	noop("re-import of the later extract", imp(later))
	s5 := imp(full)
	if s5.Stage.RawNew != 0 || s5.Stage.RawChanged != 0 {
		t.Fatalf("older re-import staged changes: %+v", s5.Stage)
	}
	if got := snapshot(t, db); got != after {
		t.Fatalf("re-importing the older extract changed the books (double count or stale overwrite):\n%s", lineDiff(after, got))
	}
	imp(later)
	if snapshot(t, db) != after {
		t.Fatal("books differ after importing the later extract again")
	}
}

// The duplicate check flags a cross-source pair (a manual entry repeating a bank row) and a raw
// row booked by two entries, but never two distinct bank rows that look alike.
func TestDuplicateCheckFlags(t *testing.T) {
	root := t.TempDir()
	full := testfixture.Extract(t, root)
	p := testfixture.Config(t, root)
	cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, p.LocalRules, p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	db, err := ledger.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ex, err := connector.Read(full)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Import(db, cfg, ex, full); err != nil {
		t.Fatal(err)
	}
	if ds := checkDuplicates(t, db); ds != (report.DupStats{}) {
		t.Fatalf("clean books flagged: %+v", ds)
	}
	// a manual entry repeating the 2026-01-04 grocery charge (same account/date/amount/counterparty)
	var groAcct, cardAcct int64
	var cp sql.NullInt64
	if err := db.QueryRow(`SELECT l.account_id, e.counterparty_id FROM journal_lines l JOIN journal_entries e ON e.id = l.entry_id
		JOIN accounts a ON a.id = l.account_id WHERE e.external_id = 'plaid:t-gro' AND a.name = 'Expenses:Food:Groceries'`).Scan(&groAcct, &cp); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id FROM accounts WHERE name LIKE 'Liabilities:%' ORDER BY id LIMIT 1`).Scan(&cardAcct); err != nil {
		t.Fatal(err)
	}
	post := func(xid, rawRef string) {
		t.Helper()
		r, err := db.Exec(`INSERT INTO journal_entries(entry_date, description, counterparty_id, source, external_id) VALUES ('2026-01-04','GROCER 42 (manual)',?,'manual',?)`, cp, xid)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		if _, err := db.Exec(`INSERT INTO journal_lines(entry_id, line_no, account_id, amount, raw_ref) VALUES (?,1,?,17030,?),(?,2,?,-17030,NULL)`,
			id, groAcct, sql.NullString{String: rawRef, Valid: rawRef != ""}, id, cardAcct); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE journal_entries SET status = 'posted' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	post("manual:gro-copy", "")
	ds := checkDuplicates(t, db)
	if ds.CrossSource != 1 || ds.SameRawMulti != 0 {
		t.Fatalf("cross-source copy not flagged: %+v", ds)
	}
	// reviewed pairs are not flagged
	tb, ds2, err := report.Duplicates(db, map[string]bool{"manual:gro-copy|plaid:t-gro": true}, "Assets:Transfers:InTransit")
	if err != nil {
		t.Fatal(err)
	}
	if ds2.CrossSource != 0 {
		t.Fatalf("reviewed pair still flagged: %v", tb.Rows)
	}
	post("manual:gro-copy-2", "t-gro")
	if ds := checkDuplicates(t, db); ds.SameRawMulti != 1 {
		t.Fatalf("raw row booked twice not flagged: %+v", ds)
	}
	var out strings.Builder
	n, err := report.Check(db, &out, "text", "Assets:Transfers:Unmatched", "Assets:Transfers:InTransit", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 || !strings.Contains(out.String(), "raw row booked twice") {
		t.Fatalf("check must fail on a raw row booked twice (failures %d)", n)
	}
}

func lineDiff(a, b string) string {
	in := map[string]int{}
	for _, l := range strings.Split(a, "\n") {
		in[l]++
	}
	var out []string
	for _, l := range strings.Split(b, "\n") {
		if in[l] > 0 {
			in[l]--
		} else {
			out = append(out, "+ "+l)
		}
	}
	for l, n := range in {
		for ; n > 0; n-- {
			out = append(out, "- "+l)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}
