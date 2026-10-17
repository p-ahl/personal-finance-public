package report

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Payables lists open payables / accrued liabilities (and, with receivables, open receivables
// and prepaids) as of a date, aged from the recognition date.
func Payables(db *sql.DB, asOf string) (*Table, error) {
	t := &Table{Title: "Open payables, accrued liabilities, receivables and prepaids as of " + asOf,
		Headers: []string{"Type", "Accrual", "Counterparty", "Balance account", "Recognized", "Age (days)", "Bucket", "Recognized amt", "Settled", "Open"},
		Right:   []bool{false, false, false, false, false, true, false, true, true, true}}
	ref, err := time.Parse("2006-01-02", asOf)
	if err != nil {
		return nil, err
	}
	// balances as of the date: recompute from links so the report honors --as-of
	rows, err := db.Query(`
		SELECT ac.accrual_key, COALESCE(cp.name,''), ba.name, ba.account_type, COALESCE(ba.level2,''),
		  MIN(CASE WHEN al.role IN ('accrual','opening') THEN e.entry_date END),
		  SUM(CASE WHEN al.role IN ('accrual','opening') THEN l.amount ELSE 0 END),
		  SUM(CASE WHEN al.role = 'settlement' THEN l.amount ELSE 0 END),
		  SUM(l.amount)
		FROM accruals ac
		JOIN accrual_links al  ON al.accrual_id = ac.id
		JOIN journal_entries e ON e.id = al.entry_id AND e.status = 'posted' AND e.entry_date <= ?
		JOIN journal_lines l   ON l.entry_id = e.id AND l.account_id = ac.balance_account_id
		JOIN accounts ba       ON ba.id = ac.balance_account_id
		LEFT JOIN counterparties cp ON cp.id = ac.counterparty_id
		GROUP BY ac.id HAVING SUM(l.amount) <> 0
		ORDER BY ba.account_type DESC, ba.name, ac.accrual_key`, asOf)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tot := map[string]int64{}
	for rows.Next() {
		var key, cp, acct, typ, class string
		var rec sql.NullString
		var recog, settled, bal int64
		if err := rows.Scan(&key, &cp, &acct, &typ, &class, &rec, &recog, &settled, &bal); err != nil {
			return nil, err
		}
		kind := "payable"
		open := -bal
		switch {
		case typ == "asset" && class == "Prepaid":
			kind, open = "prepaid", bal
		case typ == "asset":
			kind, open = "receivable", bal
		case class == "Accrued":
			kind = "accrued"
		}
		age, bucket := "", ""
		if rec.Valid {
			d, _ := time.Parse("2006-01-02", rec.String)
			n := int(ref.Sub(d).Hours() / 24)
			age = fmt.Sprint(n)
			switch {
			case n <= 30:
				bucket = "0-30"
			case n <= 60:
				bucket = "31-60"
			case n <= 90:
				bucket = "61-90"
			default:
				bucket = "90+"
			}
		}
		t.Add(kind, key, cp, acct, rec.String, age, bucket, usd(abs(recog)), usd(abs(settled)), usd(open))
		tot[kind] += open
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	t.Sep()
	kinds := make([]string, 0, len(tot))
	for k := range tot {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		t.Add("", "Total "+k, "", "", "", "", "", "", "", usd(tot[k]))
	}
	return t, nil
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// AccrualVsCash compares P&L by month on both bases (net income and each section), plus the
// accounts where the bases differ over the period.
func AccrualVsCash(db *sql.DB, from, to string) (*Table, error) {
	rows, err := db.Query(`SELECT month, account_type, basis, SUM(natural_minor) FROM v_pnl_lines
		WHERE date BETWEEN ? AND ? GROUP BY 1, 2, 3`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type k struct{ m, typ, basis string }
	v := map[k]int64{}
	for rows.Next() {
		var x k
		var n int64
		if err := rows.Scan(&x.m, &x.typ, &x.basis, &n); err != nil {
			return nil, err
		}
		v[x] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	t := &Table{Title: fmt.Sprintf("Accrual vs cash basis %s to %s", from, to),
		Headers: []string{"Month", "Income (accrual)", "Expenses (accrual)", "Net (accrual)", "Income (cash)", "Expenses (cash)", "Net (cash)", "Net diff"},
		Right:   []bool{false, true, true, true, true, true, true, true}}
	var ti, te, ci, ce int64
	for _, m := range Months(from, to) {
		ai, ae := v[k{m, "income", "accrual"}], v[k{m, "expense", "accrual"}]
		bi, be := v[k{m, "income", "cash"}], v[k{m, "expense", "cash"}]
		ti, te, ci, ce = ti+ai, te+ae, ci+bi, ce+be
		t.Add(m, usd(ai), usd(ae), usd(ai-ae), usd(bi), usd(be), usd(bi-be), usd((ai-ae)-(bi-be)))
	}
	t.Sep()
	t.Add("Total", usd(ti), usd(te), usd(ti-te), usd(ci), usd(ce), usd(ci-ce), usd((ti-te)-(ci-ce)))

	drows, err := db.Query(`SELECT account,
		SUM(CASE WHEN basis='accrual' THEN natural_minor ELSE 0 END),
		SUM(CASE WHEN basis='cash' THEN natural_minor ELSE 0 END)
		FROM v_pnl_lines WHERE date BETWEEN ? AND ? GROUP BY account
		HAVING SUM(CASE WHEN basis='accrual' THEN natural_minor ELSE -natural_minor END) <> 0
		ORDER BY account`, from, to)
	if err != nil {
		return nil, err
	}
	defer drows.Close()
	for drows.Next() {
		var a string
		var x, y int64
		if err := drows.Scan(&a, &x, &y); err != nil {
			return nil, err
		}
		t.Notes = append(t.Notes, fmt.Sprintf("%s: accrual %s, cash %s, diff %s", a, usd(x), usd(y), usd(x-y)))
	}
	return t, drows.Err()
}
