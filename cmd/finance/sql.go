package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/p-ahl/personal-finance/internal/download/common"
	"github.com/p-ahl/personal-finance/internal/sqlq"
)

const sqlUsage = `usage: finance sql [flags] "SELECT ..."      (or SQL on stdin, or -f file.sql)
       finance sql --tables | --schema <name> | --queries | --q <name> [--param k=v ...]

The database is opened READ-ONLY (mode=ro + PRAGMA query_only); writes are rejected.
Named placeholders (:name, @name, $name) bind --param values; unset ones bind NULL.

Flags:
  -f file.sql            read SQL from a file ("-" = stdin)
  --format F             table (default) | csv | json | md
  --max-width N          table: truncate cells to N characters (default 60, 0 = off)
  --max-rows N           table: show at most N rows (default 1000, 0 = all)
  --no-header            csv: omit the header row
  --tables               list tables and views (all attached schemas) with row counts
  --schema NAME          CREATE statement and columns of a table/view (NAME or schema.NAME)
  --q NAME               run saved query NAME from <repo>/queries or $FINANCE_DATA/queries
  --queries              list saved queries and their params
  --param k=v            bind :k (repeatable)
  --attach name=path     attach another SQLite db read-only as schema "name" (repeatable;
                         also FINANCE_ATTACH="name=path,name2=path2")
  --write                open read-write (maintenance only; journal triggers still apply)
  --db PATH              database (default $FINANCE_DB or $FINANCE_DATA/finance.db)
  --repo DIR             repo root holding queries/ (default $FINANCE_REPO or .)
`

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// parseInterleaved parses flags that may appear before or after positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func runSQL(args []string) int {
	return sqlMain(args, os.Stdin, os.Stdout, os.Stderr)
}

func stdinPiped(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return r != nil
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice == 0
}

func sqlMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sql", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, sqlUsage) }
	p := addPathFlags(fs)
	file := fs.String("f", "", "SQL file")
	format := fs.String("format", "table", "table | csv | json | md")
	maxWidth := fs.Int("max-width", sqlq.DefaultRender.MaxWidth, "table cell width limit")
	maxRows := fs.Int("max-rows", sqlq.DefaultRender.MaxRows, "table row limit")
	noHeader := fs.Bool("no-header", false, "csv without header")
	tables := fs.Bool("tables", false, "list tables and views")
	schema := fs.String("schema", "", "show a table/view definition")
	saved := fs.String("q", "", "saved query name")
	listQ := fs.Bool("queries", false, "list saved queries")
	write := fs.Bool("write", false, "open read-write")
	var params, attaches multiFlag
	fs.Var(&params, "param", "k=v (repeatable)")
	fs.Var(&attaches, "attach", "name=path (repeatable)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return common.ExitConfig
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	usage := func(msg string) int {
		fmt.Fprintln(stderr, "finance sql:", msg)
		fmt.Fprint(stderr, sqlUsage)
		return common.ExitConfig
	}
	if *format == "markdown" {
		*format = "md"
	}
	if !sqlq.ValidFormat(*format) {
		return usage("unknown --format " + *format)
	}
	ro := sqlq.RenderOptions{Format: *format, MaxWidth: *maxWidth, MaxRows: *maxRows, Header: !*noHeader}

	qdirs := []string{filepath.Join(p.repo, "queries"), filepath.Join(dataDir(), "queries")}
	if *listQ {
		qs, err := sqlq.ListQueries(qdirs)
		if err != nil {
			return fail(err)
		}
		r := &sqlq.Result{Columns: []string{"name", "description", "params", "path"}}
		for _, q := range qs {
			r.Rows = append(r.Rows, []any{q.Name, q.Description, strings.Join(q.Params, "; "), q.Path})
		}
		if err := sqlq.Render(stdout, r, sqlq.RenderOptions{Format: *format, MaxWidth: 0, Header: true}); err != nil {
			return fail(err)
		}
		return 0
	}

	pm := map[string]string{}
	for _, kv := range params {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimLeft(strings.TrimSpace(k), ":@$")
		if !ok || k == "" {
			return usage("--param wants k=v, got " + kv)
		}
		pm[k] = v
	}

	modes := 0
	for _, b := range []bool{*tables, *schema != "", *saved != "", *file != "", len(pos) > 0} {
		if b {
			modes++
		}
	}
	if modes > 1 {
		return usage("give only one of: SQL text, -f, --q, --tables, --schema")
	}
	var script string
	switch {
	case *saved != "":
		q, err := sqlq.FindQuery(*saved, qdirs)
		if err != nil {
			return fail(err)
		}
		script = q.SQL
	case *file == "-" || (len(pos) == 1 && pos[0] == "-"):
		b, err := io.ReadAll(stdin)
		if err != nil {
			return fail(err)
		}
		script = string(b)
	case *file != "":
		b, err := os.ReadFile(*file)
		if err != nil {
			return fail(err)
		}
		script = string(b)
	case len(pos) > 0:
		script = strings.Join(pos, " ")
	case *tables || *schema != "":
	default:
		if !stdinPiped(stdin) {
			return usage("no SQL given")
		}
		b, err := io.ReadAll(stdin)
		if err != nil {
			return fail(err)
		}
		script = string(b)
	}

	var att []sqlq.Attach
	if env := os.Getenv("FINANCE_ATTACH"); env != "" {
		a, err := sqlq.ParseAttachList(env)
		if err != nil {
			return usage("FINANCE_ATTACH: " + err.Error())
		}
		att = append(att, a...)
	}
	for _, s := range attaches {
		a, err := sqlq.ParseAttach(s)
		if err != nil {
			return usage(err.Error())
		}
		att = append(att, a)
	}
	if *write {
		fmt.Fprintln(stderr, "finance sql: --write: database opened READ-WRITE")
	}
	sess, err := sqlq.Open(p.db, sqlq.Options{Write: *write, Attach: att})
	if err != nil {
		return fail(err)
	}
	defer sess.Close()

	switch {
	case *tables:
		r, err := sess.Tables()
		if err != nil {
			return fail(err)
		}
		if err := sqlq.Render(stdout, r, ro); err != nil {
			return fail(err)
		}
		return 0
	case *schema != "":
		ddl, cols, err := sess.Schema(*schema)
		if err != nil {
			return fail(err)
		}
		switch ro.Format {
		case "table":
			fmt.Fprintln(stdout, strings.TrimSpace(ddl)+";")
			fmt.Fprintln(stdout)
		case "md":
			fmt.Fprintf(stdout, "```sql\n%s;\n```\n\n", strings.TrimSpace(ddl))
		}
		if err := sqlq.Render(stdout, cols, ro); err != nil {
			return fail(err)
		}
		return 0
	}
	rs, err := sess.Run(script, pm)
	if rerr := sqlq.RenderAll(stdout, rs, ro); rerr != nil && err == nil {
		err = rerr
	}
	if err != nil {
		return fail(err)
	}
	return 0
}
