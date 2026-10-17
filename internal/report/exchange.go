package report

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/p-ahl/personal-finance/internal/money"
)

const exchangeQtyScale = 10 // books.CryptoScale

// ReconcileExchange compares every exchange account (Kraken) with the newest balances the
// exchange reported: USD cash, the quantity of each commodity, and the total USD value
// (balance assertion with source = exchange name). Returns the table and the number of
// differences.
func ReconcileExchange(db *sql.DB) (*Table, int, error) {
	t := &Table{Title: "Reconciliation vs exchange-reported balances", Headers: []string{"Account", "As of", "Item", "Exchange", "Ledger", "Difference"},
		Right: []bool{false, false, false, true, true, true}}
	rows, err := db.Query(`SELECT c.account, b.account_id, c.as_of, c.source, c.expected_minor, c.actual_minor
		FROM v_balance_assertion_check c JOIN accounts a ON a.name = c.account
		JOIN balance_assertions b ON b.account_id = a.id AND b.as_of = c.as_of AND b.source = c.source
		WHERE c.source IN (SELECT DISTINCT exchange FROM exchange_balance_snapshots)
		ORDER BY c.source, c.account`)
	if err != nil {
		return nil, 0, err
	}
	type asrt struct {
		account, asOf, source string
		id                    int64
		exp, act              int64
	}
	var as []asrt
	for rows.Next() {
		var a asrt
		if err := rows.Scan(&a.account, &a.id, &a.asOf, &a.source, &a.exp, &a.act); err != nil {
			rows.Close()
			return nil, 0, err
		}
		as = append(as, a)
	}
	rows.Close()
	bad := 0
	for _, a := range as {
		// exchange-reported balances, summed per commodity (USD at the exchange's 4 decimals)
		rep := map[string]int64{}
		var repUSD4 int64
		r, err := db.Query(`SELECT asset, commodity, balance FROM exchange_balance_snapshots WHERE exchange = ? AND as_of = ?`, a.source, a.asOf)
		if err != nil {
			return nil, 0, err
		}
		for r.Next() {
			var asset, com, bal string
			if err := r.Scan(&asset, &com, &bal); err != nil {
				r.Close()
				return nil, 0, err
			}
			if com == "USD" {
				v, err := money.ParseMinor(bal, 4)
				if err != nil {
					r.Close()
					return nil, 0, fmt.Errorf("%s %s: %w", a.source, asset, err)
				}
				repUSD4 += v
				continue
			}
			v, err := money.ParseMinor(bal, exchangeQtyScale)
			if err != nil {
				r.Close()
				return nil, 0, fmt.Errorf("%s %s: %w", a.source, asset, err)
			}
			rep[com] += v
		}
		r.Close()
		// ledger: USD cash lines and quantity per commodity, dated on or before the pull
		led := map[string]int64{}
		var ledUSD int64
		r, err = db.Query(`SELECT COALESCE(l.commodity,''), COALESCE(SUM(l.quantity),0), COALESCE(SUM(CASE WHEN l.commodity IS NULL THEN l.amount END),0)
			FROM journal_lines l JOIN journal_entries e ON e.id = l.entry_id
			WHERE l.account_id = ? AND e.status = 'posted' AND e.entry_date <= ? GROUP BY 1`, a.id, a.asOf)
		if err != nil {
			return nil, 0, err
		}
		for r.Next() {
			var com string
			var q, usd int64
			if err := r.Scan(&com, &q, &usd); err != nil {
				r.Close()
				return nil, 0, err
			}
			if com == "" {
				ledUSD = usd
			} else {
				led[com] = q
			}
		}
		r.Close()
		repUSD := roundDiv(repUSD4, 100)
		if repUSD != ledUSD {
			bad++
		}
		t.Add(a.account, a.asOf, "USD cash", usd(repUSD), usd(ledUSD), usd(ledUSD-repUSD))
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
		t.Notes = append(t.Notes, "no exchange pulls staged (finance import kraken)")
	}
	return t, bad, nil
}

func qty(v int64) string {
	s := money.Format(v, exchangeQtyScale)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func roundDiv(v, d int64) int64 {
	if v >= 0 {
		return (v + d/2) / d
	}
	return -((-v + d/2) / d)
}
