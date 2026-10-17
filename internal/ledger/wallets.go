package ledger

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/download/wallets"
)

// WalletStageStats counts what one wallet pull staged.
type WalletStageStats struct {
	AsOf                      string
	Wallets, Identifiers      int
	Snapshots, Prices, Events int
	EventKinds                map[string]int
}

// ImportWallets stages a `finance download wallets` pull (raw/wallets/<YYYY-MM-DD>/:
// balances.json + events.csv from scripts/wallet_events.py) and rebuilds the journal. cfgPath is
// the local wallets.yaml (identifiers such as extended public keys are copied into the DB).
func ImportWallets(db *sql.DB, cfg *books.Config, dir, cfgPath string) (*Summary, error) {
	var st WalletStageStats
	s, err := runStaged(db, cfg, "wallets", dir, func(tx *sql.Tx, runID int64) error {
		var err error
		st, err = StageWallets(tx, dir, cfgPath, runID)
		return err
	})
	if s != nil {
		s.Wallets = &st
	}
	return s, err
}

func StageWallets(tx *sql.Tx, dir, cfgPath string, runID int64) (WalletStageStats, error) {
	st := WalletStageStats{AsOf: filepath.Base(filepath.Clean(dir)), EventKinds: map[string]int{}}
	if !dayDir.MatchString(st.AsOf) {
		return st, fmt.Errorf("wallet pull folder must be named YYYY-MM-DD: %s", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "MANIFEST.md")); err != nil {
		return st, fmt.Errorf("%s: MANIFEST.md missing (incomplete pull)", dir)
	}
	if err := verifySums(dir); err != nil {
		return st, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "balances.json"))
	if err != nil {
		return st, err
	}
	var pull wallets.Pull
	if err := json.Unmarshal(b, &pull); err != nil {
		return st, fmt.Errorf("balances.json: %w", err)
	}
	var conf *wallets.Config
	if cfgPath != "" {
		if conf, err = wallets.LoadConfig(cfgPath); err != nil {
			return st, err
		}
	}
	for _, w := range pull.Wallets {
		pc := 0
		if w.PerCoin {
			pc = 1
		}
		if _, err := tx.Exec(`INSERT INTO wallets(name, account, per_coin, note, updated_run_id) VALUES (?,?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET account=excluded.account, per_coin=excluded.per_coin, note=excluded.note, updated_run_id=excluded.updated_run_id`,
			w.Wallet, w.Account, pc, w.Note, runID); err != nil {
			return st, err
		}
		st.Wallets++
		type ident struct {
			chain, kind, value, path, account string
			used                              bool
		}
		var ids []ident
		for _, i := range w.Identifiers {
			if i.Kind == "xpub" {
				continue // the pull holds only a fingerprint; the key itself comes from the config
			}
			acct := ""
			if w.PerCoin {
				acct = w.Account + ":" + wallets.Coin[i.Chain].Code
			} else {
				acct = w.Account
			}
			ids = append(ids, ident{i.Chain, i.Kind, i.Value, i.Path, acct, i.Used})
		}
		if conf != nil {
			for _, cw := range conf.Wallets {
				if cw.Name != w.Wallet {
					continue
				}
				for _, c := range cw.Chains {
					if c.KPub != "" {
						ids = append(ids, ident{c.Chain, "xpub", c.KPub, c.Path, w.Account, true})
					}
				}
			}
		}
		for _, i := range ids {
			used := 0
			if i.used {
				used = 1
			}
			if _, err := tx.Exec(`INSERT INTO wallet_identifiers(wallet, chain, kind, value, path, used, account, first_seen, last_seen) VALUES (?,?,?,?,?,?,?,?,?)
				ON CONFLICT(wallet, chain, kind, value) DO UPDATE SET used=MAX(used, excluded.used), path=excluded.path, account=excluded.account, last_seen=MAX(last_seen, excluded.last_seen)`,
				w.Wallet, i.chain, i.kind, i.value, i.path, used, i.account, st.AsOf, st.AsOf); err != nil {
				return st, err
			}
			st.Identifiers++
		}
		if _, err := tx.Exec(`DELETE FROM wallet_balance_snapshots WHERE wallet=? AND as_of=?`, w.Wallet, st.AsOf); err != nil {
			return st, err
		}
		for _, h := range w.Holdings {
			if _, err := tx.Exec(`INSERT INTO wallet_balance_snapshots(wallet, as_of, commodity, account, balance, addresses, used_addresses, import_run_id) VALUES (?,?,?,?,?,?,?,?)`,
				w.Wallet, st.AsOf, h.Commodity, h.Account, h.Balance, h.Addresses, h.Used, runID); err != nil {
				return st, err
			}
			st.Snapshots++
		}
	}
	for _, p := range pull.Prices {
		if _, err := tx.Exec(`INSERT INTO wallet_prices(commodity, as_of, price, source, ref, alt, import_run_id) VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(commodity, as_of) DO UPDATE SET price=excluded.price, source=excluded.source, ref=excluded.ref, alt=excluded.alt, import_run_id=excluded.import_run_id`,
			p.Commodity, st.AsOf, p.Price, p.Source, p.Ref, p.Alt, runID); err != nil {
			return st, err
		}
		st.Prices++
	}
	if err := stagePriceHistory(tx, dir, st.AsOf); err != nil {
		return st, err
	}
	// events: optional (without them the wallets are carried at cost by the adjustment only)
	f, err := os.Open(filepath.Join(dir, "events.csv"))
	if os.IsNotExist(err) {
		return st, nil
	} else if err != nil {
		return st, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	head, err := r.Read()
	if err != nil {
		return st, fmt.Errorf("events.csv: %w", err)
	}
	cols := map[string]int{}
	for i, h := range head {
		cols[h] = i
	}
	need := []string{"id", "time", "date", "kind", "wallet_out", "coin_out", "qty_out", "fee_out", "wallet_in", "coin_in", "qty_in",
		"ref", "price_out", "price_in", "usd_out", "usd_in", "confidence", "evidence", "memo"}
	for _, n := range need {
		if _, ok := cols[n]; !ok {
			return st, fmt.Errorf("events.csv: column %s missing", n)
		}
	}
	if _, err := tx.Exec(`DELETE FROM wallet_events`); err != nil {
		return st, err
	}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, fmt.Errorf("events.csv: %w", err)
		}
		args := []any{}
		for _, n := range need {
			args = append(args, rec[cols[n]])
		}
		args = append(args, st.AsOf, runID)
		if _, err := tx.Exec(`INSERT INTO wallet_events(id, time, date, kind, wallet_out, coin_out, qty_out, fee_out, wallet_in, coin_in, qty_in,
			ref, price_out, price_in, usd_out, usd_in, confidence, evidence, memo, pull, import_run_id) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...); err != nil {
			return st, fmt.Errorf("events.csv %s: %w", rec[0], err)
		}
		st.Events++
		st.EventKinds[rec[cols["kind"]]]++
	}
	return st, nil
}

// stagePriceHistory stores the daily USD closes of a pull's history/ohlc_<COIN>.json (Kraken public
// OHLC, UTC-day candles, last 720 days) into crypto_price_history. The candle of the pull date
// itself is still open and skipped; the pull's own ticker price covers that date.
func stagePriceHistory(tx *sql.Tx, dir, asOf string) error {
	files, _ := filepath.Glob(filepath.Join(dir, "history", "ohlc_*.json"))
	sort.Strings(files)
	for _, f := range files {
		coin := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "ohlc_"), ".json")
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var doc struct {
			Error  []string                   `json:"error"`
			Result map[string]json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		for k, raw := range doc.Result {
			if k == "last" {
				continue
			}
			var rows [][]json.RawMessage
			if err := json.Unmarshal(raw, &rows); err != nil {
				return fmt.Errorf("%s: %w", filepath.Base(f), err)
			}
			for _, r := range rows {
				if len(r) < 5 {
					continue
				}
				var ts float64
				var closeS string
				if json.Unmarshal(r[0], &ts) != nil || json.Unmarshal(r[4], &closeS) != nil {
					continue
				}
				day := time.Unix(int64(ts), 0).UTC().Format("2006-01-02")
				if day >= asOf {
					continue
				}
				if _, err := tx.Exec(`INSERT INTO crypto_price_history(commodity, price_date, price, source) VALUES (?,?,?,'kraken-ohlc-close')
					ON CONFLICT(commodity, price_date) DO UPDATE SET price=excluded.price, source=excluded.source`, coin, day, closeS); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// LoadWallets reads the staged wallets for the builder.
func LoadWallets(tx *sql.Tx) (books.WalletData, error) {
	var wd books.WalletData
	rows, err := tx.Query(`SELECT name, account, per_coin, note FROM wallets ORDER BY name`)
	if err != nil {
		return wd, err
	}
	for rows.Next() {
		var w books.WalletDef
		var pc int
		if err := rows.Scan(&w.Name, &w.Account, &pc, &w.Note); err != nil {
			rows.Close()
			return wd, err
		}
		w.PerCoin = pc == 1
		wd.Wallets = append(wd.Wallets, w)
	}
	rows.Close()
	rows, err = tx.Query(`SELECT wallet, as_of, commodity, account, balance FROM wallet_balance_snapshots ORDER BY as_of, wallet, commodity`)
	if err != nil {
		return wd, err
	}
	for rows.Next() {
		var s books.WalletSnapshot
		if err := rows.Scan(&s.Wallet, &s.AsOf, &s.Commodity, &s.Account, &s.Balance); err != nil {
			rows.Close()
			return wd, err
		}
		wd.Snapshots = append(wd.Snapshots, s)
	}
	rows.Close()
	rows, err = tx.Query(`SELECT commodity, as_of, price, source FROM wallet_prices ORDER BY as_of, commodity`)
	if err != nil {
		return wd, err
	}
	for rows.Next() {
		var p books.WalletPrice
		if err := rows.Scan(&p.Commodity, &p.AsOf, &p.Price, &p.Source); err != nil {
			rows.Close()
			return wd, err
		}
		wd.Prices = append(wd.Prices, p)
	}
	rows.Close()
	rows, err = tx.Query(`SELECT commodity, price_date, price, source FROM crypto_price_history ORDER BY commodity, price_date`)
	if err != nil {
		return wd, err
	}
	for rows.Next() {
		var p books.WalletPrice
		if err := rows.Scan(&p.Commodity, &p.AsOf, &p.Price, &p.Source); err != nil {
			rows.Close()
			return wd, err
		}
		wd.History = append(wd.History, p)
	}
	rows.Close()
	rows, err = tx.Query(`SELECT id, time, date, kind, wallet_out, coin_out, qty_out, fee_out, wallet_in, coin_in, qty_in, ref, usd_out, usd_in, confidence, evidence, memo
		FROM wallet_events ORDER BY time, id`)
	if err != nil {
		return wd, err
	}
	for rows.Next() {
		var e books.WalletEvent
		if err := rows.Scan(&e.ID, &e.Time, &e.Date, &e.Kind, &e.WalletOut, &e.CoinOut, &e.QtyOut, &e.FeeOut, &e.WalletIn, &e.CoinIn, &e.QtyIn,
			&e.Ref, &e.UsdOut, &e.UsdIn, &e.Confidence, &e.Evidence, &e.Memo); err != nil {
			rows.Close()
			return wd, err
		}
		wd.Events = append(wd.Events, e)
	}
	rows.Close()
	sort.SliceStable(wd.Events, func(i, j int) bool { return wd.Events[i].Time < wd.Events[j].Time })
	return wd, nil
}
