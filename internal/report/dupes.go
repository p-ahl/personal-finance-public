package report

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Duplicate detection for `finance check` (idempotency guard). Keys are raw-row identity and
// external ids, never amount + date alone: identical same-day charges that are distinct bank
// rows (two airline tickets) are legitimate and only counted, never flagged.
//
//   hard (FAIL):   one staged raw row booked by more than one journal entry
//   REVIEW:        cross-source pairs: the same account, date, amount and counterparty booked by
//                  two different sources (connector vs accrual/opening/valuation/manual), unless
//                  the non-connector entry belongs to an accrual whose cash is already identified
//                  by a different settlement entry, or the pair is listed as reviewed;
//                  open accrual + cash: an open payable/receivable whose P&L account also received
//                  a connector line of the same amount within 90 days after recognition (the cash
//                  landed on the expense/income instead of settling the accrual);
//                  re-keyed suspects: posted staged rows missing from the newest extract of their
//                  account (inside its window) that were not superseded automatically
//   info:          identical same-day connector rows with distinct bank ids (kept)

// Reviewed is the local allowlist of cross-source pairs a human confirmed are distinct events
// ($FINANCE_DATA/config/dedupe.yaml):
//
//	reviewed:
//	  - {a: "accrual:some-id", b: "plaid:txn-id", note: "why these are two events"}
type Reviewed struct {
	Pairs []struct {
		A    string `yaml:"a"`
		B    string `yaml:"b"`
		Note string `yaml:"note"`
	} `yaml:"reviewed"`
}

// LoadReviewed reads the optional allowlist (a missing file is an empty list).
func LoadReviewed(path string) (map[string]bool, error) {
	out := map[string]bool{}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var r Reviewed
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, p := range r.Pairs {
		out[pairKey(p.A, p.B)] = true
	}
	return out, nil
}

func pairKey(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return a + "|" + b
}

// DupStats summarizes Duplicates for the check table.
type DupStats struct {
	SameRawMulti  int // raw rows booked by more than one entry (hard)
	CrossSource   int // REVIEW
	OpenAccrual   int // REVIEW
	Rekeyed       int // REVIEW
	Superseded    int // info: re-keyed rows already superseded in staging
	IdenticalKept int // info: identical same-day rows with distinct ids
}

