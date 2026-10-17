// Package common provides shared helpers for financial-institution downloaders.
//
// Exit codes used by download commands:
//
//	0 — success (including idempotent skip of unchanged files)
//	2 — configuration / usage error (missing config, bad args)
//	3 — network / remote API / I/O failure while downloading
package common

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	ExitOK           = 0
	ExitConfig       = 2
	ExitDownloadFail = 3
)

// ImportDir returns imports/YYYY-MM-DD/<fi>/ under repoRoot for today (local date).
func ImportDir(repoRoot, fi string, day time.Time) string {
	if day.IsZero() {
		day = time.Now()
	}
	return filepath.Join(repoRoot, "imports", day.Format("2006-01-02"), fi)
}

// EnsureDir creates dir with 0755.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// AtomicWriteJSON marshals v as indented JSON and writes atomically via temp+rename.
// If a file already exists with the same SHA-256 of the payload, it is left untouched
// and skipped is true (idempotent checksum skip).
func AtomicWriteJSON(path string, v any) (skipped bool, err error) {
	payload, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return false, fmt.Errorf("marshal json: %w", err)
	}
	payload = append(payload, '\n')

	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])

	if existing, rerr := os.ReadFile(path); rerr == nil {
		got := sha256.Sum256(existing)
		if hex.EncodeToString(got[:]) == want {
			return true, nil
		}
	}

	dir := filepath.Dir(path)
	if err := EnsureDir(dir); err != nil {
		return false, err
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return false, fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName) // no-op if renamed
	}()

	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("rename: %w", err)
	}
	return false, nil
}

// ChecksumHex returns hex SHA-256 of data.
func ChecksumHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RepoRoot walks up from start (or cwd) looking for go.mod.
func RepoRoot(start string) (string, error) {
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %s", start)
		}
		dir = parent
	}
}
