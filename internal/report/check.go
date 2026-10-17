package report

import (
	"database/sql"
	"fmt"
	"io"
	"strings"
)

// Coverage of expense dollars by how they were categorized, for [from, to].
func Coverage(db *sql.DB, from, to string) (*Table, int64, int64, error) {
	rows, err := db.Query(`SELECT
		CASE WHEN account = 'Expenses:Uncategorized' THEN 'uncategorized'
		     WHEN category_source LIKE 'rule:%' THEN 'rule'
		     WHEN category_source LIKE 'plaid:%' THEN 'plaid fallback'
		     WHEN category_source LIKE 'portfolio:%' THEN 'portfolio'
		     ELSE COALESCE(category_source,'other') END, COUNT(*), SUM(amount_minor)
		FROM v_ledger WHERE account_type = 'expense' AND date BETWEEN ? AND ? GROUP BY 1 ORDER BY 3 DESC`, from, to)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	type r struct {
		k string
		n int
		v int64
	}
	var rs []r
	var total, unc int64
	for rows.Next() {
		var x r
		if err := rows.Scan(&x.k, &x.n, &x.v); err != nil {
			return nil, 0, 0, err
		}
		rs = append(rs, x)
		total += x.v
		if x.k == "uncategorized" {
			unc += x.v
		}
	}
	t := &Table{Title: fmt.Sprintf("Categorization coverage of expense dollars %s to %s", from, to),
		Headers: []string{"Source", "Lines", "Amount", "Share"}, Right: []bool{false, true, true, true}}
	for _, x := range rs {
		t.Add(x.k, fmt.Sprint(x.n), usd(x.v), pct(x.v, total))
	}
	t.Sep()
	t.Add("total", "", usd(total), "")
	t.Notes = append(t.Notes, "Coverage (categorized / total expense $): "+pct(total-unc, total))
	return t, total, unc, rows.Err()
}

