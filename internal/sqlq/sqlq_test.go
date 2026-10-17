package sqlq_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/ledger"
	"github.com/p-ahl/personal-finance/internal/sqlq"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

// fixtureDB builds a books database from the synthetic extract (fake data only).
func fixtureDB(t *testing.T) string {
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

func TestSplitAndParams(t *testing.T) {
	got := sqlq.Split(`-- leading comment
SELECT 'a;b' AS x, ":notparam" AS "y;z" FROM t WHERE m = :month; /* c; */ ;
SELECT @p, $q, :month -- trailing; comment
`)
	if len(got) != 2 || !strings.Contains(got[0], "'a;b'") || !strings.Contains(got[1], "SELECT @p") {
		t.Fatalf("split: %q", got)
	}
	if n := sqlq.ParamNames(got[0]); !reflect.DeepEqual(n, []string{"month"}) {
		t.Fatalf("params 0: %v", n)
	}
	if n := sqlq.ParamNames(got[len(got)-1]); !reflect.DeepEqual(n, []string{"p", "q", "month"}) {
		t.Fatalf("params 1: %v", n)
	}
	if len(sqlq.Split("  -- only a comment\n ; ")) != 0 {
		t.Fatal("comment-only script should have no statements")
	}
}

func TestReadOnlyAndAttach(t *testing.T) {
	path := fixtureDB(t)
	other := filepath.Join(t.TempDir(), "other.db")
	w, err := sqlq.Open(fixtureDB(t), sqlq.Options{Write: true}) // a second synthetic db to attach
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Run(`VACUUM INTO '`+other+`'`, nil); err != nil {
		t.Fatal(err)
	}
	w.Close()

	s, err := sqlq.Open(path, sqlq.Options{Attach: []sqlq.Attach{{Name: "o", Path: other}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		`DELETE FROM import_runs`,
		`INSERT INTO o.import_runs(source) VALUES ('x')`,
		`CREATE TABLE zz(x)`,
		`ATTACH '` + filepath.Join(t.TempDir(), "new.db") + `' AS n`,
		`PRAGMA query_only = 0`,
	} {
		if _, err := s.Run(q, nil); err == nil || !strings.Contains(err.Error(), "write rejected") {
			t.Fatalf("%s: want write rejected, got %v", q, err)
		}
	}
	rs, err := s.Run(`SELECT (SELECT COUNT(*) FROM journal_entries) = (SELECT COUNT(*) FROM o.journal_entries) AS same`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Rows[0][0] != int64(1) {
		t.Fatalf("cross-db join: %v", rs[0].Rows)
	}
	tb, err := s.Tables()
	if err != nil {
		t.Fatal(err)
	}
	schemas := map[string]bool{}
	for _, r := range tb.Rows {
		schemas[r[0].(string)] = true
	}
	if !schemas["main"] || !schemas["o"] {
		t.Fatalf("tables: schemas %v", schemas)
	}
	ddl, cols, err := s.Schema("o.v_ledger")
	if err != nil || !strings.Contains(ddl, "CREATE VIEW v_ledger") || len(cols.Rows) < 30 {
		t.Fatalf("schema: %v %d", err, len(cols.Rows))
	}
	if _, err := s.Run(`SELECT :a`, map[string]string{"a": "1", "typo": "2"}); err == nil || !strings.Contains(err.Error(), "typo") {
		t.Fatalf("unused param should error, got %v", err)
	}
	if _, err := sqlq.ParseAttach("main=/x"); err == nil {
		t.Fatal("attach as main should be rejected")
	}
	if a, err := sqlq.ParseAttachList("health=/a/h.db, x=/b.db"); err != nil || len(a) != 2 || a[0].Name != "health" {
		t.Fatalf("attach list: %v %v", a, err)
	}
}

func TestRenderFormats(t *testing.T) {
	r := &sqlq.Result{Columns: []string{"name", "n", "amt", "note"}, Rows: [][]any{
		{"alpha", int64(3), 1.5, nil},
		{"a|b", int64(-12), -0.25, strings.Repeat("x", 80)},
	}}
	var b bytes.Buffer
	if err := sqlq.Render(&b, r, sqlq.RenderOptions{Format: "table", MaxWidth: 10}); err != nil {
		t.Fatal(err)
	}
	want := "name     n    amt  note\n" +
		"-----  ---  -----  ----------\n" +
		"alpha    3    1.5  NULL\n" +
		"a|b    -12  -0.25  xxxxxxxxx…\n" +
		"(2 rows)\n"
	if b.String() != want {
		t.Fatalf("table:\n%s\nwant:\n%s", b.String(), want)
	}
	b.Reset()
	sqlq.Render(&b, r, sqlq.RenderOptions{Format: "csv", Header: true})
	if !strings.HasPrefix(b.String(), "name,n,amt,note\nalpha,3,1.5,\n") {
		t.Fatalf("csv: %q", b.String())
	}
	b.Reset()
	sqlq.Render(&b, r, sqlq.RenderOptions{Format: "json"})
	var js []map[string]any
	if err := json.Unmarshal(b.Bytes(), &js); err != nil || len(js) != 2 || js[0]["note"] != nil || js[1]["n"] != float64(-12) {
		t.Fatalf("json: %v %s", err, b.String())
	}
	if !strings.HasPrefix(b.String(), "[\n  {\"name\": \"alpha\", \"n\": 3, \"amt\": 1.5, \"note\": null}") {
		t.Fatalf("json column order: %s", b.String())
	}
	b.Reset()
	sqlq.Render(&b, r, sqlq.RenderOptions{Format: "md"})
	if !strings.Contains(b.String(), "|---|---:|---:|---|") || !strings.Contains(b.String(), `a\|b`) {
		t.Fatalf("md: %s", b.String())
	}
	b.Reset()
	sqlq.Render(&b, r, sqlq.RenderOptions{Format: "table", MaxRows: 1})
	if !strings.Contains(b.String(), "(2 rows; showing first 1") {
		t.Fatalf("max rows: %s", b.String())
	}
}

// Every saved query in the repo's queries/ dir runs against the synthetic books.
func TestSavedQueries(t *testing.T) {
	path := fixtureDB(t)
	dirs := []string{filepath.Join("..", "..", "queries")}
	qs, err := sqlq.ListQueries(dirs)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, q := range qs {
		names[q.Name] = true
		if q.Description == "" {
			t.Errorf("%s: no description comment", q.Name)
		}
	}
	for _, n := range []string{"recent", "month-spend", "payables"} {
		if !names[n] {
			t.Fatalf("saved query %s missing (have %v)", n, names)
		}
	}
	s, err := sqlq.Open(path, sqlq.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range qs {
		if _, err := s.Run(q.SQL, nil); err != nil {
			t.Errorf("%s: %v", q.Name, err)
		}
	}
	q, _ := sqlq.FindQuery("month-spend", dirs)
	rs, err := s.Run(q.SQL, map[string]string{"month": "2026-01"})
	if err != nil {
		t.Fatal(err)
	}
	rows := rs[0].Rows
	last := rows[len(rows)-1]
	if len(rows) < 2 || last[1] != "TOTAL" || last[0] != "2026-01" {
		t.Fatalf("month-spend rows: %v", rows)
	}
	var sum float64
	for _, r := range rows[:len(rows)-1] {
		sum += r[3].(float64)
	}
	if d := sum - last[3].(float64); d > 0.005 || d < -0.005 {
		t.Fatalf("month-spend categories %v do not add up to total %v", sum, last[3])
	}
	q, _ = sqlq.FindQuery("payables", dirs)
	rs, err = s.Run(q.SQL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs[0].Rows) == 0 {
		t.Fatal("payables: the synthetic accruals leave at least one open item")
	}
	q, _ = sqlq.FindQuery("recent", dirs)
	rs, err = s.Run(q.SQL, map[string]string{"days": "100000", "account": "Expenses:Food"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs[0].Rows {
		if !strings.HasPrefix(r[1].(string), "Expenses:Food") {
			t.Fatalf("recent account filter: %v", r)
		}
	}
	if len(rs[0].Rows) == 0 {
		t.Fatal("recent with a long window should return the fixture's food lines")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
