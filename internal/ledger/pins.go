package ledger

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/p-ahl/personal-finance/internal/books"
)

// trackedOpenings maps the opening external id of every connector account whose opening is
// pinned to the account name: transaction-tracked accounts and (since 0008) valuation-only and
// holdings-tracked investment accounts.
func trackedOpenings(cfg *books.Config) map[string]string {
	out := map[string]string{}
	for _, m := range cfg.Map.Accounts {
		out["opening:"+m.ConnectorID] = m.Name
	}
	return out
}

// holdingsOpenings: opening external ids of holdings-tracked accounts (pinned per position).
func holdingsOpenings(cfg *books.Config) map[string]bool {
	out := map[string]bool{}
	for _, m := range cfg.Map.Accounts {
		if m.Tracking == "holdings" {
			out["opening:"+m.ConnectorID] = true
		}
	}
	return out
}

func encodePinLines(ls []books.PinLine) (string, error) {
	if len(ls) == 0 {
		return "", nil
	}
	b, err := json.Marshal(ls)
	return string(b), err
}

// loadPins returns the stored pins. A tracked account without a pin whose opening entry is
// already in the journal is pinned at that stored amount first (the opening established by
// earlier runs), so upgrading an existing database never re-derives an opening.
func loadPins(tx *sql.Tx, cfg *books.Config, asOf string) (map[string]books.OpeningPin, error) {
	tracked := trackedOpenings(cfg)
	holdings := holdingsOpenings(cfg)
	rows, err := tx.Query(`SELECT e.external_id, COALESCE(l.commodity,''), COALESCE(l.quantity,0), l.amount, COALESCE(l.memo,'')
		FROM journal_entries e
		JOIN journal_lines l ON l.entry_id = e.id JOIN accounts a ON a.id = l.account_id
		WHERE e.external_id LIKE 'opening:%' AND a.name <> ?
		  AND e.external_id NOT IN (SELECT external_id FROM opening_pins)
		ORDER BY e.external_id, l.id`, books.OpeningEquity)
	if err != nil {
		return nil, err
	}
	carried := map[string]*books.OpeningPin{}
	var order []string
	for rows.Next() {
		var xid string
		var pl books.PinLine
		if err := rows.Scan(&xid, &pl.Commodity, &pl.Quantity, &pl.Amount, &pl.Memo); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := tracked[xid]; !ok {
			continue
		}
		p := carried[xid]
		if p == nil {
			p = &books.OpeningPin{ExternalID: xid, Account: tracked[xid], AsOf: asOf}
			carried[xid] = p
			order = append(order, xid)
		}
		p.Amount += pl.Amount
		if holdings[xid] && (pl.Commodity != "" || pl.Amount != 0) {
			p.Lines = append(p.Lines, pl)
		}
	}
	rows.Close()
	for _, xid := range order {
		c := carried[xid]
		lines, err := encodePinLines(c.Lines)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO opening_pins(external_id, account, amount, as_of, note, lines) VALUES (?,?,?,?,?,?)`,
			c.ExternalID, c.Account, c.Amount, asOf, "carried over from the stored opening entry", lines); err != nil {
			return nil, err
		}
	}
	pins := map[string]books.OpeningPin{}
	rows, err = tx.Query(`SELECT external_id, account, amount, as_of, lines FROM opening_pins`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p books.OpeningPin
		var lines string
		if err := rows.Scan(&p.ExternalID, &p.Account, &p.Amount, &p.AsOf, &lines); err != nil {
			return nil, err
		}
		if lines != "" {
			if err := json.Unmarshal([]byte(lines), &p.Lines); err != nil {
				return nil, fmt.Errorf("opening pin %s lines: %w", p.ExternalID, err)
			}
		}
		pins[p.ExternalID] = p
	}
	return pins, rows.Err()
}

// storePins records the openings established by a build.
func storePins(tx *sql.Tx, pins []books.OpeningPin) error {
	for _, p := range pins {
		lines, err := encodePinLines(p.Lines)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO opening_pins(external_id, account, amount, as_of, note, lines) VALUES (?,?,?,?,?,?)
			ON CONFLICT(external_id) DO NOTHING`, p.ExternalID, p.Account, p.Amount, p.AsOf, "established by the first build", lines); err != nil {
			return err
		}
	}
	return nil
}

// PinOpening sets (or corrects) the pinned opening of a transaction-tracked or valuation-only
// connector account (holdings-tracked openings are pinned per position and not set here),
// then rebuilds the journal. note should record the evidence for the amount.
func PinOpening(db *sql.DB, cfg *books.Config, account string, amount int64, note string) (*Summary, error) {
	xid := ""
	for x, name := range trackedOpenings(cfg) {
		if name == account && !holdingsOpenings(cfg)[x] {
			xid = x
		}
	}
	if xid == "" {
		return nil, fmt.Errorf("%s is not a transaction-tracked or valuation-only connector account", account)
	}
	if note == "" {
		return nil, fmt.Errorf("a note (evidence for the amount) is required")
	}
	if _, err := Migrate(db); err != nil {
		return nil, err
	}
	var asOf string
	if err := db.QueryRow(`SELECT COALESCE(MAX(as_of),'') FROM balance_snapshots`).Scan(&asOf); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`INSERT INTO opening_pins(external_id, account, amount, as_of, note) VALUES (?,?,?,?,?)
		ON CONFLICT(external_id) DO UPDATE SET amount = excluded.amount, note = excluded.note,
		  updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')`, xid, account, amount, asOf, note); err != nil {
		return nil, err
	}
	return Rebuild(db, cfg, "pin-opening")
}
