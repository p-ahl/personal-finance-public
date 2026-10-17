package report

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Months lists YYYY-MM between two dates inclusive.
func Months(from, to string) []string {
	f, _ := time.Parse("2006-01-02", from)
	t, _ := time.Parse("2006-01-02", to)
	var out []string
	for m := time.Date(f.Year(), f.Month(), 1, 0, 0, 0, 0, time.UTC); !m.After(t); m = m.AddDate(0, 1, 0) {
		out = append(out, m.Format("2006-01"))
	}
	return out
}

var sectionOrder = map[string]int{"asset": 0, "liability": 1, "equity": 2, "income": 3, "expense": 4}

// BalanceSheet as of a date (inclusive). Net income since inception is computed, not stored.
func BalanceSheet(db *sql.DB, asOf string) (*Table, error) {
	rows, err := db.Query(`SELECT account_type, report_line, account, MAX(valuation_only), SUM(natural_minor)
		FROM v_ledger WHERE statement = 'balance_sheet' AND date <= ?
		GROUP BY account_id HAVING SUM(amount_minor) <> 0`, asOf)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type r struct {
		typ, line, acct string
		val             bool
		amt             int64
	}
	var rs []r
	for rows.Next() {
		var x r
		if err := rows.Scan(&x.typ, &x.line, &x.acct, &x.val, &x.amt); err != nil {
			return nil, err
		}
		rs = append(rs, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var ni int64
	if err := db.QueryRow(`SELECT -COALESCE(SUM(amount_minor),0) FROM v_ledger WHERE statement='income_statement' AND date <= ?`, asOf).Scan(&ni); err != nil {
		return nil, err
	}
	rs = append(rs, r{typ: "equity", line: "Retained earnings", acct: "Equity:Net-Income (computed, since books start)", amt: ni})
	sort.SliceStable(rs, func(i, j int) bool {
		if sectionOrder[rs[i].typ] != sectionOrder[rs[j].typ] {
			return sectionOrder[rs[i].typ] < sectionOrder[rs[j].typ]
		}
		if rs[i].line != rs[j].line {
			return rs[i].line < rs[j].line
		}
		return rs[i].acct < rs[j].acct
	})
	t := &Table{Title: "Balance sheet as of " + asOf, Headers: []string{"Section", "Line", "Account", "Amount"}, Right: []bool{false, false, false, true}}
	totals := map[string]int64{}
	cur := ""
	for _, x := range rs {
		if cur != "" && x.typ != cur {
			t.Add("", "", "Total "+plural(cur), usd(totals[cur]))
			t.Sep()
		}
		cur = x.typ
		name := x.acct
		if x.val {
			name += " *"
		}
		t.Add(plural(x.typ), x.line, name, usd(x.amt))
		totals[x.typ] += x.amt
	}
	t.Add("", "", "Total "+plural(cur), usd(totals[cur]))
	t.Sep()
	t.Add("", "", "Liabilities + equity", usd(totals["liability"]+totals["equity"]))
	t.Add("", "", "Check: assets - (liabilities + equity)", usd(totals["asset"]-totals["liability"]-totals["equity"]))
	t.Notes = append(t.Notes, "* valuation-only account: balance from the connector, no transaction history (opening entry only).")
	return t, nil
}

func plural(typ string) string {
	switch typ {
	case "asset":
		return "Assets"
	case "liability":
		return "Liabilities"
	case "equity":
		return "Equity"
	case "income":
		return "Income"
	case "expense":
		return "Expenses"
	}
	return typ
}

// IncomeStatement for [from, to]; monthly adds one column per month. Rows are grouped by the
// second account level (category); detail=true shows full account names instead.
//
// basis is "accrual" (default: recognition date, the books as kept) or "cash" (expenses/income
// on the payment date: accrual recognition entries excluded, settlements of payables/prepaids
// re-expressed in their P&L account). See v_pnl_lines.
func IncomeStatement(db *sql.DB, from, to string, monthly, detail bool, basis ...string) (*Table, error) {
	b := "accrual"
	if len(basis) > 0 && basis[0] != "" {
		b = basis[0]
	}
	if b != "accrual" && b != "cash" {
		return nil, fmt.Errorf("basis must be accrual or cash")
	}
	group := "COALESCE(level2, account)"
	if detail {
		group = "account"
	}
	rows, err := db.Query(`SELECT account_type, `+group+`, month, SUM(natural_minor) FROM v_pnl_lines
		WHERE basis = ? AND date BETWEEN ? AND ? GROUP BY 1, 2, 3`, b, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct{ typ, cat string }
	data := map[key]map[string]int64{}
	for rows.Next() {
		var k key
		var m string
		var v int64
		if err := rows.Scan(&k.typ, &k.cat, &m, &v); err != nil {
			return nil, err
		}
		if data[k] == nil {
			data[k] = map[string]int64{}
		}
		data[k][m] += v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	months := Months(from, to)
	cols := []string{}
	if monthly {
		cols = months
	}
	t := &Table{Title: fmt.Sprintf("Income statement (%s basis) %s to %s", b, from, to)}
	t.Headers = append([]string{"Section", "Category"}, append(append([]string{}, cols...), "Total")...)
	t.Right = make([]bool, len(t.Headers))
	for i := 2; i < len(t.Right); i++ {
		t.Right[i] = true
	}
	secTot := map[string]map[string]int64{"income": {}, "expense": {}}
	for _, typ := range []string{"income", "expense"} {
		var keys []key
		for k := range data {
			if k.typ == typ {
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool {
			return sum(data[keys[i]]) > sum(data[keys[j]])
		})
		for _, k := range keys {
			cells := []string{plural(typ), k.cat}
			for _, m := range cols {
				cells = append(cells, usd(data[k][m]))
				secTot[typ][m] += data[k][m]
			}
			cells = append(cells, usd(sum(data[k])))
			secTot[typ]["total"] += sum(data[k])
			t.Add(cells...)
		}
		cells := []string{"", "Total " + strings.ToLower(plural(typ))}
		for _, m := range cols {
			cells = append(cells, usd(secTot[typ][m]))
		}
		cells = append(cells, usd(secTot[typ]["total"]))
		t.Add(cells...)
		t.Sep()
	}
	cells := []string{"", "Net income"}
	for _, m := range cols {
		cells = append(cells, usd(secTot["income"][m]-secTot["expense"][m]))
	}
	cells = append(cells, usd(secTot["income"]["total"]-secTot["expense"]["total"]))
	t.Add(cells...)
	return t, nil
}

func sum(m map[string]int64) int64 {
	var s int64
	for _, v := range m {
		s += v
	}
	return s
}

// CashFlow (direct method) for [from, to]: the non-cash legs of entries touching cash accounts,
// classified by the counterpart account's cash_flow_class, reconciled to the change in cash.
func CashFlow(db *sql.DB, from, to string, monthly bool) (*Table, error) {
	rows, err := db.Query(`SELECT flow_class, flow_line, month, SUM(cash_effect_minor) FROM v_cash_flow_lines
		WHERE date BETWEEN ? AND ? GROUP BY 1, 2, 3`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct{ class, line string }
	data := map[key]map[string]int64{}
	for rows.Next() {
		var k key
		var m string
		var v int64
		if err := rows.Scan(&k.class, &k.line, &m, &v); err != nil {
			return nil, err
		}
		if data[k] == nil {
			data[k] = map[string]int64{}
		}
		data[k][m] += v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cols := []string{}
	if monthly {
		cols = Months(from, to)
	}
	t := &Table{Title: fmt.Sprintf("Cash flow (direct method) %s to %s", from, to)}
	t.Headers = append([]string{"Activity", "Line"}, append(append([]string{}, cols...), "Total")...)
	t.Right = make([]bool, len(t.Headers))
	for i := 2; i < len(t.Right); i++ {
		t.Right[i] = true
	}
	all := map[string]int64{}
	for _, class := range []string{"operating", "investing", "financing", "opening", "transfer", "unclassified"} {
		var keys []key
		for k := range data {
			if k.class == class {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			continue
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].line < keys[j].line })
		tot := map[string]int64{}
		for _, k := range keys {
			cells := []string{class, k.line}
			for _, m := range cols {
				cells = append(cells, usd(data[k][m]))
				tot[m] += data[k][m]
			}
			cells = append(cells, usd(sum(data[k])))
			tot["total"] += sum(data[k])
			t.Add(cells...)
		}
		cells := []string{"", "Net " + class}
		for _, m := range cols {
			cells = append(cells, usd(tot[m]))
			all[m] += tot[m]
		}
		cells = append(cells, usd(tot["total"]))
		all["total"] += tot["total"]
		t.Add(cells...)
		t.Sep()
	}
	cells := []string{"", "Net change in cash"}
	for _, m := range cols {
		cells = append(cells, usd(all[m]))
	}
	cells = append(cells, usd(all["total"]))
	t.Add(cells...)
	var begin, end int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN date < ? THEN amount_minor END),0), COALESCE(SUM(CASE WHEN date <= ? THEN amount_minor END),0)
		FROM v_ledger WHERE cash_flow_class = 'cash'`, from, to).Scan(&begin, &end); err != nil {
		return nil, err
	}
	t.Notes = append(t.Notes, fmt.Sprintf("Cash accounts: beginning %s, ending %s, change %s (difference vs activities: %s).",
		usd(begin), usd(end), usd(end-begin), usd(end-begin-all["total"])))
	t.Notes = append(t.Notes, "`transfer` = bank-to-bank transfers whose legs post on different days (nets to zero once both post); `unclassified` = unmatched transfer-like rows.")
	return t, nil
}

// TrialBalance as of a date: per-account debits, credits and balance; debits must equal credits.
func TrialBalance(db *sql.DB, asOf string) (*Table, error) {
	rows, err := db.Query(`SELECT account,
		SUM(CASE WHEN amount_minor > 0 THEN amount_minor ELSE 0 END), SUM(CASE WHEN amount_minor < 0 THEN -amount_minor ELSE 0 END), SUM(amount_minor)
		FROM v_ledger WHERE date <= ? GROUP BY account_id ORDER BY account`, asOf)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	t := &Table{Title: "Trial balance as of " + asOf, Headers: []string{"Account", "Debits", "Credits", "Balance (Dr+)"}, Right: []bool{false, true, true, true}}
	var td, tc, tb int64
	for rows.Next() {
		var a string
		var d, c, b int64
		if err := rows.Scan(&a, &d, &c, &b); err != nil {
			return nil, err
		}
		td, tc, tb = td+d, tc+c, tb+b
		t.Add(a, usd(d), usd(c), usd(b))
	}
	t.Sep()
	t.Add("Total", usd(td), usd(tc), usd(tb))
	return t, rows.Err()
}

// Uncategorized lists lines on the Uncategorized accounts, largest first.
func Uncategorized(db *sql.DB, from, to string, limit int, grouped bool) (*Table, error) {
	if grouped {
		rows, err := db.Query(`SELECT account, COALESCE(counterparty,''), COUNT(*), SUM(natural_minor) FROM v_uncategorized
			WHERE date BETWEEN ? AND ? GROUP BY 1, 2 ORDER BY ABS(SUM(natural_minor)) DESC LIMIT ?`, from, to, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		t := &Table{Title: "Uncategorized by counterparty", Headers: []string{"Account", "Counterparty", "Count", "Amount"}, Right: []bool{false, false, true, true}}
		for rows.Next() {
			var a, c string
			var n int
			var v int64
			if err := rows.Scan(&a, &c, &n, &v); err != nil {
				return nil, err
			}
			t.Add(a, c, fmt.Sprint(n), usd(v))
		}
		return t, rows.Err()
	}
	rows, err := db.Query(`SELECT u.date, u.account, u.description, COALESCE(u.counterparty,''), u.natural_minor,
		(SELECT a.name FROM journal_lines l JOIN accounts a ON a.id = l.account_id WHERE l.entry_id = u.entry_id AND l.id <> u.line_id LIMIT 1)
		FROM v_uncategorized u WHERE u.date BETWEEN ? AND ? ORDER BY ABS(u.natural_minor) DESC LIMIT ?`, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	t := &Table{Title: "Uncategorized lines", Headers: []string{"Date", "Account", "Description", "Counterparty", "Amount", "Source account"}, Right: []bool{false, false, false, false, true, false}}
	for rows.Next() {
		var d, a, desc, c, src string
		var v int64
		if err := rows.Scan(&d, &a, &desc, &c, &v, &src); err != nil {
			return nil, err
		}
		t.Add(d, a, desc, c, usd(v), src)
	}
	return t, rows.Err()
}

// NetWorth per balance-sheet account as of a date, flagging valuation-only and non-connector values.
func NetWorth(db *sql.DB, asOf string) (*Table, error) {
	rows, err := db.Query(`SELECT a.name, a.account_type, a.valuation_only, a.tracking, COALESCE(i.name,''),
		COALESCE((SELECT SUM(l.amount) FROM journal_lines l JOIN journal_entries e ON e.id = l.entry_id
		          WHERE l.account_id = a.id AND e.status = 'posted' AND e.entry_date <= ?),0) AS bal
		FROM accounts a LEFT JOIN institutions i ON i.id = a.institution_id
		WHERE a.account_type IN ('asset','liability') AND a.is_placeholder = 0
		ORDER BY a.account_type, a.name`, asOf)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	t := &Table{Title: "Net worth as of " + asOf, Headers: []string{"Account", "Institution", "Basis", "Amount"}, Right: []bool{false, false, false, true}}
	var total, valOnly, internal int64
	for rows.Next() {
		var name, typ, tracking, inst string
		var val bool
		var bal int64
		if err := rows.Scan(&name, &typ, &val, &tracking, &inst, &bal); err != nil {
			return nil, err
		}
		if bal == 0 && (tracking != "connector" && tracking != "holdings" || strings.HasPrefix(name, "Assets:Crypto:SelfCustody")) {
			continue
		}
		basis := "connector (transactions)"
		switch {
		case val:
			basis = "valuation-only (connector balance)"
			valOnly += bal
		case tracking == "holdings" && strings.HasPrefix(name, "Assets:Crypto:SelfCustody"):
			basis = "on-chain wallet balance (marked to market)"
		case tracking == "holdings" && inst == "":
			basis = "exchange API holdings (marked to market)"
		case tracking == "holdings":
			basis = "connector holdings (marked to market)"
		case strings.HasPrefix(name, "Assets:Transfers"):
			basis = "clearing"
		case tracking != "connector":
			basis = "derived (net flows at cost; not in connector)"
			internal += bal
		}
		total += bal
		t.Add(name, inst, basis, usd(bal))
	}
	t.Sep()
	t.Add("Net worth", "", "", usd(total))
	t.Add("  of which valuation-only", "", "", usd(valOnly))
	t.Add("  of which derived / not in connector", "", "", usd(internal))
	t.Add("Net worth excluding derived", "", "", usd(total-internal))
	return t, rows.Err()
}

// Reconcile compares the ledger balance of every connector account with the connector balance.
func Reconcile(db *sql.DB) (*Table, int, error) {
	// newest connector assertion per account (normally all share the latest extract date; an
	// account missing from the latest extract keeps its last snapshot)
	rows, err := db.Query(`SELECT c.account, a.account_type, c.as_of, c.expected_minor, c.actual_minor
		FROM v_balance_assertion_check c JOIN accounts a ON a.name = c.account
		WHERE c.source = 'connector' AND c.as_of = (SELECT MAX(b.as_of) FROM balance_assertions b
		      WHERE b.account_id = a.id AND b.source = 'connector')
		ORDER BY a.account_type, c.account`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	t := &Table{Headers: []string{"Account", "Connector balance", "DB balance", "Difference"}, Right: []bool{false, true, true, true}}
	bad := 0
	asOf := ""
	older := map[string][]string{}
	for rows.Next() {
		var name, typ, d string
		var exp, act int64
		if err := rows.Scan(&name, &typ, &d, &exp, &act); err != nil {
			return nil, 0, err
		}
		older[d] = append(older[d], name)
		if d > asOf {
			asOf = d
		}
		if typ == "liability" { // show amounts owed as positive, like the connector
			exp, act = -exp, -act
		}
		if exp != act {
			bad++
		}
		t.Add(name, usd(exp), usd(act), usd(act-exp))
	}
	t.Title = "Reconciliation vs connector balances as of " + asOf
	for d, names := range older {
		if d != asOf {
			t.Notes = append(t.Notes, fmt.Sprintf("older snapshot %s (not in the latest extract): %s", d, strings.Join(names, ", ")))
		}
	}
	sort.Strings(t.Notes)
	return t, bad, rows.Err()
}

// Spending by top-level expense category per month, plus total.
func Spending(db *sql.DB, from, to string) (*Table, error) {
	t, err := IncomeStatement(db, from, to, true, false)
	if err != nil {
		return nil, err
	}
	out := &Table{Title: fmt.Sprintf("Spending by category %s to %s", from, to), Headers: t.Headers[1:], Right: t.Right[1:]}
	for _, r := range t.Rows {
		if r != nil && r[0] == "Expenses" {
			out.Add(r[1:]...)
		}
		if r != nil && r[1] == "Total expenses" {
			out.Sep()
			out.Add(r[1:]...)
		}
	}
	return out, nil
}
