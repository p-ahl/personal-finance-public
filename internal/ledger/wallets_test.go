package ledger_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/ledger"
	"github.com/p-ahl/personal-finance/internal/report"
)

// walletPull writes a synthetic wallet pull (no real identifiers) next to the Kraken fixture:
// wallet "Cold" receives the pre-books KAS withdrawal (500) and the in-books BTC withdrawal,
// swaps the BTC to KAS, sends 600 KAS back to Kraken (the fixture's deposit), receives an
// untraced 100 KAS, and a pass-through crosses it.
func walletPull(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "raw", "wallets", "2026-02-01")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"MANIFEST.md": "# synthetic wallet pull\n",
		"balances.json": `{"as_of":"2026-02-01","fetched_at":"2026-02-01T12:00:00Z","wallets":[{"wallet":"Cold","account":"Assets:Crypto:SelfCustody:Cold","per_coin":true,
 "identifiers":[{"chain":"kaspa","kind":"address","value":"kaspa:example-not-real","used":true}],
 "holdings":[{"chain":"kaspa","commodity":"KAS","account":"Assets:Crypto:SelfCustody:Cold:KAS","balance":"1149.5","addresses":1,"used_addresses":1},
             {"chain":"bitcoin","commodity":"BTC","account":"Assets:Crypto:SelfCustody:Cold:BTC","balance":"0","addresses":1,"used_addresses":1}]}],
 "prices":[{"commodity":"KAS","price":"0.07","source":"kraken-ticker","ref":"KASUSD"},{"commodity":"BTC","price":"100000","source":"kraken-ticker","ref":"XBTUSD"}]}`,
		"events.csv": "id,time,date,kind,wallet_out,coin_out,qty_out,fee_out,wallet_in,coin_in,qty_in,ref,price_out,price_in,usd_out,usd_in,confidence,evidence,memo\n" +
			"w1,2025-12-02T13:00:00Z,2025-12-02,exchange_out,,,,,Cold,KAS,500,kraken:FTFAKE-0002,,0.1,,50.00,high,aa,\n" +
			"w2,2025-12-10T13:00:00Z,2025-12-10,unmatched_in,,,,,Cold,KAS,150,,,0.06,,9.00,,bb,unknown\n" +
			"w3,2026-01-10T13:00:00Z,2026-01-10,passthrough,,,,,Cold,KAS,1000,,,0.06,,60.00,high,cc,pass-through\n" +
			"w4,2026-01-10T15:00:00Z,2026-01-10,passthrough,Cold,KAS,1000,,,,,,0.06,,60.00,,high,dd,pass-through\n" +
			"w5,2026-01-20T13:00:00Z,2026-01-20,exchange_out,,,,,Cold,BTC,0.001,kraken:FTFAKE-0004,,50000,,50.00,high,ee,\n" +
			"w6,2026-01-21T13:00:00Z,2026-01-21,swap,Cold,BTC,0.001,,Cold,KAS,1000,,60000,0.059,60.00,59.00,high,ff gg,\n" +
			"w7,2026-01-25T12:00:00Z,2026-01-25,exchange_in,Cold,KAS,600,0.5,,,,kraken:FTFAKE-0005,0.06,,36.00,,high,hh,\n" +
			"w8,2026-01-28T13:00:00Z,2026-01-28,unmatched_in,,,,,Cold,KAS,100,,,0.06,,6.00,,ii,unknown\n",
	}
	for n, s := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestWalletImportCarriesCostAndReconciles(t *testing.T) {
	db, cfg, kpull, root := krakenSetup(t)
	if _, err := ledger.ImportKraken(db, cfg, kpull); err != nil {
		t.Fatal(err)
	}
	wpull := walletPull(t, root)
	s, err := ledger.ImportWallets(db, cfg, wpull, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Wallets.Events != 8 || s.Wallets.Snapshots != 2 {
		t.Fatalf("staging: %+v", s.Wallets)
	}
	const kas = "Assets:Crypto:SelfCustody:Cold:KAS"
	checks := []struct {
		name, where string
		args        []any
		want        int64
	}{
		{"opening: 500 KAS at Kraken cost 49.50 + 150 untraced at 9.00", "external_id = 'opening:wallets' AND account = ?", []any{kas}, 5850},
		{"BTC withdrawal re-pointed to the wallet at cost", "external_id = 'kraken:FTFAKE-0004' AND account = 'Assets:Crypto:SelfCustody:Cold:BTC'", nil, 5000},
		{"deposit back to Kraken carries the wallet's FIFO cost (49.50 + 100 x 0.06)", "external_id = 'kraken:FTFAKE-0005' AND account = ?", []any{kas}, -5550},
		{"no opening-equity guess on the deposit", "external_id = 'kraken:FTFAKE-0005' AND account = ?", []any{books.OpeningEquity}, 0},
		{"swap gain 10.00 (proceeds 60.00, cost 50.00)", "external_id = 'wallet:w6' AND account = ?", []any{books.RealizedST}, -1000},
		{"pass-through never booked", "external_id IN ('wallet:w3','wallet:w4')", nil, 0},
		{"self-custody clearing (crypto_selfcustody) emptied by the at-cost adjustment", "account = 'Assets:Crypto:SelfCustody'", nil, 0},
		{"wallet KAS at market: 1149.5 x 0.07", "account = ?", []any{kas}, 8047},
		{"BTC account empty", "account = 'Assets:Crypto:SelfCustody:Cold:BTC'", nil, 0},
	}
	for _, c := range checks {
		if got := sumWhere(t, db, c.where, c.args...); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
	var q int64
	if err := db.QueryRow(`SELECT SUM(quantity) FROM v_ledger WHERE account = ? AND commodity = 'KAS'`, kas).Scan(&q); err != nil || q != 11495000000000 {
		t.Fatalf("wallet KAS quantity %d %v", q, err)
	}
	tb, bad, err := report.ReconcileWallets(db)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 0 || len(tb.Rows) == 0 {
		t.Fatalf("wallet reconciliation: %d bad of %d", bad, len(tb.Rows))
	}
	var sb strings.Builder
	if err := tb.Render(&sb, "text"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "example-not-real") {
		t.Fatal("reconciliation shows a wallet identifier")
	}
	var unbalanced, mismatched int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM v_unbalanced_entries),
		(SELECT COUNT(*) FROM v_balance_assertion_check WHERE expected_minor <> actual_minor)`).Scan(&unbalanced, &mismatched); err != nil {
		t.Fatal(err)
	}
	if unbalanced != 0 || mismatched != 0 {
		t.Fatalf("unbalanced %d, assertion mismatches %d", unbalanced, mismatched)
	}
	// identifiers live in the DB only
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wallet_identifiers WHERE wallet = 'Cold'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("identifiers %d %v", n, err)
	}
	// re-import is a no-op
	s2, err := ledger.ImportWallets(db, cfg, wpull, "")
	if err != nil {
		t.Fatal(err)
	}
	if s2.Sync.Inserted+s2.Sync.Updated+s2.Sync.Deleted != 0 {
		t.Fatalf("second import changed the journal: %+v", s2.Sync)
	}
}

// laterPull copies a wallet pull to another date with the same on-chain balances, prices and
// events (nothing happened on chain in between).
func laterPull(t *testing.T, root, from, date string) string {
	t.Helper()
	dir := filepath.Join(root, "raw", "wallets", date)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"MANIFEST.md", "balances.json", "events.csv"} {
		b, err := os.ReadFile(filepath.Join(from, n))
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ReplaceAll(string(b), `"as_of":"`+filepath.Base(from)+`"`, `"as_of":"`+date+`"`)
		if err := os.WriteFile(filepath.Join(dir, n), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The at-cost adjustment is a true-up to the on-chain state, not an append: running it twice
// and again a day later on unchanged on-chain data must leave the clearing (Unassigned) account
// at zero and the wallet balances unchanged. (Regression: the second pull re-booked the cleared
// cost back into the clearing account, inflating net worth.)
func TestWalletAdjustmentIdempotentAcrossPulls(t *testing.T) {
	db, cfg, kpull, root := krakenSetup(t)
	if _, err := ledger.ImportKraken(db, cfg, kpull); err != nil {
		t.Fatal(err)
	}
	const clearing = "Assets:Crypto:SelfCustody"
	const kas = "Assets:Crypto:SelfCustody:Cold:KAS"
	wpull := walletPull(t, root)
	if _, err := ledger.ImportWallets(db, cfg, wpull, ""); err != nil {
		t.Fatal(err)
	}
	// the first pull really clears something (otherwise this test proves nothing)
	if got := sumWhere(t, db, "external_id = 'wallets:adjust:2026-02-01' AND account = ?", clearing); got == 0 {
		t.Fatal("fixture: the 2026-02-01 adjustment clears nothing")
	}
	type state struct{ clear, clearQ, kasCost, kasQ, nw, adjusts int64 }
	read := func(asOf string) state {
		t.Helper()
		var s state
		s.clear = sumWhere(t, db, "account = ? AND date <= ?", clearing, asOf)
		s.kasCost = sumWhere(t, db, "account = ? AND date <= ? AND external_id NOT LIKE 'valuation:wallets:%'", kas, asOf) // at cost
		s.nw = sumWhere(t, db, "account_type IN ('asset','liability') AND date <= ?", asOf)
		if err := db.QueryRow(`SELECT COALESCE(SUM(quantity),0) FROM v_ledger WHERE account = ? AND date <= ?`, clearing, asOf).Scan(&s.clearQ); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COALESCE(SUM(quantity),0) FROM v_ledger WHERE account = ? AND date <= ?`, kas, asOf).Scan(&s.kasQ); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(DISTINCT external_id) FROM v_ledger WHERE external_id LIKE 'wallets:adjust:%'`).Scan(&s.adjusts); err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := read("2026-02-01")
	if first.clear != 0 || first.clearQ != 0 {
		t.Fatalf("clearing after the first pull: %d cents, qty %d", first.clear, first.clearQ)
	}
	// same pull again: no journal change at all
	s2, err := ledger.ImportWallets(db, cfg, wpull, "")
	if err != nil {
		t.Fatal(err)
	}
	if s2.Sync.Inserted+s2.Sync.Updated+s2.Sync.Deleted != 0 {
		t.Fatalf("second run of the same pull changed the journal: %+v", s2.Sync)
	}
	if again := read("2026-02-01"); again != first {
		t.Fatalf("second run: %+v, first %+v", again, first)
	}
	// a day later, same on-chain data: no new adjustment, clearing stays at zero
	next := laterPull(t, root, wpull, "2026-02-02")
	if _, err := ledger.ImportWallets(db, cfg, next, ""); err != nil {
		t.Fatal(err)
	}
	day2 := read("2026-02-02")
	if day2.clear != 0 || day2.clearQ != 0 {
		t.Fatalf("clearing after the day-later pull: %d cents, qty %d (want 0)", day2.clear, day2.clearQ)
	}
	if n := sumWhere(t, db, "external_id = 'wallets:adjust:2026-02-02'"); n != 0 {
		t.Fatalf("day-later pull booked an adjustment")
	}
	var cnt int
	if err := db.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE external_id = 'wallets:adjust:2026-02-02'`).Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("day-later adjustment entry exists (%d, %v)", cnt, err)
	}
	if day2.kasQ != first.kasQ || day2.kasCost != first.kasCost || day2.nw != first.nw || day2.adjusts != first.adjusts {
		t.Fatalf("day-later pull moved the books: %+v, before %+v", day2, first)
	}
	// and rerunning the day-later pull is a no-op too
	s4, err := ledger.ImportWallets(db, cfg, next, "")
	if err != nil {
		t.Fatal(err)
	}
	if s4.Sync.Inserted+s4.Sync.Updated+s4.Sync.Deleted != 0 {
		t.Fatalf("rerun of the day-later pull changed the journal: %+v", s4.Sync)
	}
}

