// Package sqlq runs ad-hoc SQL against the books database (read-only by default) and renders
// result sets as aligned tables, CSV, JSON or Markdown. It backs `finance sql`.
package sqlq

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"
)

// Attach is an extra database attached under a schema name (always read-only).
type Attach struct{ Name, Path string }

// Options for Open.
type Options struct {
	Write  bool     // open read-write (default: read-only + query_only)
	Attach []Attach // extra databases, attached read-only
}

// Session is one pinned SQLite connection: attachments and pragmas persist across statements.
type Session struct {
	db       *sql.DB
	conn     *sql.Conn
	ReadOnly bool
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// fileURI builds a SQLite URI filename (paths may contain '?', '#' or '%').
func fileURI(path string, ro bool) string {
	r := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")
	u := "file:" + r.Replace(path)
	if ro {
		u += "?mode=ro"
	}
	return u
}

// ParseAttach parses "name=path".
func ParseAttach(s string) (Attach, error) {
	name, path, ok := strings.Cut(strings.TrimSpace(s), "=")
	name, path = strings.TrimSpace(name), strings.TrimSpace(path)
	if !ok || name == "" || path == "" {
		return Attach{}, fmt.Errorf("--attach %q: want name=path", s)
	}
	if !identRe.MatchString(name) || strings.EqualFold(name, "main") || strings.EqualFold(name, "temp") {
		return Attach{}, fmt.Errorf("--attach %q: schema name must be an identifier other than main/temp", s)
	}
	return Attach{Name: name, Path: path}, nil
}

// ParseAttachList parses a comma-separated list of name=path (the FINANCE_ATTACH format).
func ParseAttachList(s string) ([]Attach, error) {
	var out []Attach
	for _, part := range strings.Split(s, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		a, err := ParseAttach(part)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// Open opens path (which must exist) on a single pinned connection. Read-only mode opens the
// file with mode=ro and sets PRAGMA query_only, so any write (to main or an attachment) fails.
func Open(path string, o Options) (*Session, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	dsn := fileURI(path, !o.Write)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn += sep + "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Session{db: db, conn: conn, ReadOnly: !o.Write}
	seen := map[string]bool{}
	for _, a := range o.Attach {
		if seen[strings.ToLower(a.Name)] {
			s.Close()
			return nil, fmt.Errorf("attach %s: duplicate schema name", a.Name)
		}
		seen[strings.ToLower(a.Name)] = true
		if _, err := os.Stat(a.Path); err != nil {
			s.Close()
			return nil, fmt.Errorf("attach %s: %w", a.Name, err)
		}
		if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS "`+a.Name+`"`, fileURI(a.Path, true)); err != nil {
			s.Close()
			return nil, fmt.Errorf("attach %s=%s: %w", a.Name, a.Path, err)
		}
	}
	if !o.Write {
		if _, err := conn.ExecContext(ctx, `PRAGMA query_only = 1`); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Session) Close() error {
	if s.conn != nil {
		s.conn.Close()
	}
	return s.db.Close()
}

// Result is one statement's result set.
type Result struct {
	SQL     string
	Columns []string
	Rows    [][]any
}

// Run executes every statement of script in order and returns the result sets of statements
// that produce columns. params bind :name / @name / $name placeholders; placeholders without
// a value bind NULL (saved queries use COALESCE(:p, default)). A param that no statement
// uses is an error, to catch typos.
func (s *Session) Run(script string, params map[string]string) ([]*Result, error) {
	stmts := Split(script)
	if len(stmts) == 0 {
		return nil, fmt.Errorf("no SQL statement given")
	}
	used := map[string]bool{}
	var out []*Result
	for _, st := range stmts {
		if s.ReadOnly {
			if err := guardReadOnly(st); err != nil {
				return out, err
			}
		}
		var args []any
		for _, n := range ParamNames(st) {
			used[n] = true
			if v, ok := params[n]; ok {
				args = append(args, sql.Named(n, v))
			} else {
				args = append(args, sql.Named(n, nil))
			}
		}
		r, err := s.query(st, args)
		if err != nil {
			return out, s.explain(err, st)
		}
		if len(r.Columns) > 0 {
			out = append(out, r)
		}
	}
	var unused []string
	for k := range params {
		if !used[k] {
			unused = append(unused, k)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		return out, fmt.Errorf("--param not used by the query: %s", strings.Join(unused, ", "))
	}
	return out, nil
}

func (s *Session) explain(err error, st string) error {
	msg := err.Error()
	if s.ReadOnly && (strings.Contains(msg, "readonly") || strings.Contains(msg, "read-only") || strings.Contains(msg, "query_only")) {
		return fmt.Errorf("write rejected: the database is opened read-only (%v)", err)
	}
	short := strings.Join(strings.Fields(st), " ")
	if len(short) > 120 {
		short = short[:117] + "..."
	}
	return fmt.Errorf("%v\n  in: %s", err, short)
}

func (s *Session) query(st string, args []any) (*Result, error) {
	rows, err := s.conn.QueryContext(context.Background(), st, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	r := &Result{SQL: st, Columns: cols}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			switch x := v.(type) {
			case []byte:
				vals[i] = string(x)
			case time.Time: // driver converts DATE/DATETIME-typed columns
				if x.Hour() == 0 && x.Minute() == 0 && x.Second() == 0 && x.Nanosecond() == 0 {
					vals[i] = x.Format("2006-01-02")
				} else {
					vals[i] = x.Format(time.RFC3339)
				}
			}
		}
		r.Rows = append(r.Rows, vals)
	}
	return r, rows.Err()
}

// Tables lists tables and views in every attached schema, with row counts for tables.
func (s *Session) Tables() (*Result, error) {
	schemas, err := s.schemas()
	if err != nil {
		return nil, err
	}
	r := &Result{Columns: []string{"schema", "name", "type", "rows", "columns"}}
	ctx := context.Background()
	for _, sc := range schemas {
		q := `SELECT name, type FROM ` + quoteIdent(sc) + `.sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY type, name`
		res, err := s.query(q, nil)
		if err != nil {
			return nil, err
		}
		for _, row := range res.Rows {
			name, typ := row[0].(string), row[1].(string)
			var n any
			if typ == "table" {
				var c int64
				if err := s.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+quoteIdent(sc)+`.`+quoteIdent(name)).Scan(&c); err == nil {
					n = c
				}
			}
			var ncol int64
			s.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?, ?)`, name, sc).Scan(&ncol)
			r.Rows = append(r.Rows, []any{sc, name, typ, n, ncol})
		}
	}
	return r, nil
}

func (s *Session) schemas() ([]string, error) {
	res, err := s.query(`SELECT name FROM pragma_database_list ORDER BY seq`, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, row := range res.Rows {
		n := row[0].(string)
		if n == "temp" {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// Schema returns the CREATE statement and the columns of a table or view ("name" or "schema.name").
func (s *Session) Schema(name string) (string, *Result, error) {
	schema, obj := "main", name
	if a, b, ok := strings.Cut(name, "."); ok {
		schema, obj = a, b
	}
	if !identRe.MatchString(schema) {
		return "", nil, fmt.Errorf("bad schema name %q", schema)
	}
	var ddl sql.NullString
	var typ string
	err := s.conn.QueryRowContext(context.Background(),
		`SELECT type, sql FROM `+quoteIdent(schema)+`.sqlite_master WHERE name = ? COLLATE NOCASE AND type IN ('table','view')`, obj).Scan(&typ, &ddl)
	if err == sql.ErrNoRows {
		return "", nil, fmt.Errorf("no table or view named %q (try `finance sql --tables`)", name)
	}
	if err != nil {
		return "", nil, err
	}
	cols, err := s.query(`SELECT cid, name, type, "notnull" AS not_null, dflt_value AS "default", pk FROM pragma_table_xinfo(?, ?) ORDER BY cid`,
		[]any{obj, schema})
	if err != nil {
		return "", nil, err
	}
	return ddl.String, cols, nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// guardReadOnly rejects statements that could create files or lift the read-only guard:
// ATTACH (a missing file would be created; use --attach), DETACH, VACUUM [INTO] and
// PRAGMA query_only / writable_schema.
func guardReadOnly(st string) error {
	var code strings.Builder
	scan(st, func(kind int, text string) {
		if kind == tCode {
			code.WriteString(text)
		} else if kind == tString {
			code.WriteString(" '' ")
		}
	})
	f := strings.Fields(strings.ToUpper(code.String()))
	if len(f) == 0 {
		return nil
	}
	switch f[0] {
	case "ATTACH", "DETACH", "VACUUM":
		return fmt.Errorf("write rejected: %s is not allowed in read-only mode (use --attach name=path)", f[0])
	case "PRAGMA":
		u := strings.Join(f, " ")
		if strings.Contains(u, "QUERY_ONLY") || strings.Contains(u, "WRITABLE_SCHEMA") {
			return fmt.Errorf("write rejected: changing query_only/writable_schema is not allowed in read-only mode")
		}
	}
	return nil
}