// Duplicates lists likely double counts (see the file comment). reviewed holds pairKey()s.
func Duplicates(db *sql.DB, reviewed map[string]bool, transferAccts ...string) (*Table, DupStats, error) {
	var st DupStats
	t := &Table{Title: "Likely duplicates (keyed on raw rows / external ids; identical same-day bank rows are kept)",
		Headers: []string{"Kind", "Date", "Account", "Amount", "Entries", "Note"}}

	// 1. one raw row, several entries
	rows, err := db.Query(`SELECT raw_ref, MIN(date), MIN(account), MIN(amount_minor), group_concat(DISTINCT external_id), COUNT(DISTINCT entry_id) n
		FROM v_ledger WHERE raw_ref IS NOT NULL GROUP BY raw_ref HAVING n > 1 ORDER BY 2`)
	if err != nil {
		return nil, st, err
	}
	for rows.Next() {
		var ref, d, a, x string
		var amt int64
		var n int
		if err := rows.Scan(&ref, &d, &a, &amt, &x, &n); err != nil {
			rows.Close()
			return nil, st, err
		}
		st.SameRawMulti++
		t.Add("FAIL raw row booked twice", d, a, usd(amt), x, fmt.Sprintf("raw %s in %d entries", short(ref), n))
	}
	rows.Close()

	// 2. cross-source pairs
	excl := []string{"Equity:Opening-Balances"}
	excl = append(excl, transferAccts...)
	ph := strings.TrimSuffix(strings.Repeat("?,", len(excl)), ",")
	args := make([]any, len(excl))
	for i, e := range excl {
		args[i] = e
	}
	rows, err = db.Query(`WITH acc AS (
		  SELECT al.entry_id, al.accrual_id FROM accrual_links al WHERE al.role IN ('accrual','opening','journal'))
		SELECT a.date, a.account, a.amount_minor, COALESCE(a.counterparty,''), a.source, a.external_id, b.source, b.external_id
		FROM v_ledger a JOIN v_ledger b
		  ON b.account_id = a.account_id AND b.date = a.date AND b.amount_minor = a.amount_minor
		 AND lower(COALESCE(b.counterparty,'')) = lower(COALESCE(a.counterparty,''))
		 AND b.source <> a.source AND a.entry_id < b.entry_id
		WHERE a.amount_minor <> 0 AND a.account NOT IN (`+ph+`)
		  AND NOT EXISTS (SELECT 1 FROM acc s JOIN accrual_links st ON st.accrual_id = s.accrual_id AND st.role = 'settlement'
		                  WHERE s.entry_id IN (a.entry_id, b.entry_id) AND st.entry_id NOT IN (a.entry_id, b.entry_id))
		ORDER BY a.date, a.account`, args...)
	if err != nil {
		return nil, st, err
	}
	for rows.Next() {
		var d, a, cp, sa, xa, sb, xb string
		var amt int64
		if err := rows.Scan(&d, &a, &amt, &cp, &sa, &xa, &sb, &xb); err != nil {
			rows.Close()
			return nil, st, err
		}
		if reviewed[pairKey(xa, xb)] {
			continue
		}
		st.CrossSource++
		t.Add("REVIEW cross-source", d, a, usd(amt), xa+" + "+xb, fmt.Sprintf("%s + %s, counterparty %q", sa, sb, cp))
	}
	rows.Close()

	// 3. open accrual whose cash hit the P&L account instead of settling it
	rows, err = db.Query(`SELECT ab.accrual_key, ab.pnl_account, l.date, l.amount_minor, l.external_id
		FROM v_accrual_balances ab
		JOIN v_ledger l ON l.account = ab.pnl_account AND l.source = 'connector'
		 AND abs(l.amount_minor) = abs(ab.balance_minor) AND l.date >= ab.recognized_on
		 AND julianday(l.date) - julianday(ab.recognized_on) <= 90
		WHERE ab.kind = 'payable' AND ab.balance_minor <> 0 AND ab.pnl_account IS NOT NULL
		ORDER BY l.date`)
	if err != nil {
		return nil, st, err
	}
	for rows.Next() {
		var k, a, d, x string
		var amt int64
		if err := rows.Scan(&k, &a, &d, &amt, &x); err != nil {
			rows.Close()
			return nil, st, err
		}
		if reviewed[pairKey("accrual:"+k, x)] {
			continue
		}
		st.OpenAccrual++
		t.Add("REVIEW open accrual + cash", d, a, usd(amt), "accrual:"+k+" + "+x, "cash landed on the P&L account while the accrual is still open (settle it, or list as reviewed)")
	}
	rows.Close()

	// 4. re-keyed suspects still booked, 5. superseded (info)
	// newest extract per account = the last run (by as-of, then id) that contained any of its rows
	if err := db.QueryRow(`WITH rr AS (SELECT r.connector_txn_id id, r.connector_account_id acct, r.txn_date d, r.pending p, r.superseded_by sb,
		    COALESCE(ir.as_of,'') || printf('%010d', COALESCE(r.last_run_id,0)) ord
		  FROM raw_transactions r LEFT JOIN import_runs ir ON ir.id = r.last_run_id),
		  lr AS (SELECT acct, MAX(ord) ord FROM rr GROUP BY acct),
		  win AS (SELECT rr.acct, MIN(rr.d) d FROM rr JOIN lr ON lr.acct = rr.acct AND rr.ord = lr.ord WHERE rr.p = 0 GROUP BY rr.acct)
		SELECT COUNT(*) FROM rr JOIN lr ON lr.acct = rr.acct JOIN win ON win.acct = rr.acct
		WHERE rr.p = 0 AND rr.sb IS NULL AND rr.ord < lr.ord AND rr.d >= win.d`).Scan(&st.Rekeyed); err != nil {
		return nil, st, err
	}
	if st.Rekeyed > 0 {
		t.Add("REVIEW re-keyed suspects", "", "", "", fmt.Sprint(st.Rekeyed), "posted staged rows missing from the newest extract of their account (removed or re-issued upstream); still booked")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM raw_transactions WHERE superseded_by IS NOT NULL`).Scan(&st.Superseded); err != nil {
		return nil, st, err
	}

	// 6. identical same-day connector rows with distinct ids (info)
	rows, err = db.Query(`SELECT connector_account_id, txn_date, amount, raw_json FROM raw_transactions WHERE pending = 0 AND superseded_by IS NULL`)
	if err != nil {
		return nil, st, err
	}
	groups := map[string]int{}
	for rows.Next() {
		var a, d, amt, raw string
		if err := rows.Scan(&a, &d, &amt, &raw); err != nil {
			rows.Close()
			return nil, st, err
		}
		groups[a+"|"+d+"|"+amt+"|"+nameOf(raw)]++
	}
	rows.Close()
	var ks []string
	for k, n := range groups {
		if n > 1 {
			ks = append(ks, k)
			st.IdenticalKept += n - 1
		}
	}
	sort.Strings(ks)
	if len(ks) > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d identical same-day bank rows with distinct bank transaction ids kept as separate events (%d groups); not flagged", st.IdenticalKept, len(ks)))
	}
	if st.Superseded > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d staged rows superseded as re-keyed upstream (not booked)", st.Superseded))
	}
	return t, st, nil
}

func nameOf(raw string) string {
	// cheap extraction of "name" without a full decode of every row's other fields
	i := strings.Index(raw, `"name":"`)
	if i < 0 {
		return ""
	}
	s := raw[i+8:]
	if j := strings.Index(s, `"`); j >= 0 {
		return strings.ToLower(strings.Join(strings.Fields(s[:j]), " "))
	}
	return ""
}

func short(s string) string {
	if len(s) > 12 {
		return s[:8] + "…"
	}
	return s
}

// InTransitByMonth lists month-ends where the in-transit clearing account is not zero, with the
// number of links still open at that month-end and how many of them close later (a transfer that
// crosses the month-end) versus never (an unmatched or doubled leg).
func InTransitByMonth(db *sql.DB, acct string) (*Table, int, error) {
	t := &Table{Title: "In-transit clearing by month-end (non-zero only)", Headers: []string{"Month-end", "Balance", "Open links", "Close later", "Never close"}}
	rows, err := db.Query(`WITH m AS (SELECT DISTINCT date(substr(date,1,7)||'-01','+1 month','-1 day') me FROM v_ledger WHERE account = ?),
		  link AS (SELECT link_id, SUM(amount_minor) tot FROM v_ledger WHERE account = ? GROUP BY link_id)
		SELECT m.me, SUM(l.amount_minor),
		  COUNT(DISTINCT CASE WHEN x.s <> 0 THEN x.link_id END),
		  COUNT(DISTINCT CASE WHEN x.s <> 0 AND link.tot = 0 THEN x.link_id END),
		  COUNT(DISTINCT CASE WHEN x.s <> 0 AND link.tot <> 0 THEN x.link_id END)
		FROM m JOIN v_ledger l ON l.account = ? AND l.date <= m.me
		LEFT JOIN (SELECT m2.me, v.link_id, SUM(v.amount_minor) s FROM m m2 JOIN v_ledger v ON v.account = ? AND v.date <= m2.me GROUP BY m2.me, v.link_id) x
		  ON x.me = m.me AND x.link_id = l.link_id
		LEFT JOIN link ON link.link_id = l.link_id
		GROUP BY m.me ORDER BY m.me`, acct, acct, acct, acct)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	never := 0
	for rows.Next() {
		var me string
		var bal int64
		var open, later, nv int
		if err := rows.Scan(&me, &bal, &open, &later, &nv); err != nil {
			return nil, 0, err
		}
		if bal == 0 && open == 0 {
			continue
		}
		never += nv
		t.Add(me, usd(bal), fmt.Sprint(open), fmt.Sprint(later), fmt.Sprint(nv))
	}
	if len(t.Rows) == 0 {
		t.Notes = append(t.Notes, "zero at every month-end")
	}
	return t, never, rows.Err()
}