// Marks are dated at every valuation date (month-ends from the daily price history and each pull
// date), not lumped into the newest pull; crypto_valuations records quantity, price, value and
// cost basis per day, and a rerun changes nothing.
func TestWalletMarksAreDatedAndValuationsRecorded(t *testing.T) {
	db, cfg, kpull, root := krakenSetup(t)
	if _, err := ledger.ImportKraken(db, cfg, kpull); err != nil {
		t.Fatal(err)
	}
	wpull := walletPull(t, root)
	hist := filepath.Join(wpull, "history")
	if err := os.MkdirAll(hist, 0o700); err != nil {
		t.Fatal(err)
	}
	// Kraken OHLC rows: [time, open, high, low, close, vwap, volume, count]; 2026-01-31 00:00 UTC closes at 0.05
	ohlc := `{"error":[],"result":{"KASUSD":[[1769817600,"0.05","0.05","0.05","0.05","0.05","1",1],[1769904000,"0.06","0.06","0.06","0.06","0.06","1",1]]}}`
	if err := os.WriteFile(filepath.Join(hist, "ohlc_KAS.json"), []byte(ohlc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ImportWallets(db, cfg, wpull, ""); err != nil {
		t.Fatal(err)
	}
	const kas = "Assets:Crypto:SelfCustody:Cold:KAS"
	// the 2026-01-31 close is staged; the pull-date candle (2026-02-01 = as_of) is skipped
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM crypto_price_history WHERE commodity='KAS'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("price history rows %d %v", n, err)
	}
	// month-end mark exists and values the held quantity at that day's close
	var q, c int64 // c unused beyond the scan
	_ = c
	if err := db.QueryRow(`SELECT COALESCE(SUM(quantity),0), COALESCE(SUM(amount_minor),0) FROM v_ledger WHERE account = ? AND date <= '2026-01-31' AND external_id NOT LIKE 'valuation:wallets:%'`, kas).Scan(&q, &c); err != nil {
		t.Fatal(err)
	}
	if got := sumWhere(t, db, "account = ? AND date <= '2026-01-31'", kas); got != (q*5+5000000000)/10000000000 {
		t.Fatalf("KAS carried at %d on 2026-01-31, want quantity x 0.05 = %d", got, (q*5+5000000000)/10000000000)
	}
	if got := sumWhere(t, db, "external_id = 'valuation:wallets:2026-01-31' AND account = ?", books.UnrealizedAcc); got == 0 {
		t.Fatal("no unrealized entry on the month-end")
	}
	// valuation history: a row per day for the held coin, last day ties to the ledger
	var v, k int64
	if err := db.QueryRow(`SELECT value_cents, cost_cents FROM crypto_valuations WHERE val_date = '2026-02-01' AND account = ?`, kas).Scan(&v, &k); err != nil {
		t.Fatal(err)
	}
	if v != 8047 || sumWhere(t, db, "account = ? AND external_id NOT LIKE 'valuation:wallets:%'", kas) != k {
		t.Fatalf("valuation row value %d cost %d", v, k)
	}
	if err := db.QueryRow(`SELECT COUNT(DISTINCT val_date) FROM crypto_valuations WHERE account = ?`, kas).Scan(&n); err != nil || n != 2 { // only 2026-01-31 (close) and 2026-02-01 (pull ticker) have prices in this fixture
		t.Fatalf("daily rows %d %v", n, err)
	}
	s2, err := ledger.ImportWallets(db, cfg, wpull, "")
	if err != nil {
		t.Fatal(err)
	}
	if s2.Sync.Inserted+s2.Sync.Updated+s2.Sync.Deleted != 0 {
		t.Fatalf("rerun changed the journal: %+v", s2.Sync)
	}
}
