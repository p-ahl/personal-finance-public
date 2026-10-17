package ledger

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
)

type StageStats struct {
	RawNew, RawChanged, RawUnchanged int
	Pending                          int
	Snapshots, Holdings              int
	// Rows already staged inside this extract's date span (per kind: earliest row date in the
	// extract .. as-of) that the extract no longer contains. Staging keeps them (rows are
	// never deleted); posted ones need review (removed or re-issued by the institution),
	// pending ones were usually superseded by a posted row with a new id.
	UnseenPosted, UnseenPending int
	// Staged posted rows replaced by an identical row under a new id in this extract (re-keyed
	// upstream); kept in staging with superseded_by set and no longer booked.
	Superseded int
}

// BeginRun records an import run and returns its id.
func BeginRun(tx *sql.Tx, source, rawDir, asOf string) (int64, error) {
	r, err := tx.Exec(`INSERT INTO import_runs(source, raw_dir, as_of) VALUES (?,?,?)`, source, rawDir, asOf)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// FinishRun stores stats and marks the run ok.
func FinishRun(tx *sql.Tx, runID int64, stats any) error {
	b, _ := json.Marshal(stats)
	_, err := tx.Exec(`UPDATE import_runs SET finished_at = strftime('%Y-%m-%dT%H:%M:%SZ','now'), stats_json = ?, status = 'ok' WHERE id = ?`, string(b), runID)
	return err
}

// StageExtract copies an extract verbatim into the staging tables. Idempotent: unchanged rows
// only get their last_run_id bumped.
func StageExtract(tx *sql.Tx, ex *connector.Extract, cfg *books.Config, runID int64) (StageStats, error) {
	var st StageStats
	if ex.AsOf == "" {
		return st, fmt.Errorf("extract has no as-of date (name the folder YYYY-MM-DD or pass --as-of)")
	}
	connByID := map[string]connector.Connection{}
	for _, c := range ex.Connections {
		connByID[c.ID] = c
	}
	for _, in := range cfg.Map.Institutions {
		c := connByID[in.ConnectorID]
		if _, err := tx.Exec(`INSERT INTO institutions(name, connector_id, connector_institution_id, display_name, provider)
			VALUES (?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET connector_id=excluded.connector_id,
			connector_institution_id=excluded.connector_institution_id, display_name=excluded.display_name, provider=excluded.provider`,
			in.Name, nullStr(in.ConnectorID), nullStr(c.InstitutionID), firstNonEmpty(in.Display, c.InstitutionName), nullStr(c.Provider)); err != nil {
			return st, err
		}
	}
	upsertRaw := func(id, kind, acct, date, amount string, pending bool, raw string) error {
		var old, lastAsOf string
		err := tx.QueryRow(`SELECT r.raw_json, COALESCE(ir.as_of, '') FROM raw_transactions r LEFT JOIN import_runs ir ON ir.id = r.last_run_id
			WHERE r.connector_txn_id = ?`, id).Scan(&old, &lastAsOf)
		switch {
		case err == nil && lastAsOf != "" && ex.AsOf < lastAsOf:
			// re-import of an OLDER extract: never let stale content (e.g. the pending version of
			// a row that has since posted) overwrite what a newer extract staged
			st.RawUnchanged++
			return nil
		case err == sql.ErrNoRows:
			st.RawNew++
			_, err = tx.Exec(`INSERT INTO raw_transactions(connector_txn_id, kind, connector_account_id, txn_date, amount, pending, raw_json, first_run_id, last_run_id)
				VALUES (?,?,?,?,?,?,?,?,?)`, id, kind, acct, date, amount, b2i(pending), raw, runID, runID)
			return err
		case err != nil:
			return err
		case old == raw:
			st.RawUnchanged++
			_, err = tx.Exec(`UPDATE raw_transactions SET last_run_id = ? WHERE connector_txn_id = ?`, runID, id)
			return err
		default:
			st.RawChanged++
			_, err = tx.Exec(`UPDATE raw_transactions SET kind=?, connector_account_id=?, txn_date=?, amount=?, pending=?, raw_json=?, last_run_id=? WHERE connector_txn_id=?`,
				kind, acct, date, amount, b2i(pending), raw, runID, id)
			return err
		}
	}
	for _, t := range ex.Txns {
		if t.Pending {
			st.Pending++
		}
		if err := upsertRaw(t.ID, t.Kind, t.AccountID, t.Date, t.Amount, t.Pending, connector.RawJSON(t.Raw)); err != nil {
			return st, err
		}
	}
	for _, p := range ex.PortfolioTxns {
		if err := upsertRaw(p.ID, "portfolio", p.AccountID, p.Date, p.Amount, false, connector.RawJSON(p.Raw)); err != nil {
			return st, err
		}
	}
	n, err := supersedeRekeyed(tx, ex, runID)
	if err != nil {
		return st, err
	}
	st.Superseded = n
	span := map[string]string{} // kind -> earliest date in this extract
	for _, t := range ex.Txns {
		if d, ok := span[t.Kind]; !ok || t.Date < d {
			span[t.Kind] = t.Date
		}
	}
	for _, p := range ex.PortfolioTxns {
		if d, ok := span["portfolio"]; !ok || p.Date < d {
			span["portfolio"] = p.Date
		}
	}
	for kind, from := range span {
		var posted, pending int
		if err := tx.QueryRow(`SELECT COALESCE(SUM(pending = 0),0), COALESCE(SUM(pending = 1),0) FROM raw_transactions
			WHERE kind = ? AND txn_date BETWEEN ? AND ? AND COALESCE(last_run_id, -1) <> ? AND superseded_by IS NULL
			AND COALESCE((SELECT ir.as_of FROM import_runs ir WHERE ir.id = last_run_id), '') <= ?`, kind, from, ex.AsOf, runID, ex.AsOf).Scan(&posted, &pending); err != nil {
			return st, err
		}
		st.UnseenPosted += posted
		st.UnseenPending += pending
	}
	for _, a := range ex.Accounts {
		cur, avail := "", ""
		if a.CurrentBalance != nil {
			cur = *a.CurrentBalance
		}
		if a.AvailableBalance != nil {
			avail = *a.AvailableBalance
		}
		if _, err := tx.Exec(`INSERT INTO balance_snapshots(connector_account_id, as_of, current_balance, available_balance, currency, raw_json, import_run_id)
			VALUES (?,?,?,?,?,?,?) ON CONFLICT(connector_account_id, as_of) DO UPDATE SET current_balance=excluded.current_balance,
			available_balance=excluded.available_balance, currency=excluded.currency, raw_json=excluded.raw_json`,
			a.ID, ex.AsOf, nullStr(cur), nullStr(avail), a.Currency, string(a.Raw), runID); err != nil {
			return st, err
		}
		st.Snapshots++
	}
	for _, h := range ex.Holdings {
		if _, err := tx.Exec(`INSERT INTO raw_holdings(as_of, connector_holding_id, connector_account_id, raw_json, import_run_id)
			VALUES (?,?,?,?,?) ON CONFLICT(as_of, connector_holding_id) DO UPDATE SET raw_json=excluded.raw_json`,
			ex.AsOf, h.ID, h.AccountID, connector.RawJSON(h.Raw), runID); err != nil {
			return st, err
		}
		st.Holdings++
	}
	return st, nil
}

// LoadInput rebuilds the builder input from staging: latest balance snapshot + every raw row.
func LoadInput(tx *sql.Tx) (books.Input, error) {
	var in books.Input
	if err := tx.QueryRow(`SELECT COALESCE(MAX(as_of),'') FROM balance_snapshots`).Scan(&in.AsOf); err != nil {
		return in, err
	}
	if in.AsOf == "" {
		return in, fmt.Errorf("staging is empty: run `finance import connector --raw <dir>` first")
	}
	// newest snapshot per account: an extract that misses an account (closed, or a connection
	// that failed that night) keeps the account's last known balance instead of dropping it
	rows, err := tx.Query(`SELECT s.connector_account_id, s.as_of, s.raw_json FROM balance_snapshots s
		WHERE s.as_of = (SELECT MAX(s2.as_of) FROM balance_snapshots s2 WHERE s2.connector_account_id = s.connector_account_id)
		ORDER BY s.connector_account_id`)
	if err != nil {
		return in, err
	}
	in.SnapshotAsOf = map[string]string{}
	for rows.Next() {
		var id, asOf, raw string
		if err := rows.Scan(&id, &asOf, &raw); err != nil {
			rows.Close()
			return in, err
		}
		in.SnapshotAsOf[id] = asOf
		var a connector.Account
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			rows.Close()
			return in, err
		}
		a.Raw = json.RawMessage(raw)
		in.Accounts = append(in.Accounts, a)
	}
	rows.Close()

	rows, err = tx.Query(`SELECT kind, raw_json FROM raw_transactions WHERE superseded_by IS NULL ORDER BY txn_date, connector_txn_id`)
	if err != nil {
		return in, err
	}
	for rows.Next() {
		var kind, raw string
		if err := rows.Scan(&kind, &raw); err != nil {
			rows.Close()
			return in, err
		}
		m := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			rows.Close()
			return in, err
		}
		if kind == "portfolio" {
			in.PortfolioTxns = append(in.PortfolioTxns, connector.PortfolioFromRow(m))
		} else {
			in.Txns = append(in.Txns, connector.TxnFromRow(kind, m))
		}
	}
	rows.Close()

	// holdings from the newest extract that pulled holdings (a nightly extract without a
	// holdings file must not zero the positions)
	rows, err = tx.Query(`SELECT raw_json FROM raw_holdings WHERE as_of = (SELECT MAX(as_of) FROM raw_holdings) ORDER BY connector_holding_id`)
	if err != nil {
		return in, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return in, err
		}
		m := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return in, err
		}
		in.Holdings = append(in.Holdings, connector.HoldingFromRow(m))
	}
	return in, rows.Err()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// supersedeRekeyed marks staged posted rows that the institution re-issued under a new id (see
