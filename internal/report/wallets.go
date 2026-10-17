package report

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/p-ahl/personal-finance/internal/money"
)

// walletUnassigned is the clearing account for self-custody items not traced to a wallet.
const walletUnassigned = "Assets:Crypto:SelfCustody:Unassigned"

// ReconcileWallets compares each wallet account's ledger quantities with the on-chain balances
// of the newest wallet pull (wallet_balance_snapshots) and the USD balance assertion (source =
// wallets). Shows wallet account names only (no addresses). Returns the table and the number
// of differences (hard failures).
func ReconcileWallets(db *sql.DB) (*Table, int, error) {
	t := &Table{Title: "Reconciliation vs on-chain wallet balances", Headers: []string{"Account", "As of", "Item", "On-chain", "Ledger", "Difference"},
		Right: []bool{false, false, false, true, true, true}}
	var hasTable int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='wallet_balance_snapshots'`).Scan(&hasTable); err != nil {
		return nil, 0, err
	}
	if hasTable == 0 {
		t.Notes = append(t.Notes, "no wallet pulls staged (finance import wallets)")
		return t, 0, nil
	}
	rows, err := db.Query(`SELECT c.account, b.account_id, c.as_of, c.expected_minor, c.actual_minor
		FROM v_balance_assertion_check c JOIN accounts a ON a.name = c.account
		JOIN balance_assertions b ON b.account_id = a.id AND b.as_of = c.as_of AND b.source = c.source
		WHERE c.source = 'wallets' ORDER BY c.account`)
	if err != nil {
		return nil, 0, err
	}
	type asrt struct {
		account, asOf string
		id, exp, act  int64
	}
	var as []asrt
	for rows.Next() {
		var a asrt
		if err := rows.Scan(&a.account, &a.id, &a.asOf, &a.exp, &a.act); err != nil {
			rows.Close()
			return nil, 0, err
		}
		as = append(as, a)
	}
	rows.Close()
	bad := 0
	for _, a := range as {
		rep := map[string]int64{}
		r, err := db.Query(`SELECT commodity, balance FROM wallet_balance_snapshots WHERE account = ? AND as_of = ?`, a.account, a.asOf)
		if err != nil {
			return nil, 0, err
		}
		for r.Next() {
			var com, bal string
			if err := r.Scan(&com, &bal); err != nil {
				r.Close()
				return nil, 0, err
			}
			v, err := money.ParseMinor(bal, exchangeQtyScale)
			if err != nil {
				r.Close()
				return nil, 0, fmt.Errorf("%s %s: %w", a.account, com, err)
			}
			rep[com] += v
		}
		r.Close()
		led := map[string]int64{}
		r, err = db.Query(`SELECT COALESCE(l.commodity,''), COALESCE(SUM(l.quantity),0)
			FROM journal_lines l JOIN journal_entries e ON e.id = l.entry_id
			WHERE l.account_id = ? AND e.status = 'posted' AND e.entry_date <= ? GROUP BY 1`, a.id, a.asOf)
		if err != nil {
			return nil, 0, err
		}
		for r.Next() {
			var com string
			var q int64
			if err := r.Scan(&com, &q); err != nil {
				r.Close()
				return nil, 0, err
			}
			if com != "" {
				led[com] = q
			}
		}
		r.Close()
		var coms []string
		seen := map[string]bool{}
		for c := range rep {
			coms, seen[c] = append(coms, c), true
		}
		for c := range led {
			if !seen[c] {
				coms = append(coms, c)
			}
		}
		sort.Strings(coms)
		for _, c := range coms {
			if rep[c] != led[c] {
				bad++
			}
			t.Add(a.account, a.asOf, c+" quantity", qty(rep[c]), qty(led[c]), qty(led[c]-rep[c]))
		}
		if a.exp != a.act {
			bad++
		}
		t.Add(a.account, a.asOf, "total USD value", usd(a.exp), usd(a.act), usd(a.act-a.exp))
	}
	if len(as) == 0 {
		t.Notes = append(t.Notes, "no wallet pulls staged (finance import wallets)")
	}
	return t, bad, nil
}

// walletUnassignedBalance is the cost left in the self-custody clearing account (should be 0
// right after a wallet import; exchange withdrawals after the newest pull land there).
func walletUnassignedBalance(db *sql.DB) (int64, error) {
	var v int64
	err := db.QueryRow(`SELECT COALESCE(SUM(amount_minor),0) FROM v_ledger WHERE account = ?`, walletUnassigned).Scan(&v)
	return v, err
}
