package ledger_test

import (
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

// A nightly extract that covers only the last few weeks must add to staging without dropping
// older rows, keep accounts missing from tonight's extract at their last snapshot, keep the
// last holdings pull, and reconcile against the newest balance per account.
func TestIncrementalNightlyImport(t *testing.T) {
	root := t.TempDir()
	full := testfixture.Extract(t, root)
	nightly := testfixture.Nightly(t, root)
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

	ex1, err := connector.Read(full)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Import(db, cfg, ex1, full); err != nil {
		t.Fatal(err)
	}
	plaidIDs := func() map[string]bool {
		rows, err := db.Query(`SELECT external_id FROM journal_entries WHERE external_id LIKE 'plaid:%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		m := map[string]bool{}
		for rows.Next() {
			var s string
			rows.Scan(&s)
			m[s] = true
		}
		return m
	}
	before := plaidIDs()

	ex2, err := connector.Read(nightly)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex2.Txns) != 3 || len(ex2.Accounts) != 5 || len(ex2.Holdings) != 0 {
		t.Fatalf("nightly extract read: %d txns %d accounts %d holdings", len(ex2.Txns), len(ex2.Accounts), len(ex2.Holdings))
	}
	s2, err := ledger.Import(db, cfg, ex2, nightly)
	if err != nil {
		t.Fatal(err)
	}
	if s2.AsOf != testfixture.NightlyAsOf {
		t.Fatalf("as-of %s", s2.AsOf)
	}
	if s2.Stage.RawNew != 2 || s2.Stage.RawUnchanged != 1 || s2.Stage.RawChanged != 0 {
		t.Fatalf("stage: %+v", s2.Stage)
	}
	if len(s2.OpeningDrift) != 0 {
		t.Fatalf("a consistent nightly must not move opening balances: %+v", s2.OpeningDrift)
	}
	if s2.Stage.UnseenPending != 1 || s2.Stage.UnseenPosted != 0 {
		t.Fatalf("unseen: posted %d pending %d, want 0 / 1 (t-pp superseded by t-pp2)", s2.Stage.UnseenPosted, s2.Stage.UnseenPending)
	}

	var raw int
	if err := db.QueryRow(`SELECT COUNT(*) FROM raw_transactions`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != 18 {
		t.Fatalf("staging should be cumulative: %d raw rows, want 16 + 2", raw)
	}
	after := plaidIDs()
	for id := range before {
		if !after[id] {
			t.Fatalf("entry %s from the first extract was dropped by the incremental import", id)
		}
	}
	if !after["plaid:t-new"] || !after["plaid:t-pp2"] || len(after) != len(before)+2 {
		t.Fatalf("new entries missing: before %d after %d", len(before), len(after))
	}

	// newest snapshot per account: IRA keeps its 2026-02-01 balance, the rest use 2026-02-10
	asOf := map[string]string{}
	rows, err := db.Query(`SELECT a.name, b.as_of FROM balance_assertions b JOIN accounts a ON a.id = b.account_id WHERE b.source = 'connector'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n, d string
		rows.Scan(&n, &d)
		asOf[n] = d
	}
	rows.Close()
	if len(asOf) != 6 || asOf["Assets:Investments:IRA"] != testfixture.AsOf || asOf["Assets:Bank:TestBank:Checking"] != testfixture.NightlyAsOf {
		t.Fatalf("assertion dates: %v", asOf)
	}
	tb, bad, err := report.Reconcile(db)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 0 || len(tb.Rows) != 6 {
		t.Fatalf("reconcile: %d differences over %d accounts: %v", bad, len(tb.Rows), tb.Rows)
	}
	if len(tb.Notes) != 1 || !strings.Contains(tb.Notes[0], "IRA") {
		t.Fatalf("reconcile notes: %v", tb.Notes)
	}
	var mismatched, unbalanced, holdings int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM v_balance_assertion_check WHERE expected_minor <> actual_minor),
		(SELECT COUNT(*) FROM v_unbalanced_entries), (SELECT COUNT(*) FROM v_holdings WHERE account = 'Assets:Investments:Brokerage')`).Scan(&mismatched, &unbalanced, &holdings); err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 || unbalanced != 0 || holdings == 0 {
		t.Fatalf("mismatched %d unbalanced %d brokerage positions %d (holdings must survive a nightly without a holdings pull)", mismatched, unbalanced, holdings)
	}

	// re-running the same nightly is a no-op
	s3, err := ledger.Import(db, cfg, ex2, nightly)
	if err != nil {
		t.Fatal(err)
	}
	if s3.Stage.RawNew != 0 || s3.Sync.Inserted != 0 || s3.Sync.Updated != 0 || s3.Sync.Deleted != 0 {
		t.Fatalf("second nightly run not idempotent: %+v %+v", s3.Stage, s3.Sync)
	}

	// a later extract whose checking balance does not follow from the staged history (a row
	// is missing): the opening is pinned and does not absorb it; the gap goes to the default
	// pending account and is flagged (unconfigured = unexplained)
	later := filepath.Join(root, "2026-02-11")
	if err := os.MkdirAll(later, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"connections.json", "accounts.json", "cash_txns_page001.json"} {
		b, err := os.ReadFile(filepath.Join(nightly, f))
		if err != nil {
			t.Fatal(err)
		}
		if f == "accounts.json" {
			b = []byte(strings.Replace(string(b), `"current_balance":"1467.24"`, `"current_balance":"1400.00"`, 1))
		}
		if err := os.WriteFile(filepath.Join(later, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ex4, err := connector.Read(later)
	if err != nil {
		t.Fatal(err)
	}
	s4, err := ledger.Import(db, cfg, ex4, later)
	if err != nil {
		t.Fatal(err)
	}
	if len(s4.OpeningDrift) != 0 {
		t.Fatalf("pinned openings must not drift: %+v", s4.OpeningDrift)
	}
	if len(s4.Result.Pending) != 1 || s4.Result.Pending[0].Account != "Assets:Bank:TestBank:Checking" || s4.Result.Pending[0].Amount != -6724 ||
		s4.Result.Pending[0].Pending != books.DefaultPendingAcct || s4.Result.Pending[0].Configured {
		t.Fatalf("pending gap: %+v", s4.Result.Pending)
	}
	if got := sumWhere(t, db, "account = ?", books.DefaultPendingAcct); got != 6724 {
		t.Fatalf("default pending account %d", got)
	}
	if _, bad, err := report.Reconcile(db); err != nil || bad != 0 {
		t.Fatalf("reconcile after the gap: %d differences, %v", bad, err)
	}
}