// migration 0006), so exactly one copy of each event is booked whatever order extracts are
// imported in:
//
//  1. a superseded row that is in this extract again is live again; if the row that replaced it
//     is NOT in this extract, the roles swap (that one becomes the superseded copy);
//  2. a staged posted row of an account covered by this extract (earliest row date of that
//     account in the extract .. as-of) that is not in the extract is superseded by an identical
//     row (same account, date, amount, normalized name/merchant/description) that was staged for
//     the first time in this run under a different id and that no other row claims yet.
//
// Identical rows that are all present in an extract (two same-day airline tickets) are never
// merged, and re-running the same import changes nothing.
func supersedeRekeyed(tx *sql.Tx, ex *connector.Extract, runID int64) (int, error) {
	seen := map[string]bool{}
	from := map[string]string{} // account -> earliest posted date in this extract
	for _, t := range ex.Txns {
		seen[t.ID] = true
		if d, ok := from[t.AccountID]; !ok || t.Date < d {
			from[t.AccountID] = t.Date
		}
	}
	// 1. rows back in this extract. An extract older than the one that staged the supersessor is
	// stale and changes nothing (re-importing yesterday's files is a no-op).
	type sup struct{ id, by string }
	var back []sup
	rows, err := tx.Query(`SELECT r.connector_txn_id, r.superseded_by FROM raw_transactions r
		LEFT JOIN raw_transactions s ON s.connector_txn_id = r.superseded_by
		LEFT JOIN import_runs ir ON ir.id = s.last_run_id
		WHERE r.superseded_by IS NOT NULL AND COALESCE(ir.as_of, '') <= ? ORDER BY r.connector_txn_id`, ex.AsOf)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var x sup
		if err := rows.Scan(&x.id, &x.by); err != nil {
			rows.Close()
			return 0, err
		}
		if seen[x.id] {
			back = append(back, x)
		}
	}
	rows.Close()
	for _, x := range back {
		if _, err := tx.Exec(`UPDATE raw_transactions SET superseded_by = NULL WHERE connector_txn_id = ?`, x.id); err != nil {
			return 0, err
		}
		if !seen[x.by] {
			if _, err := tx.Exec(`UPDATE raw_transactions SET superseded_by = ? WHERE connector_txn_id = ? AND superseded_by IS NULL`, x.id, x.by); err != nil {
				return 0, err
			}
		}
	}

	key := func(raw string) (string, error) {
		m := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return "", err
		}
		acct := m["account_id"]
		if acct == "" {
			acct = m["liability_id"]
		}
		return strings.Join([]string{acct, m["date"], connector.NormAmount(m["amount"]), connector.NormText(m["name"]),
			connector.NormText(m["merchant_name"]), connector.NormText(m["description"])}, "\x1f"), nil
	}
	claimed := map[string]bool{}
	rows, err = tx.Query(`SELECT superseded_by FROM raw_transactions WHERE superseded_by IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return 0, err
		}
		claimed[s] = true
	}
	rows.Close()
	// 2. ids staged for the first time in this run, by identity key
	fresh := map[string][]string{}
	rows, err = tx.Query(`SELECT connector_txn_id, raw_json FROM raw_transactions
		WHERE first_run_id = ? AND pending = 0 AND superseded_by IS NULL AND kind IN ('cash','liability') ORDER BY connector_txn_id`, runID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		if claimed[id] {
			continue
		}
		k, err := key(raw)
		if err != nil {
			rows.Close()
			return 0, err
		}
		fresh[k] = append(fresh[k], id)
	}
	rows.Close()
	n := 0
	if len(fresh) == 0 {
		return n, nil
	}
	type cand struct{ id, acct, date, raw string }
	var cands []cand
	rows, err = tx.Query(`SELECT connector_txn_id, connector_account_id, txn_date, raw_json FROM raw_transactions
		WHERE pending = 0 AND superseded_by IS NULL AND kind IN ('cash','liability') ORDER BY connector_txn_id`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.acct, &c.date, &c.raw); err != nil {
			rows.Close()
			return 0, err
		}
		if seen[c.id] {
			continue
		}
		if f, ok := from[c.acct]; ok && c.date >= f && c.date <= ex.AsOf {
			cands = append(cands, c)
		}
	}
	rows.Close()
	for _, c := range cands {
		k, err := key(c.raw)
		if err != nil {
			return 0, err
		}
		ids := fresh[k]
		if len(ids) == 0 {
			continue
		}
		fresh[k] = ids[1:]
		if _, err := tx.Exec(`UPDATE raw_transactions SET superseded_by = ? WHERE connector_txn_id = ?`, ids[0], c.id); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}
