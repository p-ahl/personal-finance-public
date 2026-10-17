package ledger

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/p-ahl/personal-finance/internal/books"
)

type SyncStats struct {
	Inserted, Updated, Unchanged, Deleted int
	Accounts                              int
}

var managedSources = []string{"connector", "opening", "valuation", books.AccrualSource, books.PendingSource}

// Sync writes a build result into the journal idempotently, keyed by external_id + content hash.
// Builder-managed entries (connector/opening/valuation) that are no longer produced are removed;
// manual entries are never touched.
func Sync(tx *sql.Tx, res *books.Result, cfg *books.Config, runID int64) (SyncStats, error) {
	var st SyncStats
	for _, c := range sortedCommodities(res.Commodities) {
		if _, err := tx.Exec(`INSERT INTO commodities(code, scale, kind, name) VALUES (?,?,?,?)
			ON CONFLICT(code) DO UPDATE SET kind=excluded.kind, name=COALESCE(excluded.name, commodities.name)`,
			c.Code, c.Scale, c.Kind, nullStr(c.Name)); err != nil {
			return st, err
		}
	}
	acctID, err := syncAccounts(tx, res, cfg)
	if err != nil {
		return st, err
	}
	st.Accounts = len(acctID)

	cpID := map[string]int64{}
	counterparty := func(name string) (any, error) {
		if name == "" {
			return nil, nil
		}
		if id, ok := cpID[name]; ok {
			return id, nil
		}
		kind := "merchant"
		if name == "self" {
			kind = "self"
		}
		if _, err := tx.Exec(`INSERT INTO counterparties(name, kind) VALUES (?,?) ON CONFLICT(name) DO NOTHING`, name, kind); err != nil {
			return nil, err
		}
		var id int64
		if err := tx.QueryRow(`SELECT id FROM counterparties WHERE name = ?`, name).Scan(&id); err != nil {
			return nil, err
		}
		cpID[name] = id
		return id, nil
	}
	dimID := func(spec string) (int64, error) {
		kind, code, ok := strings.Cut(spec, ":")
		if !ok {
			return 0, fmt.Errorf("dimension %q must be kind:code", spec)
		}
		if _, err := tx.Exec(`INSERT INTO dimensions(kind, code, name) VALUES (?,?,?) ON CONFLICT(kind, code) DO NOTHING`, kind, code, code); err != nil {
			return 0, err
		}
		var id int64
		err := tx.QueryRow(`SELECT id FROM dimensions WHERE kind = ? AND code = ?`, kind, code).Scan(&id)
		return id, err
	}

	type existing struct {
		id   int64
		hash string
	}
	have := map[string]existing{}
	rows, err := tx.Query(`SELECT id, external_id, COALESCE(content_hash,'') FROM journal_entries WHERE source IN ('connector','opening','valuation','accrual','pending')`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var e existing
		var xid string
		if err := rows.Scan(&e.id, &xid, &e.hash); err != nil {
			rows.Close()
			return st, err
		}
		have[xid] = e
	}
	rows.Close()

	seen := map[string]bool{}
	for _, e := range res.Entries {
		if seen[e.ExternalID] {
			return st, fmt.Errorf("duplicate external_id %s in build", e.ExternalID)
		}
		seen[e.ExternalID] = true
		h := e.Hash()
		cp, err := counterparty(e.Counterparty)
		if err != nil {
			return st, err
		}
		var id int64
		if old, ok := have[e.ExternalID]; ok {
			if old.hash == h {
				st.Unchanged++
				continue
			}
			id = old.id
			if _, err := tx.Exec(`UPDATE journal_entries SET status='draft' WHERE id=?`, id); err != nil {
				return st, err
			}
			if _, err := tx.Exec(`DELETE FROM journal_lines WHERE entry_id=?`, id); err != nil {
				return st, err
			}
			if _, err := tx.Exec(`UPDATE journal_entries SET entry_date=?, description=?, counterparty_id=?, entry_type=?, source=?, link_id=?, content_hash=?, import_run_id=?,
				service_start=?, service_end=? WHERE id=?`,
				e.Date, e.Description, cp, e.Type, e.Source, nullStr(e.LinkID), h, runID, nullStr(e.ServiceStart), nullStr(e.ServiceEnd), id); err != nil {
				return st, err
			}
			st.Updated++
		} else {
			r, err := tx.Exec(`INSERT INTO journal_entries(entry_date, description, counterparty_id, entry_type, source, external_id, link_id, content_hash, import_run_id,
				service_start, service_end) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, e.Date, e.Description, cp, e.Type, e.Source, e.ExternalID, nullStr(e.LinkID), h, runID,
				nullStr(e.ServiceStart), nullStr(e.ServiceEnd))
			if err != nil {
				return st, err
			}
			id, _ = r.LastInsertId()
			st.Inserted++
		}
		for i, l := range e.Lines {
			aid, ok := acctID[l.Account]
			if !ok {
				return st, fmt.Errorf("entry %s: unknown account %s", e.ExternalID, l.Account)
			}
			lcp, err := counterparty(l.Counterparty)
			if err != nil {
				return st, err
			}
			var qty, com any
			if l.Quantity != nil {
				qty, com = *l.Quantity, l.Commodity
			}
			r, err := tx.Exec(`INSERT INTO journal_lines(entry_id, line_no, account_id, amount, currency, quantity, commodity, unit_price, counterparty_id, memo, category_source, raw_ref)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, id, i+1, aid, l.Amount, l.Currency, qty, com, nullStr(l.UnitPrice), lcp, nullStr(l.Memo), nullStr(l.CategorySource), nullStr(l.RawRef))
			if err != nil {
				return st, fmt.Errorf("entry %s line %d: %w", e.ExternalID, i+1, err)
			}
			lid, _ := r.LastInsertId()
			for _, d := range l.Dimensions {
				did, err := dimID(d)
				if err != nil {
					return st, err
				}
				if _, err := tx.Exec(`INSERT INTO line_dimensions(line_id, dimension_id) VALUES (?,?)`, lid, did); err != nil {
					return st, err
				}
			}
		}
		if _, err := tx.Exec(`UPDATE journal_entries SET status='posted', posted_at=strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE id=?`, id); err != nil {
			return st, fmt.Errorf("posting %s: %w", e.ExternalID, err)
		}
	}
	for xid, old := range have {
		if seen[xid] {
			continue
		}
		if _, err := tx.Exec(`UPDATE journal_entries SET status='draft' WHERE id=?`, old.id); err != nil {
			return st, err
		}
		if _, err := tx.Exec(`DELETE FROM journal_entries WHERE id=?`, old.id); err != nil {
			return st, err
		}
		st.Deleted++
	}

	if err := syncAccruals(tx, res, acctID, counterparty); err != nil {
		return st, fmt.Errorf("accruals: %w", err)
	}

	for _, p := range res.Prices {
		if _, err := tx.Exec(`INSERT INTO prices(commodity, price_date, price, currency, source) VALUES (?,?,?,?,?)
			ON CONFLICT(commodity, price_date, source) DO UPDATE SET price=excluded.price`, p.Commodity, p.Date, p.Price, books.USD, p.Source); err != nil {
			return st, err
		}
	}
	for _, l := range res.Lots {
		if _, err := tx.Exec(`INSERT INTO lots(account_id, commodity, acquired_on, quantity, cost_total, is_aggregate, source, as_of, note)
			VALUES (?,?,NULL,?,?,1,'connector-holdings',?,'position-level cost basis from connector; no tax-lot detail available')
			ON CONFLICT(account_id, commodity, source, as_of) DO UPDATE SET quantity=excluded.quantity, cost_total=excluded.cost_total`,
			acctID[l.Account], l.Commodity, l.Quantity, l.Cost, l.AsOf); err != nil {
			return st, err
		}
	}
	// connector assertions are derived from the newest balance snapshot per account; older
	// snapshots stay in balance_snapshots, but their assertions are replaced (a balance from an
	// earlier extract can legitimately miss rows that posted later with an earlier date).
	// exchange assertions (source = exchange name) are rebuilt the same way from the newest pull
	if _, err := tx.Exec(`DELETE FROM balance_assertions WHERE source='connector' OR source='wallets' OR source IN (SELECT DISTINCT exchange FROM exchange_balance_snapshots)`); err != nil {
		return st, err
	}
	for _, a := range res.Assertions {
		src := a.Source
		if src == "" {
			src = "connector"
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO balance_assertions(account_id, as_of, amount, currency, source) VALUES (?,?,?,?,?)`,
			acctID[a.Account], a.AsOf, a.Amount, books.USD, src); err != nil {
			return st, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM categorization_rules`); err != nil {
		return st, err
	}
	for i, r := range cfg.Rules.Rules {
		b, _ := json.Marshal(r)
		if _, err := tx.Exec(`INSERT OR REPLACE INTO categorization_rules(priority, name, origin, spec_json) VALUES (?,?,?,?)`, i+1, r.Name, r.Origin, string(b)); err != nil {
			return st, err
		}
	}
	keys := make([]string, 0, len(cfg.Rules.PlaidFallback))
	for k := range cfg.Rules.PlaidFallback {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		b, _ := json.Marshal(map[string]string{"plaid": k, "account": cfg.Rules.PlaidFallback[k]})
		if _, err := tx.Exec(`INSERT INTO categorization_rules(priority, name, origin, spec_json) VALUES (?,?,?,?)`, 10000+i, k, "plaid_fallback", string(b)); err != nil {
			return st, err
		}
	}
	return st, nil
}

func syncAccounts(tx *sql.Tx, res *books.Result, cfg *books.Config) (map[string]int64, error) {
	all := map[string]bool{}
	for n := range res.Accounts {
		segs := strings.Split(n, ":")
		for i := 1; i <= len(segs); i++ {
			all[strings.Join(segs[:i], ":")] = true
		}
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		di, dj := strings.Count(names[i], ":"), strings.Count(names[j], ":")
		if di != dj {
			return di < dj
		}
		return names[i] < names[j]
	})
	ids := map[string]int64{}
	for _, n := range names {
		info := res.Accounts[n]
		placeholder := info == nil
		var attrs books.Attrs
		var ext, mask, inst string
		if info != nil {
			attrs, ext, mask, inst = info.Attrs, info.ExternalID, info.Mask, info.Institution
		} else {
			a, err := cfg.Resolve(n)
			if err != nil {
				return nil, err
			}
			attrs = a
		}
		tracking := attrs.Tracking
		for _, m := range cfg.Map.Accounts {
			if m.Name == n {
				tracking = m.Tracking
				if tracking == "valuation" {
					tracking = "connector"
				}
			}
		}
		if tracking == "" {
			tracking = "none"
		}
		segs := strings.Split(n, ":")
		var parent any
		if len(segs) > 1 {
			parent = ids[strings.Join(segs[:len(segs)-1], ":")]
		}
		lv := func(i int) any {
			if i < len(segs) {
				return segs[i]
			}
			return nil
		}
		var instID any
		if inst != "" {
			var id int64
			if err := tx.QueryRow(`SELECT id FROM institutions WHERE name = ?`, inst).Scan(&id); err == nil {
				instID = id
			}
		}
		_, err := tx.Exec(`INSERT INTO accounts(name, parent_id, depth, level1, level2, level3, account_type, normal_balance, statement, report_line,
				cash_flow_class, institution_id, external_id, mask, is_placeholder, valuation_only, tracking, description)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET parent_id=excluded.parent_id, depth=excluded.depth, level1=excluded.level1, level2=excluded.level2,
				level3=excluded.level3, account_type=excluded.account_type, normal_balance=excluded.normal_balance, statement=excluded.statement,
				report_line=excluded.report_line, cash_flow_class=excluded.cash_flow_class, institution_id=COALESCE(excluded.institution_id, accounts.institution_id),
				external_id=COALESCE(excluded.external_id, accounts.external_id), mask=COALESCE(excluded.mask, accounts.mask),
				is_placeholder=excluded.is_placeholder, valuation_only=excluded.valuation_only, tracking=excluded.tracking,
				description=COALESCE(excluded.description, accounts.description)`,
			n, parent, len(segs), segs[0], lv(1), lv(2), attrs.Type, attrs.Normal, attrs.Statement, attrs.ReportLine, attrs.CashFlow,
			instID, nullStr(ext), nullStr(mask), b2i(placeholder), b2i(attrs.ValuationOnly || isValuation(cfg, n)), tracking, nullStr(attrs.Description))
		if err != nil {
			return nil, fmt.Errorf("account %s: %w", n, err)
		}
		var id int64
		if err := tx.QueryRow(`SELECT id FROM accounts WHERE name = ?`, n).Scan(&id); err != nil {
			return nil, err
		}
		ids[n] = id
	}
	return ids, nil
}

