package ledger

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/download/coinbase"
)

// ImportCoinbase stages one saved Coinbase pull (raw/coinbase/<YYYY-MM-DD>/) and rebuilds the
// journal. The exchange builder is shared with Kraken (books.buildExchange, CoA key coinbase).
func ImportCoinbase(db *sql.DB, cfg *books.Config, dir string) (*Summary, error) {
	var st ExchangeStageStats
	s, err := runStaged(db, cfg, "coinbase", dir, func(tx *sql.Tx, runID int64) error {
		var err error
		st, err = StageCoinbase(tx, dir, runID)
		return err
	})
	if s != nil {
		s.Exchange = &st
	}
	return s, err
}

// StageCoinbase copies a saved Coinbase pull into the exchange staging tables. Ledger rows
// come from connector fills plus events.csv; each pull carries the full history, so rows
// staged earlier but absent from this pull are removed (an edited evidence file wins).
// Re-staging the same folder changes nothing.
func StageCoinbase(tx *sql.Tx, dir string, runID int64) (ExchangeStageStats, error) {
	const ex = "coinbase"
	st := ExchangeStageStats{Exchange: ex, AsOf: filepath.Base(filepath.Clean(dir))}
	if !dayDir.MatchString(st.AsOf) {
		return st, fmt.Errorf("coinbase pull folder must be named YYYY-MM-DD: %s", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "MANIFEST.md")); err != nil {
		return st, fmt.Errorf("%s: MANIFEST.md missing (list the saved connector responses; see README)", dir)
	}
	if err := verifySums(dir); err != nil {
		return st, err
	}
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		return st, err
	}

	// ---- ledger rows: connector fills + events.csv ----
	var rows []coinbase.Row
	fills, _ := filepath.Glob(filepath.Join(dir, "fills_*.json"))
	sort.Strings(fills)
	for _, p := range fills {
		b, err := os.ReadFile(p)
		if err != nil {
			return st, err
		}
		fr, err := coinbase.ParseFills(b)
		if err != nil {
			return st, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		rows = append(rows, fr...)
		st.Trades += len(fr) / 2
	}
	if f, err := os.Open(filepath.Join(dir, "events.csv")); err == nil {
		er, err := coinbase.ParseEvents(f)
		f.Close()
		if err != nil {
			return st, err
		}
		rows = append(rows, er...)
	} else if !os.IsNotExist(err) {
		return st, err
	}
	seen := map[string]bool{}
	first := ""
	for _, r := range rows {
		if seen[r.ID] {
			return st, fmt.Errorf("coinbase: duplicate ledger id %s", r.ID)
		}
		seen[r.ID] = true
		ts := strconv.FormatFloat(r.Time, 'f', -1, 64)
		sec, frac := splitTime(r.Time)
		tm := time.Unix(sec, frac)
		date := tm.In(loc).Format("2006-01-02")
		if d := tm.UTC().Format("2006-01-02"); first == "" || d < first {
			first = d
		}
		var old, oldAmt, oldFee, oldTime string
		err := tx.QueryRow(`SELECT raw_json, amount, fee, time FROM raw_exchange_ledger WHERE exchange=? AND ledger_id=?`, ex, r.ID).Scan(&old, &oldAmt, &oldFee, &oldTime)
		switch {
		case err == sql.ErrNoRows:
			st.LedgerNew++
		case err != nil:
			return st, err
		case old == r.Raw && oldAmt == r.Amount && oldFee == r.Fee && oldTime == ts:
			st.LedgerUnchanged++
			if _, err := tx.Exec(`UPDATE raw_exchange_ledger SET last_run_id=? WHERE exchange=? AND ledger_id=?`, runID, ex, r.ID); err != nil {
				return st, err
			}
			continue
		default:
			st.LedgerChanged++
		}
		if _, err := tx.Exec(`INSERT INTO raw_exchange_ledger(exchange, ledger_id, refid, time, local_date, type, subtype, asset, commodity, amount, fee, balance, raw_json, first_run_id, last_run_id)
			VALUES (?,?,?,?,?,?,'',?,?,?,?,'',?,?,?)
			ON CONFLICT(exchange, ledger_id) DO UPDATE SET refid=excluded.refid, time=excluded.time, local_date=excluded.local_date, type=excluded.type,
			subtype=excluded.subtype, asset=excluded.asset, commodity=excluded.commodity, amount=excluded.amount, fee=excluded.fee,
			balance=excluded.balance, raw_json=excluded.raw_json, last_run_id=excluded.last_run_id`,
			ex, r.ID, r.RefID, ts, date, r.Type, r.Asset, r.Asset, r.Amount, r.Fee, r.Raw, runID, runID); err != nil {
			return st, err
		}
	}
	res, err := tx.Exec(`DELETE FROM raw_exchange_ledger WHERE exchange=? AND last_run_id <> ?`, ex, runID)
	if err != nil {
		return st, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		st.LedgerChanged += int(n)
	}

	// ---- balances: coinbase_balance accounts, overridden by portfolio spot positions ----
	bal := map[string]string{}
	bfiles, _ := filepath.Glob(filepath.Join(dir, "balance_*.json"))
	pfiles, _ := filepath.Glob(filepath.Join(dir, "portfolio_*.json"))
	if len(bfiles)+len(pfiles) == 0 {
		return st, fmt.Errorf("%s: no balance_*.json or portfolio_*.json (coinbase_balance / coinbase_portfolios_get)", dir)
	}
	sort.Strings(bfiles)
	sort.Strings(pfiles)
	add := func(m map[string]string, name string) error {
		for a, v := range m {
			if prev, ok := bal[a]; ok && prev != "0" && v != prev {
				return fmt.Errorf("%s: %s %s disagrees with %s", name, a, v, prev)
			}
			bal[a] = v
		}
		return nil
	}
	for _, p := range bfiles {
		b, err := os.ReadFile(p)
		if err != nil {
			return st, err
		}
		m, err := coinbase.ParseBalances(b)
		if err != nil {
			return st, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		if err := add(m, filepath.Base(p)); err != nil {
			return st, err
		}
	}
	for _, p := range pfiles {
		b, err := os.ReadFile(p)
		if err != nil {
			return st, err
		}
		m, err := coinbase.ParsePortfolio(b)
		if err != nil {
			return st, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		for a, v := range m { // positions include staked balances: they win
			bal[a] = v
		}
	}
	if _, err := tx.Exec(`DELETE FROM exchange_balance_snapshots WHERE exchange=? AND as_of=?`, ex, st.AsOf); err != nil {
		return st, err
	}
	assets := make([]string, 0, len(bal))
	for a := range bal {
		assets = append(assets, a)
	}
	sort.Strings(assets)
	for _, a := range assets {
		if _, err := tx.Exec(`INSERT INTO exchange_balance_snapshots(exchange, as_of, asset, commodity, balance, raw_json, import_run_id) VALUES (?,?,?,?,?,?,?)`,
			ex, st.AsOf, a, a, bal[a], fmt.Sprintf("%q", bal[a]), runID); err != nil {
			return st, err
		}
		st.Balances++
	}

	// ---- prices ----
	putPrice := func(com, d, kind, price, pair string) error {
		_, err := tx.Exec(`INSERT INTO exchange_prices(exchange, commodity, price_date, kind, price, pair) VALUES (?,?,?,?,?,?)
			ON CONFLICT(exchange, commodity, price_date, kind) DO UPDATE SET price=excluded.price, pair=excluded.pair`, ex, com, d, kind, price, pair)
		return err
	}
	cfiles, _ := filepath.Glob(filepath.Join(dir, "candles_*-USD.json"))
	sort.Strings(cfiles)
	for _, p := range cfiles {
		pair := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "candles_"), ".json")
		base := strings.TrimSuffix(pair, "-USD")
		b, err := os.ReadFile(p)
		if err != nil {
			return st, err
		}
		cs, err := coinbase.ParseCandles(b)
		if err != nil {
			return st, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		for _, c := range cs {
			if err := putPrice(base, time.Unix(c.Start, 0).UTC().Format("2006-01-02"), "close", c.Close, pair); err != nil {
				return st, err
			}
			st.Closes++
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "best_bid_ask.json")); err == nil {
		mids, err := coinbase.ParseBestBidAsk(b)
		if err != nil {
			return st, fmt.Errorf("best_bid_ask.json: %w", err)
		}
		for prod, mid := range mids {
			base, quote, ok := strings.Cut(prod, "-")
			if !ok || quote != "USD" {
				continue
			}
			if err := putPrice(base, st.AsOf, "ticker", mid, prod); err != nil {
				return st, err
			}
			st.Ticks++
		}
	}
	// USDC: Coinbase converts USD<->USDC 1:1 without a fee and lists no USDC-USD product, so
	// USDC is priced at par for every day of the history and at the pull.
	if first != "" {
		start, _ := time.Parse("2006-01-02", first)
		end, _ := time.Parse("2006-01-02", st.AsOf)
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			if err := putPrice("USDC", d.Format("2006-01-02"), "close", "1", "USDC-USD (par)"); err != nil {
				return st, err
			}
			st.Closes++
		}
	}
	if _, ok := bal["USDC"]; ok || first != "" {
		if err := putPrice("USDC", st.AsOf, "ticker", "1", "USDC-USD (par)"); err != nil {
			return st, err
		}
		st.Ticks++
	}
	return st, nil
}
