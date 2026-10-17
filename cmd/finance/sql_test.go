package main

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/ledger"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

func fixtureBooks(t *testing.T) string {
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
	path := filepath.Join(root, "finance.db")
	db, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := ledger.Import(db, cfg, ex, dir); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSQLCommand(t *testing.T) {
	dbPath := fixtureBooks(t)
	t.Setenv("FINANCE_DATA", t.TempDir())
	t.Setenv("FINANCE_ATTACH", "")
	base := []string{"--db", dbPath, "--repo", filepath.Join("..", "..")}
	run := func(stdin string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := sqlMain(append(append([]string{}, base...), args...), strings.NewReader(stdin), &out, &errb)
		return code, out.String(), errb.String()
	}

	code, out, errs := run("", "--tables", "--format", "csv")
	recs, _ := csv.NewReader(strings.NewReader(out)).ReadAll()
	if code != 0 || len(recs) < 30 || !strings.Contains(out, "main,v_ledger,view,") || !strings.Contains(out, "main,journal_entries,table,") {
		t.Fatalf("--tables: %d %s %s", code, out, errs)
	}

	// SQL as an argument, flags after it
	code, out, _ = run("", "SELECT COUNT(*) AS n FROM raw_transactions", "--format", "csv")
	if code != 0 || out != "n\n16\n" {
		t.Fatalf("arg query: %d %q", code, out)
	}
	// from stdin
	code, out, _ = run("SELECT :x AS x;", "--param", "x=hi", "--format", "json")
	if code != 0 || !strings.Contains(out, `"x": "hi"`) {
		t.Fatalf("stdin query: %d %q", code, out)
	}
	// from a file
	f := filepath.Join(t.TempDir(), "q.sql")
	os.WriteFile(f, []byte("-- count\nSELECT COUNT(*) AS entries FROM journal_entries;\n"), 0o644)
	code, out, _ = run("", "-f", f)
	if code != 0 || !strings.Contains(out, "entries") || !strings.Contains(out, "(1 row)") {
		t.Fatalf("-f: %d %q", code, out)
	}
	// saved query with a param
	code, out, errs = run("", "--q", "month-spend", "--param", "month=2026-01", "--format", "md")
	if code != 0 || !strings.Contains(out, "| month | category |") || !strings.Contains(out, "TOTAL") {
		t.Fatalf("--q month-spend: %d %q %s", code, out, errs)
	}
	code, out, _ = run("", "--queries", "--format", "csv")
	if code != 0 || !strings.Contains(out, "payables,") || !strings.Contains(out, "recent,") {
		t.Fatalf("--queries: %d %q", code, out)
	}
	code, out, _ = run("", "--schema", "raw_transactions")
	if code != 0 || !strings.Contains(out, "CREATE TABLE raw_transactions") || !strings.Contains(out, "connector_txn_id") {
		t.Fatalf("--schema: %d %q", code, out)
	}
	// writes are rejected and leave the db untouched
	code, _, errs = run("", "DELETE FROM journal_lines")
	if code != 1 || !strings.Contains(errs, "write rejected") {
		t.Fatalf("write: %d %s", code, errs)
	}
	code, out, _ = run("", "SELECT COUNT(*) FROM journal_lines", "--format", "csv", "--no-header")
	if code != 0 || out == "0\n" {
		t.Fatalf("journal_lines after rejected delete: %q", out)
	}
	// attach via env
	t.Setenv("FINANCE_ATTACH", "copy="+dbPath)
	code, out, errs = run("", "SELECT COUNT(*) AS n FROM copy.raw_transactions", "--format", "csv")
	if code != 0 || out != "n\n16\n" {
		t.Fatalf("FINANCE_ATTACH: %d %q %s", code, out, errs)
	}
	// usage errors
	if code, _, _ = run("", "--format", "xml", "SELECT 1"); code != 2 {
		t.Fatalf("bad format exit %d", code)
	}
	if code, _, _ = run("", "--tables", "SELECT 1"); code != 2 {
		t.Fatalf("two modes exit %d", code)
	}
}
