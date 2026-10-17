package ledger_test

import (
	"database/sql"
	"encoding/json"
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

// writeExtract writes a synthetic nightly extract: the Nightly fixture's accounts with the given
// checking balance (wallet reports 0) and the given cash rows.
func writeExtract(t *testing.T, root, date, checking, rows string) string {
	t.Helper()
	nightly := filepath.Join(root, testfixture.NightlyAsOf)
	dir := filepath.Join(root, date)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"connections.json", "accounts.json"} {
		b, err := os.ReadFile(filepath.Join(nightly, f))
		if err != nil {
			t.Fatal(err)
		}
		if f == "accounts.json" {
			b = []byte(strings.Replace(string(b), `"current_balance":"1467.24","available_balance":"1467.24"`,
				`"current_balance":"`+checking+`","available_balance":"`+checking+`"`, 1))
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	csv := "id,account_id,date,amount,direction,currency,name,merchant_name,description,pending,category\n" + rows
	b, _ := json.Marshal(map[string]any{"format": "csv", "csv": csv, "row_count": strings.Count(rows, "\n"), "next_cursor": nil})
	if err := os.WriteFile(filepath.Join(dir, "cash_txns_page001.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func importDir(t *testing.T, db *sql.DB, cfg *books.Config, dir string) *ledger.Summary {
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

// Venmo-style lag: two wallet payments are booked tonight, but the bank debit that funded them
// posts days later and the wallet keeps reporting 0. The wallet's opening must stay pinned
// (opening equity unchanged), the expense must hit net worth once, the gap must sit in the
// configured pending account, and the later funding debit must clear it without double
// counting.
func TestWalletFundingLagGoesToPendingNotOpening(t *testing.T) {
	root := t.TempDir()
	full := testfixture.Extract(t, root)
	testfixture.Nightly(t, root)
	p := testfixture.Config(t, root)
	const pend = "Liabilities:Pending:WalletFunding"
	b, err := os.ReadFile(p.Map)
	if err != nil {
		t.Fatal(err)
	}
	m := strings.Replace(string(b), `name: "Assets:Bank:Wallet", institution: TestBank, tracking: connector}`,
		`name: "Assets:Bank:Wallet", institution: TestBank, tracking: connector, pending_account: "`+pend+`"}`, 1)
	if m == string(b) {
		t.Fatal("fixture: wallet mapping not found")
	}
	if err := os.WriteFile(p.Map, []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	appendFile(t, p.LocalRules, "  - {name: bowling, match: '(?i)bowling', account: Expenses:Entertainment}\n")
	cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, p.LocalRules, p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	db, err := ledger.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	importDir(t, db, cfg, full)
	importDir(t, db, cfg, filepath.Join(root, testfixture.NightlyAsOf))

	const wallet = "Assets:Bank:Wallet"
	type state struct{ nw, equity, opening, wallet, pending, ent, checking int64 }
	read := func() state {
		return state{
			nw:       sumWhere(t, db, "account_type IN ('asset','liability')"),
			equity:   sumWhere(t, db, "account = ?", books.OpeningEquity),
			opening:  sumWhere(t, db, "entry_type = 'opening' AND account = ?", wallet),
			wallet:   sumWhere(t, db, "account = ?", wallet),
			pending:  sumWhere(t, db, "account = ?", pend),
			ent:      sumWhere(t, db, "account = 'Expenses:Entertainment'"),
			checking: sumWhere(t, db, "account = 'Assets:Bank:TestBank:Checking'"),
		}
	}
	base := read()
	reconciles := func(when string) {
		t.Helper()
		if _, bad, err := report.Reconcile(db); err != nil || bad != 0 {
			t.Fatalf("%s: reconcile %d differences, %v", when, bad, err)
		}
	}
	reconciles("base")

	// night 1: payments posted, funding not yet; wallet still reports 0
	night1 := writeExtract(t, root, "2026-02-12", "1467.24",
		"g-1,acc-wallet,2026-02-11,-60.0000,out,USD,Friend Bowling,,,false,\n"+
			"g-2,acc-wallet,2026-02-11,-25.0000,out,USD,Friend Bowling,,,false,\n")
	s1 := importDir(t, db, cfg, night1)
	got := read()
	if len(s1.OpeningDrift) != 0 || got.opening != base.opening || got.equity != base.equity {
		t.Fatalf("the funding lag moved the opening: drift %+v, opening %d -> %d, equity %d -> %d", s1.OpeningDrift, base.opening, got.opening, base.equity, got.equity)
	}
	if got.ent-base.ent != 8500 || got.nw-base.nw != -8500 {
		t.Fatalf("expense %d / net worth change %d, want +8500 / -8500", got.ent-base.ent, got.nw-base.nw)
	}
	if got.pending != -8500 || got.wallet != 0 {
		t.Fatalf("pending %d (want -8500), wallet %d (want reported 0)", got.pending, got.wallet)
	}
	if len(s1.Result.Pending) != 1 || !s1.Result.Pending[0].Configured || s1.Result.Pending[0].Pending != pend {
		t.Fatalf("pending gaps: %+v", s1.Result.Pending)
	}
	reconciles("night 1")
	// rerun: no-op
	if again := importDir(t, db, cfg, night1); again.Sync.Inserted+again.Sync.Updated+again.Sync.Deleted != 0 {
		t.Fatalf("rerun changed the journal: %+v", again.Sync)
	}
	night1State := read()

	// night 2: the bank funding debits post (checking reports 144 less); wallet still 0
	night2 := writeExtract(t, root, "2026-02-14", "1382.24",
		"f-1,acc-checking,2026-02-13,-60.0000,out,USD,WALLETPAY,,,false,\n"+
			"f-2,acc-checking,2026-02-13,-25.0000,out,USD,WALLETPAY,,,false,\n")
	s2 := importDir(t, db, cfg, night2)
	got = read()
	if len(s2.Result.Pending) != 0 || got.pending != 0 {
		t.Fatalf("pending not cleared by the funding debit: %+v, balance %d", s2.Result.Pending, got.pending)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE external_id = 'pending:acc-wallet'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("pending entry still in the journal (%d, %v)", n, err)
	}
	if got.ent != night1State.ent || got.nw != night1State.nw || got.equity != base.equity || got.opening != base.opening {
		t.Fatalf("funding debit double counted or moved equity: %+v, night 1 %+v", got, night1State)
	}
	if got.wallet != 0 || got.checking-base.checking != -8500 {
		t.Fatalf("wallet %d (want 0), checking change %d (want -8500)", got.wallet, got.checking-base.checking)
	}
	if len(s2.OpeningDrift) != 0 {
		t.Fatalf("drift: %+v", s2.OpeningDrift)
	}
	reconciles("night 2")
	if again := importDir(t, db, cfg, night2); again.Sync.Inserted+again.Sync.Updated+again.Sync.Deleted != 0 {
		t.Fatalf("rerun changed the journal: %+v", again.Sync)
	}
}

// An explicit pin correction (with evidence) is applied and the journal rebuilt.
func TestPinOpeningCorrectsPin(t *testing.T) {
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
	importDir(t, db, cfg, full)
	const wallet = "Assets:Bank:Wallet"
	if _, err := ledger.PinOpening(db, cfg, wallet, 1000, ""); err == nil {
		t.Fatal("pin without a note accepted")
	}
	if _, err := ledger.PinOpening(db, cfg, "Assets:Investments:Brokerage", 1000, "x"); err == nil {
		t.Fatal("amount pin on a holdings-tracked account accepted")
	}
	s, err := ledger.PinOpening(db, cfg, wallet, 1000, "test evidence")
	if err != nil {
		t.Fatal(err)
	}
	if got := sumWhere(t, db, "entry_type = 'opening' AND account = ?", wallet); got != 1000 {
		t.Fatalf("opening %d", got)
	}
	if len(s.Result.Pending) != 1 || s.Result.Pending[0].Amount != -1000 || s.Result.Pending[0].Configured {
		t.Fatalf("pending: %+v", s.Result.Pending)
	}
}
