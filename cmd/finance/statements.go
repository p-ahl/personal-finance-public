package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/p-ahl/personal-finance/internal/report"
)

func setFlag(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// statementColumns builds the period columns (income statement, cash flow) and the as-of
// columns (balance sheet). from defaults to the first day of the month of the first entry.
func statementColumns(db *sql.DB, from, to, period, extra, dates string) (pcols, bcols []report.Column, err error) {
	if from == "" {
		var first sql.NullString
		if err := db.QueryRow(`SELECT MIN(date) FROM v_ledger`).Scan(&first); err != nil {
			return nil, nil, err
		}
		if !first.Valid {
			return nil, nil, fmt.Errorf("empty ledger")
		}
		from = first.String[:8] + "01"
	}
	pcols, err = report.Periods(from, to, period)
	if err != nil {
		return nil, nil, err
	}
	for _, x := range strings.Split(extra, ",") {
		if x = strings.TrimSpace(x); x == "" {
			continue
		}
		c, err := report.ExtraPeriod(x, from, to)
		if err != nil {
			return nil, nil, err
		}
		pcols = append(pcols, c)
	}
	switch {
	case dates != "":
		for _, d := range strings.Split(dates, ",") {
			d = strings.TrimSpace(d)
			bcols = append(bcols, report.Column{Key: d, Label: d, To: d})
		}
	case period == "none" || period == "total" || period == "":
		bcols = []report.Column{{Key: to, Label: to, To: to}}
	default:
		bcols = report.AsOfDates(from, to)
	}
	return pcols, bcols, nil
}

func runStatements(db *sql.DB, kind, from, to, period, extra, dates, labelsFile, format string) int {
	lb, err := report.LoadLabels(labelsFile)
	if err != nil {
		return fail(err)
	}
	pcols, bcols, err := statementColumns(db, from, to, period, extra, dates)
	if err != nil {
		return fail(err)
	}
	var out []*report.Statement
	if kind == "balance-sheet" || kind == "statements" {
		s, err := report.ClassifiedBalanceSheet(db, bcols, lb)
		if err != nil {
			return fail(err)
		}
		out = append(out, s)
	}
	if kind == "income-statement" || kind == "statements" {
		s, err := report.ClassifiedIncomeStatement(db, pcols, lb)
		if err != nil {
			return fail(err)
		}
		out = append(out, s)
	}
	if kind == "cash-flow" || kind == "statements" {
		s, err := report.ClassifiedCashFlow(db, pcols, lb)
		if err != nil {
			return fail(err)
		}
		out = append(out, s)
	}
	bad := false
	for _, s := range out {
		if s.CheckRow() != 0 {
			bad = true
			fmt.Fprintf(os.Stderr, "%s: check row is not zero (%s)\n", s.Kind, report.FormatUSD(s.CheckRow()))
		}
	}
	if format == "json" && kind == "statements" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"as_of": to, "statements": out}); err != nil {
			return fail(err)
		}
	} else {
		for i, s := range out {
			if i > 0 && format != "json" {
				fmt.Println()
			}
			if err := s.Render(os.Stdout, format); err != nil {
				return fail(err)
			}
		}
	}
	if bad {
		return 1
	}
	return 0
}
