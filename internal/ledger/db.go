// Package ledger owns the SQLite source-of-truth database: connection, migrations, writes.
package ledger

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens (creating if needed) the database with foreign keys and WAL enabled.
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; keeps pragmas on one connection
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Migration is one embedded, versioned schema file (NNNN_name.sql).
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations returns the embedded migrations in version order.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("migration %s: bad version prefix", name)
		}
		b, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: name, SQL: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Migrate applies pending migrations; returns the names applied.
func Migrate(db *sql.DB) ([]string, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')))`); err != nil {
		return nil, err
	}
	ms, err := Migrations()
	if err != nil {
		return nil, err
	}
	var applied []string
	for _, m := range ms {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.Version).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(m.SQL); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("migration %s: %w", m.Name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, name) VALUES (?, ?)`, m.Version, m.Name); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		applied = append(applied, m.Name)
	}
	return applied, nil
}
