package ledger

import (
	"database/sql"
	"fmt"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
)

// Summary of one pipeline run.
type Summary struct {
	RunID  int64
	AsOf   string
	Stage  *StageStats
	Build  books.Stats
	Sync   SyncStats
	Result *books.Result
	// OpeningDrift lists connector accounts (all pinned, incl. investment accounts) whose opening balance changed
	// in this run. Their openings are pinned (opening_pins), so this only happens when a pin
	// is corrected explicitly; a reported balance that does not follow from pinned opening +
	// history shows up in Result.Pending instead (booked to a pending account).
	OpeningDrift []Drift
	// Exchange is set by `finance import kraken`.
	Exchange *ExchangeStageStats
	// Wallets is set by `finance import wallets`.
	Wallets *WalletStageStats
}

// Drift is one changed opening balance (minor units, debit-positive).
type Drift struct {
	Account  string
	Old, New int64
}

// openingDrift compares stored opening entries with the freshly built ones.
func openingDrift(tx *sql.Tx, cfg *books.Config, res *books.Result) ([]Drift, error) {
	txnTracked := trackedOpenings(cfg) // opening external id -> account name (every pinned opening)
	rows, err := tx.Query(`SELECT e.external_id, l.amount FROM journal_entries e
		JOIN journal_lines l ON l.entry_id = e.id JOIN accounts a ON a.id = l.account_id
		WHERE e.external_id LIKE 'opening:%' AND a.name <> ?`, books.OpeningEquity)
	if err != nil {
		return nil, err
	}
	old := map[string]int64{}
	for rows.Next() {
		var xid string
		var amt int64
		if err := rows.Scan(&xid, &amt); err != nil {
			rows.Close()
			return nil, err
		}
		old[xid] += amt
	}
	rows.Close()
	var out []Drift
	for _, e := range res.Entries {
		name, ok := txnTracked[e.ExternalID]
		if !ok {
			continue
		}
		prev, had := old[e.ExternalID]
		if !had {
			continue
		}
		var amt int64
		for _, l := range e.Lines {
			if l.Account != books.OpeningEquity {
				amt += l.Amount
			}
		}
		if amt != prev {
			out = append(out, Drift{Account: name, Old: prev, New: amt})
		}
	}
	return out, nil
}

// Import stages an extract and rebuilds the journal from staging, in one transaction.
func Import(db *sql.DB, cfg *books.Config, ex *connector.Extract, rawDir string) (*Summary, error) {
	return run(db, cfg, "connector", rawDir, ex)
}

// Rebuild re-derives the journal from what is already staged (after rule/config changes).
func Rebuild(db *sql.DB, cfg *books.Config, source string) (*Summary, error) {
	return run(db, cfg, source, "", nil)
}

func run(db *sql.DB, cfg *books.Config, source, rawDir string, ex *connector.Extract) (*Summary, error) {
	var stage func(*sql.Tx, int64) error
	var s *Summary
	if ex != nil {
		stage = func(tx *sql.Tx, runID int64) error {
			st, err := StageExtract(tx, ex, cfg, runID)
			if err != nil {
				return fmt.Errorf("staging: %w", err)
			}
			s.Stage = &st
			return nil
		}
	}
	return runStagedInto(db, cfg, source, rawDir, ex, stage, &s)
}

// runStaged runs a non-connector staging step (e.g. an exchange pull), then rebuilds.
func runStaged(db *sql.DB, cfg *books.Config, source, rawDir string, stage func(*sql.Tx, int64) error) (*Summary, error) {
	var s *Summary
	return runStagedInto(db, cfg, source, rawDir, nil, stage, &s)
}

func runStagedInto(db *sql.DB, cfg *books.Config, source, rawDir string, ex *connector.Extract, stage func(*sql.Tx, int64) error, out **Summary) (*Summary, error) {
	if _, err := Migrate(db); err != nil {
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	asOf := ""
	if ex != nil {
		asOf = ex.AsOf
	}
	runID, err := BeginRun(tx, source, rawDir, asOf)
	if err != nil {
		return nil, err
	}
	s := &Summary{RunID: runID}
	*out = s
	if stage != nil {
		if err := stage(tx, runID); err != nil {
			return nil, err
		}
	}
	in, err := LoadInput(tx)
	if err != nil {
		return nil, err
	}
	if in.Exchanges, err = LoadExchanges(tx); err != nil {
		return nil, fmt.Errorf("exchange staging: %w", err)
	}
	if in.Wallets, err = LoadWallets(tx); err != nil {
		return nil, fmt.Errorf("wallet staging: %w", err)
	}
	s.AsOf = in.AsOf
	if in.OpeningPins, err = loadPins(tx, cfg, in.AsOf); err != nil {
		return nil, fmt.Errorf("opening pins: %w", err)
	}
	res, err := books.Build(in, cfg)
	if err != nil {
		return nil, fmt.Errorf("build: %w", err)
	}
	s.Result, s.Build = res, res.Stats
	if s.OpeningDrift, err = openingDrift(tx, cfg, res); err != nil {
		return nil, fmt.Errorf("opening drift: %w", err)
	}
	if err := storePins(tx, res.NewPins); err != nil {
		return nil, fmt.Errorf("opening pins: %w", err)
	}
	if s.Sync, err = Sync(tx, res, cfg, runID); err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	if err := FinishRun(tx, runID, map[string]any{"stage": s.Stage, "build": s.Build, "sync": s.Sync, "opening_drift": len(s.OpeningDrift), "pending": len(res.Pending), "exchange_source": source}); err != nil {
		return nil, err
	}
	return s, tx.Commit()
}
