package report_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/ledger"
	"github.com/p-ahl/personal-finance/internal/report"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

// fixtureDB imports the synthetic extract with the synthetic accruals (all amounts made up).
func fixtureDB(t *testing.T) *sql.DB {
	t.Helper()
	root := t.TempDir()
	dir := testfixture.Extract(t, root)
	p := testfixture.Config(t, root)
	cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, p.LocalRules, p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.LoadAccruals(p.Accruals); err != nil {
		t.Fatal(err)
	}
	ex, err := connector.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := ledger.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := ledger.Import(db, cfg, ex, dir); err != nil {
		t.Fatal(err)
	}
	return db
}

func row(t *testing.T, s *report.Statement, label string) report.Row {
	t.Helper()
	for _, r := range s.Rows {
		if r.Label == label {
			return r
		}
	}
	t.Fatalf("%s: no row %q", s.Kind, label)
	return report.Row{}
}

func hasRowPrefix(s *report.Statement, prefix string) bool {
	for _, r := range s.Rows {
		if strings.HasPrefix(r.Label, prefix) {
			return true
		}
	}
	return false
}

func TestClassifiedStatementsTieOut(t *testing.T) {
	db := fixtureDB(t)
	from, to := "2025-12-01", testfixture.AsOf
	pcols, err := report.Periods(from, to, "month")
	if err != nil {
		t.Fatal(err)
	}
	if len(pcols) != 3 || pcols[2].Label != "Feb 2026 (to 02-01)" || pcols[0].From != from {
		t.Fatalf("periods %+v", pcols)
	}
	ytd, _ := report.ExtraPeriod("ytd", from, to)
	ttm, _ := report.ExtraPeriod("ttm", from, to)
	if ytd.From != "2026-01-01" || ttm.From != "2025-02-02" {
		t.Fatalf("extra %+v %+v", ytd, ttm)
	}
	pcols = append(pcols, ytd, ttm)
	bcols := report.AsOfDates(from, to)
	if len(bcols) != 3 || bcols[0].To != "2025-12-31" || bcols[2].To != to {
		t.Fatalf("as-of %+v", bcols)
	}
	lb := report.Labels{"Assets:Bank:TestBank": "Main bank"}

	bs, err := report.ClassifiedBalanceSheet(db, bcols, lb)
	if err != nil {
		t.Fatal(err)
	}
	is, err := report.ClassifiedIncomeStatement(db, pcols, lb)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := report.ClassifiedCashFlow(db, pcols, lb)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*report.Statement{bs, is, cf} {
		if s.CheckRow() != 0 {
			var buf bytes.Buffer
			_ = s.Render(&buf, "text")
			t.Fatalf("%s does not tie out:\n%s", s.Kind, buf.String())
		}
	}
	// net income on the income statement = change in retained earnings on the balance sheet
	re := row(t, bs, "Net income since books start (retained earnings)")
	ni := row(t, is, "Net income")
	if re.Values[2]-re.Values[0] != ni.Values[1]+ni.Values[2] {
		t.Fatalf("retained earnings change %d vs net income %d", re.Values[2]-re.Values[0], ni.Values[1]+ni.Values[2])
	}
	// ending cash on the cash flow = cash on the balance sheet
	cash := row(t, bs, "Cash")
	end := row(t, cf, "Cash on the balance sheet at end of period")
	if end.Values[2] != cash.Values[2] || end.Values[1] != cash.Values[1] {
		t.Fatalf("cash flow end %v vs balance sheet cash %v", end.Values, cash.Values)
	}
	// labels file applies to the cash item; groups follow the chart
	if !hasRowPrefix(bs, "Main bank") || !hasRowPrefix(bs, "Credit cards") {
		t.Fatal("expected labelled cash item and a credit card group")
	}
	// the lender-funded land purchase is non-cash: memo only, no investing or financing flow
	if !hasRowPrefix(cf, "Non-cash: land acquired with new loans") {
		t.Fatal("missing non-cash memo for the lender-funded land")
	}
	for _, r := range cf.Rows {
		if (r.Section == "Investing" || r.Section == "Financing") && r.Kind == "item" && (r.Label == "Land" || r.Label == "Loans") {
			t.Fatalf("non-cash land/loan leaked into flows: %+v", r)
		}
	}
	// json is machine-readable with numeric values
	var buf bytes.Buffer
	if err := cf.Render(&buf, "json"); err != nil {
		t.Fatal(err)
	}
	var back struct {
		Columns []report.Column
		Rows    []struct {
			Kind   string
			Label  string
			Values []float64
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Columns) != 5 || len(back.Rows) == 0 {
		t.Fatalf("json round trip: %d columns %d rows", len(back.Columns), len(back.Rows))
	}
	buf.Reset()
	if err := bs.Render(&buf, "csv"); err != nil || !strings.HasPrefix(buf.String(), "kind,section,group,label,key,") {
		t.Fatalf("csv header: %v %q", err, buf.String()[:40])
	}
}

func TestHumanize(t *testing.T) {
	for in, want := range map[string]string{"XMoney": "X Money", "SoFi": "SoFi", "RewardsCard-1000": "Rewards Card 1000",
		"Realized-Gains": "Realized Gains", "RothIRA-1234": "Roth IRA 1234", "GT500": "GT500"} {
		if got := report.Humanize(in); got != want {
			t.Errorf("Humanize(%q) = %q, want %q", in, got, want)
		}
	}
}
