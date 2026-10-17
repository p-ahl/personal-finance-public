package ledger

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // America/Denver dates for exchange rows on any host

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/download/kraken"
)

// ExchangeStageStats counts what one exchange pull added to staging.
type ExchangeStageStats struct {
	Exchange                        string
	AsOf                            string
	LedgerNew, LedgerChanged        int
	LedgerUnchanged                 int
	Trades, Balances, Closes, Ticks int
}

var dayDir = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ImportKraken stages one raw Kraken pull (raw/kraken/<YYYY-MM-DD>/) and rebuilds the journal.
func ImportKraken(db *sql.DB, cfg *books.Config, dir string) (*Summary, error) {
	var st ExchangeStageStats
	s, err := runStaged(db, cfg, "kraken", dir, func(tx *sql.Tx, runID int64) error {
		var err error
		st, err = StageKraken(tx, dir, runID)
		return err
	})
	if s != nil {
		s.Exchange = &st
	}
	return s, err
}

// StageKraken copies a saved Kraken pull into the exchange staging tables (cumulative, keyed by
// Kraken's ids; re-staging the same folder changes nothing).
func StageKraken(tx *sql.Tx, dir string, runID int64) (ExchangeStageStats, error) {
	st := ExchangeStageStats{Exchange: "kraken", AsOf: filepath.Base(filepath.Clean(dir))}
	if !dayDir.MatchString(st.AsOf) {
		return st, fmt.Errorf("kraken pull folder must be named YYYY-MM-DD: %s", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "MANIFEST.md")); err != nil {
		return st, fmt.Errorf("%s: MANIFEST.md missing (run `finance download kraken`)", dir)
	}
	if err := verifySums(dir); err != nil {
		return st, err
	}
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, name)) }
	pb, err := read("AssetPairs.json")
	if err != nil {
		return st, fmt.Errorf("kraken pull: %w", err)
	}
	ab, _ := read("Assets.json")
	pairs, err := kraken.ParsePairs(pb, ab)
	if err != nil {
		return st, err
	}
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		return st, err
	}

	// ledger pages
	pages, _ := filepath.Glob(filepath.Join(dir, "Ledgers_page*.json"))
	sort.Strings(pages)
	if len(pages) == 0 {
		return st, fmt.Errorf("%s: no Ledgers_pageNNN.json (the key needs the Query Ledger Entries permission)", dir)
	}
	for _, p := range pages {
		b, err := os.ReadFile(p)
		if err != nil {
			return st, err
		}
		rows, err := kraken.ParseLedgerPage(b)
		if err != nil {
			return st, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		for _, r := range rows {
			ts := strconv.FormatFloat(r.Time, 'f', -1, 64)
			sec, frac := splitTime(r.Time)
			date := time.Unix(sec, frac).In(loc).Format("2006-01-02")
			raw := string(r.Raw)
			var old string
			err := tx.QueryRow(`SELECT raw_json FROM raw_exchange_ledger WHERE exchange='kraken' AND ledger_id=?`, r.ID).Scan(&old)
			switch {
			case err == sql.ErrNoRows:
				st.LedgerNew++
			case err != nil:
				return st, err
			case old == raw:
				st.LedgerUnchanged++
				if _, err := tx.Exec(`UPDATE raw_exchange_ledger SET last_run_id=? WHERE exchange='kraken' AND ledger_id=?`, runID, r.ID); err != nil {
					return st, err
				}
				continue
			default:
				st.LedgerChanged++
			}
			if _, err := tx.Exec(`INSERT INTO raw_exchange_ledger(exchange, ledger_id, refid, time, local_date, type, subtype, asset, commodity, amount, fee, balance, raw_json, first_run_id, last_run_id)
				VALUES ('kraken',?,?,?,?,?,?,?,?,?,?,?,?,?,?)
				ON CONFLICT(exchange, ledger_id) DO UPDATE SET refid=excluded.refid, time=excluded.time, local_date=excluded.local_date, type=excluded.type,
				subtype=excluded.subtype, asset=excluded.asset, commodity=excluded.commodity, amount=excluded.amount, fee=excluded.fee,
				balance=excluded.balance, raw_json=excluded.raw_json, last_run_id=excluded.last_run_id`,
				r.ID, r.RefID, ts, date, r.Type, r.Subtype, r.Asset, pairs.Commodity(r.Asset), r.Amount, r.Fee, r.Balance, raw, runID, runID); err != nil {
				return st, err
			}
		}
	}

	// trades (audit trail)
	tpages, _ := filepath.Glob(filepath.Join(dir, "TradesHistory_page*.json"))
	sort.Strings(tpages)
	for _, p := range tpages {
		b, err := os.ReadFile(p)
		if err != nil {
			return st, err
		}
		trades, err := kraken.ParseTradesPage(b)
		if err != nil {
			return st, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		for _, t := range trades {
			if _, err := tx.Exec(`INSERT INTO raw_exchange_trades(exchange, trade_id, pair, time, side, price, cost, fee, vol, raw_json, first_run_id, last_run_id)
				VALUES ('kraken',?,?,?,?,?,?,?,?,?,?,?)
				ON CONFLICT(exchange, trade_id) DO UPDATE SET raw_json=excluded.raw_json, last_run_id=excluded.last_run_id`,
				t.ID, t.Pair, strconv.FormatFloat(t.Time, 'f', -1, 64), t.Type, t.Price, t.Cost, t.Fee, t.Vol, string(t.Raw), runID, runID); err != nil {
				return st, err
			}
			st.Trades++
		}
	}

	// balances (reconciliation source): replace this pull's snapshot
	bb, err := read("Balance.json")
	if err != nil {
		return st, fmt.Errorf("%s: Balance.json missing (the key needs the Query Funds permission)", dir)
	}
	bal, err := kraken.ParseBalance(bb)
	if err != nil {
		return st, fmt.Errorf("Balance.json: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM exchange_balance_snapshots WHERE exchange='kraken' AND as_of=?`, st.AsOf); err != nil {
		return st, err
	}
	assets := make([]string, 0, len(bal))
	for a := range bal {
		assets = append(assets, a)
	}
	sort.Strings(assets)
	for _, a := range assets {
		if _, err := tx.Exec(`INSERT INTO exchange_balance_snapshots(exchange, as_of, asset, commodity, balance, raw_json, import_run_id) VALUES ('kraken',?,?,?,?,?,?)`,
			st.AsOf, a, pairs.Commodity(a), bal[a], fmt.Sprintf("%q", bal[a]), runID); err != nil {
			return st, err
		}
		st.Balances++
	}

	// prices: daily closes and the ticker at pull time
	ohlc, _ := filepath.Glob(filepath.Join(dir, "OHLC_*.json"))
	sort.Strings(ohlc)
	for _, p := range ohlc {
		b, err := os.ReadFile(p)
		if err != nil {
			return st, err
		}
		pair, candles, err := kraken.ParseOHLC(b)
		if err != nil {
			return st, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		base, _, ok := pairs.PairAssets(pair)
		if !ok {
			continue
		}
		com := pairs.Commodity(base)
		for _, c := range candles {
			d := time.Unix(c.Time, 0).UTC().Format("2006-01-02")
			if _, err := tx.Exec(`INSERT INTO exchange_prices(exchange, commodity, price_date, kind, price, pair) VALUES ('kraken',?,?,'close',?,?)
				ON CONFLICT(exchange, commodity, price_date, kind) DO UPDATE SET price=excluded.price, pair=excluded.pair`, com, d, c.Close, pair); err != nil {
				return st, err
			}
			st.Closes++
		}
	}
	if tb, err := read("Ticker.json"); err == nil {
		tick, err := kraken.ParseTicker(tb)
		if err != nil {
			return st, fmt.Errorf("Ticker.json: %w", err)
		}
		for pair, price := range tick {
			base, _, ok := pairs.PairAssets(pair)
			if !ok {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO exchange_prices(exchange, commodity, price_date, kind, price, pair) VALUES ('kraken',?,?,'ticker',?,?)
				ON CONFLICT(exchange, commodity, price_date, kind) DO UPDATE SET price=excluded.price, pair=excluded.pair`, pairs.Commodity(base), st.AsOf, price, pair); err != nil {
				return st, err
			}
			st.Ticks++
		}
	}
	return st, nil
}

// splitTime turns Kraken's float seconds into (sec, nsec) for the date only.
func splitTime(t float64) (int64, int64) {
	sec := int64(t)
	return sec, int64((t - float64(sec)) * 1e9)
}

func verifySums(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return nil // optional
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, f[1]))
		if err != nil {
			return fmt.Errorf("SHA256SUMS: %w", err)
		}
		h := sha256.Sum256(data)
		if hex.EncodeToString(h[:]) != f[0] {
			return fmt.Errorf("SHA256SUMS: %s does not match (raw files must not be edited)", f[1])
		}
	}
	return nil
}

// LoadExchanges reads every staged exchange for the builder.
func LoadExchanges(tx *sql.Tx) ([]books.ExchangeData, error) {
	rows, err := tx.Query(`SELECT exchange FROM raw_exchange_ledger UNION SELECT exchange FROM exchange_balance_snapshots ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	var out []books.ExchangeData
	for _, n := range names {
		ex := books.ExchangeData{Name: n, Closes: map[string]map[string]string{}, Ticker: map[string]string{}}
		if err := tx.QueryRow(`SELECT COALESCE(MAX(as_of),'') FROM exchange_balance_snapshots WHERE exchange=?`, n).Scan(&ex.AsOf); err != nil {
			return nil, err
		}
		r, err := tx.Query(`SELECT ledger_id, refid, time, local_date, type, subtype, asset, commodity, amount, fee, balance
			FROM raw_exchange_ledger WHERE exchange=? ORDER BY CAST(time AS REAL), ledger_id`, n)
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var l books.ExchangeLedgerRow
			if err := r.Scan(&l.ID, &l.RefID, &l.Time, &l.Date, &l.Type, &l.Subtype, &l.Asset, &l.Commodity, &l.Amount, &l.Fee, &l.Balance); err != nil {
				r.Close()
				return nil, err
			}
			ex.Ledger = append(ex.Ledger, l)
		}
		r.Close()
		r, err = tx.Query(`SELECT asset, commodity, balance FROM exchange_balance_snapshots WHERE exchange=? AND as_of=? ORDER BY asset`, n, ex.AsOf)
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var b books.ExchangeBalance
			if err := r.Scan(&b.Asset, &b.Commodity, &b.Balance); err != nil {
				r.Close()
				return nil, err
			}
			ex.Balances = append(ex.Balances, b)
		}
		r.Close()
		r, err = tx.Query(`SELECT commodity, price_date, kind, price FROM exchange_prices WHERE exchange=? ORDER BY commodity, price_date`, n)
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var c, d, k, p string
			if err := r.Scan(&c, &d, &k, &p); err != nil {
				r.Close()
				return nil, err
			}
			if k == "ticker" {
				if d == ex.AsOf {
					ex.Ticker[c] = p
				}
				continue
			}
			if ex.Closes[c] == nil {
				ex.Closes[c] = map[string]string{}
			}
			ex.Closes[c][d] = p
		}
		r.Close()
		out = append(out, ex)
	}
	return out, nil
}