// Openings lists every opening-balance entry.
func Openings(db *sql.DB) (*Table, error) {
	rows, err := db.Query(`SELECT v.date, v.account, v.amount_minor, e.description FROM v_ledger v JOIN journal_entries e ON e.id = v.entry_id
		WHERE v.entry_type = 'opening' AND v.account <> 'Equity:Opening-Balances' ORDER BY v.account, v.line_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	t := &Table{Title: "Opening balance entries (against Equity:Opening-Balances)", Headers: []string{"Date", "Account", "Amount (Dr+)", "Basis"}, Right: []bool{false, false, true, false}}
	for rows.Next() {
		var d, a, desc string
		var v int64
		if err := rows.Scan(&d, &a, &v, &desc); err != nil {
			return nil, err
		}
		desc = strings.TrimSuffix(strings.TrimPrefix(desc, "Opening balance ("), ")")
		t.Add(d, a, usd(v), desc)
	}
	return t, rows.Err()
}

// Unmatched lists transfer-like lines parked in the unmatched clearing account.
func Unmatched(db *sql.DB, account string) (*Table, int, int64, error) {
	rows, err := db.Query(`SELECT u.date, u.description, u.amount_minor, COALESCE(u.category_source,''),
		(SELECT a.name FROM journal_lines l JOIN accounts a ON a.id = l.account_id WHERE l.entry_id = u.entry_id AND l.id <> u.line_id LIMIT 1)
		FROM v_ledger u WHERE u.account = ? ORDER BY u.date, u.line_id`, account)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	t := &Table{Title: "Unmatched transfers (" + account + ")", Headers: []string{"Date", "Source account", "Description", "Amount (into source)", "Reason"}, Right: []bool{false, false, false, true, false}}
	n := 0
	var net, gross int64
	for rows.Next() {
		var d, desc, cs, src string
		var v int64
		if err := rows.Scan(&d, &desc, &v, &cs, &src); err != nil {
			return nil, 0, 0, err
		}
		n++
		net += v
		if v < 0 {
			gross -= v
		} else {
			gross += v
		}
		t.Add(d, src, desc, usd(-v), cs)
	}
	t.Notes = append(t.Notes, fmt.Sprintf("%d rows, gross %s, net balance in clearing %s", n, usd(gross), usd(net)))
	return t, n, gross, rows.Err()
}

// Check runs all integrity checks and writes a summary; it returns the number of hard failures
// (unbalanced entries, reconciliation differences).
// reviewed: cross-source pairs a human confirmed are distinct (LoadReviewed); may be nil.
func Check(db *sql.DB, w io.Writer, format string, unmatchedAcct, inTransitAcct string, reviewed map[string]bool) (int, error) {
	failures := 0
	var entries, lines, unbalanced, drafts int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM journal_entries WHERE status='posted'), (SELECT COUNT(*) FROM v_ledger),
		(SELECT COUNT(*) FROM v_unbalanced_entries), (SELECT COUNT(*) FROM journal_entries WHERE status='draft')`).Scan(&entries, &lines, &unbalanced, &drafts); err != nil {
		return 0, err
	}
	var pending, raw int
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(pending),0) FROM raw_transactions`).Scan(&raw, &pending); err != nil {
		return 0, err
	}
	var tb int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(amount_minor),0) FROM v_ledger`).Scan(&tb); err != nil {
		return 0, err
	}
	var inTransit int64
	var openLinks int
	if err := db.QueryRow(`SELECT COALESCE(SUM(amount_minor),0) FROM v_ledger WHERE account = ?`, inTransitAcct).Scan(&inTransit); err != nil {
		return 0, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM (SELECT link_id FROM v_ledger WHERE account = ? GROUP BY link_id HAVING SUM(amount_minor) <> 0)`, inTransitAcct).Scan(&openLinks); err != nil {
		return 0, err
	}
	var uncN int
	var uncExp, uncInc int64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN account_type='expense' THEN amount_minor END),0),
		COALESCE(SUM(CASE WHEN account_type='income' THEN natural_minor END),0) FROM v_uncategorized`).Scan(&uncN, &uncExp, &uncInc); err != nil {
		return 0, err
	}
	if unbalanced > 0 || tb != 0 {
		failures++
	}
	s := &Table{Title: "finance check", Headers: []string{"Check", "Result", "Status"}}
	status := func(ok bool) string {
		if ok {
			return "OK"
		}
		return "FAIL"
	}
	s.Add("posted entries / lines", fmt.Sprintf("%d / %d", entries, lines), "")
	s.Add("unbalanced entries", fmt.Sprint(unbalanced), status(unbalanced == 0))
	s.Add("trial balance sum (all lines)", usd(tb), status(tb == 0))
	s.Add("draft entries", fmt.Sprint(drafts), status(drafts == 0))
	s.Add("staged raw rows / pending (not booked)", fmt.Sprintf("%d / %d", raw, pending), "")

	rec, bad, err := Reconcile(db)
	if err != nil {
		return 0, err
	}
	failures += bad
	s.Add("connector reconciliation differences", fmt.Sprintf("%d of %d accounts", bad, len(rec.Rows)), status(bad == 0))
	xrec, xbad, err := ReconcileExchange(db)
	if err != nil {
		return 0, err
	}
	failures += xbad
	s.Add("exchange reconciliation differences", fmt.Sprintf("%d of %d balances", xbad, len(xrec.Rows)), status(xbad == 0))
	wrec, wbad, err := ReconcileWallets(db)
	if err != nil {
		return 0, err
	}
	failures += wbad
	s.Add("wallet reconciliation differences", fmt.Sprintf("%d of %d rows", wbad, len(wrec.Rows)), status(wbad == 0))
	unassigned, err := walletUnassignedBalance(db)
	if err != nil {
		return 0, err
	}
	s.Add("self-custody not traced to a wallet", usd(unassigned), warn(unassigned == 0))
	um, n, gross, err := Unmatched(db, unmatchedAcct)
	if err != nil {
		return 0, err
	}
	s.Add("unmatched transfers", fmt.Sprintf("%d rows, gross %s", n, usd(gross)), warn(n == 0))
	itm, never, err := InTransitByMonth(db, inTransitAcct)
	if err != nil {
		return 0, err
	}
	s.Add("in-transit clearing balance", fmt.Sprintf("%s (%d open links)", usd(inTransit), openLinks), warn(inTransit == 0 && openLinks == 0))
	s.Add("in-transit residue at month-ends", fmt.Sprintf("%d month-end(s) non-zero; %d link(s) never close", len(itm.Rows), never), warn(never == 0))
	dup, ds, err := Duplicates(db, reviewed, inTransitAcct, unmatchedAcct)
	if err != nil {
		return 0, err
	}
	if ds.SameRawMulti > 0 {
		failures++
	}
	s.Add("duplicates: raw row booked by 2+ entries", fmt.Sprint(ds.SameRawMulti), status(ds.SameRawMulti == 0))
	s.Add("duplicates: cross-source same account/date/amount/counterparty", fmt.Sprint(ds.CrossSource), warn(ds.CrossSource == 0))
	s.Add("duplicates: open accrual + cash on the P&L account", fmt.Sprint(ds.OpenAccrual), warn(ds.OpenAccrual == 0))
	s.Add("duplicates: re-keyed rows still booked / superseded", fmt.Sprintf("%d / %d", ds.Rekeyed, ds.Superseded), warn(ds.Rekeyed == 0))
	s.Add("identical same-day bank rows kept (distinct ids)", fmt.Sprint(ds.IdenticalKept), "")
	s.Add("uncategorized lines", fmt.Sprintf("%d (expenses %s, income %s)", uncN, usd(uncExp), usd(uncInc)), warn(uncN == 0))
	tables := []*Table{s, rec, xrec, wrec, um}
	if len(itm.Rows) > 0 {
		tables = append(tables, itm)
	}
	if len(dup.Rows) > 0 {
		tables = append(tables, dup)
	}
	for _, t := range tables {
		if err := t.Render(w, format); err != nil {
			return 0, err
		}
	}
	return failures, nil
}

func warn(ok bool) string {
	if ok {
		return "OK"
	}
	return "REVIEW"
}