func isValuation(cfg *books.Config, name string) bool {
	for _, m := range cfg.Map.Accounts {
		if m.Name == name {
			return m.ValuationOnly || m.Tracking == "valuation"
		}
	}
	return false
}

func sortedCommodities(m map[string]books.Commodity) []books.Commodity {
	out := make([]books.Commodity, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// syncAccruals materializes documents, accrual specs and their entry links. All three are fully
// derived from config + build output, so managed rows are rewritten on every run.
func syncAccruals(tx *sql.Tx, res *books.Result, acctID map[string]int64, counterparty func(string) (any, error)) error {
	for _, q := range []string{`DELETE FROM accrual_links`, `DELETE FROM accruals`, `DELETE FROM entry_documents WHERE managed = 1`} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	keep := map[string]bool{}
	for _, d := range res.Documents {
		keep[d.ID] = true
		cp, err := counterparty(d.Counterparty)
		if err != nil {
			return err
		}
		var amt any
		if d.AmountMinor != nil {
			amt = *d.AmountMinor
		}
		if _, err := tx.Exec(`INSERT INTO documents(doc_key, kind, source, external_ref, doc_date, counterparty_id, amount_minor, service_start, service_end, title, local_path, notes, managed)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,1)
			ON CONFLICT(doc_key) DO UPDATE SET kind=excluded.kind, source=excluded.source, external_ref=excluded.external_ref, doc_date=excluded.doc_date,
				counterparty_id=excluded.counterparty_id, amount_minor=excluded.amount_minor, service_start=excluded.service_start, service_end=excluded.service_end,
				title=excluded.title, local_path=excluded.local_path, notes=excluded.notes, managed=1`,
			d.ID, d.Kind, d.Source, nullStr(d.ExternalRef), nullStr(d.DocDate), cp, amt, nullStr(d.ServiceStart), nullStr(d.ServiceEnd),
			nullStr(d.Title), nullStr(d.LocalPath), nullStr(d.Notes)); err != nil {
			return fmt.Errorf("document %s: %w", d.ID, err)
		}
	}
	rows, err := tx.Query(`SELECT doc_key FROM documents WHERE managed = 1`)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		if !keep[k] {
			stale = append(stale, k)
		}
	}
	rows.Close()
	for _, k := range stale {
		if _, err := tx.Exec(`DELETE FROM documents WHERE doc_key = ?`, k); err != nil {
			return err
		}
	}
	acc := func(name string) any {
		if name == "" {
			return nil
		}
		if id, ok := acctID[name]; ok {
			return id
		}
		return nil
	}
	accID := map[string]int64{}
	for _, a := range res.Accruals {
		cp, err := counterparty(a.Counterparty)
		if err != nil {
			return err
		}
		r, err := tx.Exec(`INSERT INTO accruals(accrual_key, kind, description, counterparty_id, pnl_account_id, balance_account_id, service_start, service_end, amount_minor, basis, notes)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, a.ID, a.Kind, nullStr(a.Description), cp, acc(a.Account), acc(a.Balance),
			nullStr(a.ServiceStart), nullStr(a.ServiceEnd), a.AmountMinor, nullStr(a.Basis), nullStr(a.Notes))
		if err != nil {
			return fmt.Errorf("accrual %s: %w", a.ID, err)
		}
		accID[a.ID], _ = r.LastInsertId()
	}
	entryID := func(xid string) (int64, error) {
		var id int64
		err := tx.QueryRow(`SELECT id FROM journal_entries WHERE external_id = ?`, xid).Scan(&id)
		return id, err
	}
	for _, l := range res.AccrualLinks {
		eid, err := entryID(l.ExternalID)
		if err != nil {
			return fmt.Errorf("link %s -> %s: %w", l.AccrualID, l.ExternalID, err)
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO accrual_links(accrual_id, entry_id, role) VALUES (?,?,?)`, accID[l.AccrualID], eid, l.Role); err != nil {
			return err
		}
	}
	for _, d := range res.EntryDocs {
		eid, err := entryID(d.ExternalID)
		if err != nil {
			return fmt.Errorf("doc link %s -> %s: %w", d.DocumentID, d.ExternalID, err)
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO entry_documents(entry_id, document_id, role, managed)
			SELECT ?, id, ?, 1 FROM documents WHERE doc_key = ?`, eid, d.Role, d.DocumentID); err != nil {
			return err
		}
	}
	return nil
}
