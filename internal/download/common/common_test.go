package common

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestImportDir(t *testing.T) {
	day := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	got := ImportDir("/repo", "kaspa", day)
	want := filepath.Join("/repo", "imports", "2026-09-11", "kaspa")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestAtomicWriteJSONIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	payload := map[string]any{"a": 1, "b": "x"}

	skipped, err := AtomicWriteJSON(path, payload)
	if err != nil || skipped {
		t.Fatalf("first write: skipped=%v err=%v", skipped, err)
	}
	skipped, err = AtomicWriteJSON(path, payload)
	if err != nil || !skipped {
		t.Fatalf("second write: skipped=%v err=%v", skipped, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["a"].(float64) != 1 {
		t.Fatalf("decoded=%v", decoded)
	}
}
