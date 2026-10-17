package books_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/p-ahl/personal-finance/internal/books"
)

// The shipped example files must load (they are the fallback when no personal files exist).
func TestShippedExamplesLoad(t *testing.T) {
	root := filepath.Join("..", "..")
	cfg, err := books.LoadConfig(
		filepath.Join(root, "config", "coa.yaml"),
		filepath.Join(root, "config", "accounts.example.yaml"),
		filepath.Join(root, "rules", "categories.example.yaml"),
		"",
		filepath.Join(root, "rules", "transfers.example.yaml"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules.Rules) == 0 || len(cfg.Transfers.Hints) == 0 {
		t.Fatalf("example rules empty: %d rules, %d hints", len(cfg.Rules.Rules), len(cfg.Transfers.Hints))
	}
	if err := cfg.LoadCOAOverlay(filepath.Join(root, "config", "coa.local.example.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestCOAOverlayMergesAndReplaces(t *testing.T) {
	root := filepath.Join("..", "..")
	cfg, err := books.LoadConfig(
		filepath.Join(root, "config", "coa.yaml"),
		filepath.Join(root, "config", "accounts.example.yaml"),
		filepath.Join(root, "rules", "categories.example.yaml"), "",
		filepath.Join(root, "rules", "transfers.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	n := len(cfg.COA.Accounts)
	ov := filepath.Join(t.TempDir(), "coa.local.yaml")
	os.WriteFile(ov, []byte("accounts:\n  - {name: Income:Extra, report_line: Extra}\n  - {name: Expenses:Food, report_line: Groceries etc}\n"), 0o644)
	if err := cfg.LoadCOAOverlay(ov); err != nil {
		t.Fatal(err)
	}
	if len(cfg.COA.Accounts) != n+1 {
		t.Fatalf("accounts %d, want %d (one new, one replaced)", len(cfg.COA.Accounts), n+1)
	}
	if err := cfg.LoadCOAOverlay(filepath.Join(t.TempDir(), "missing.yaml")); err != nil {
		t.Fatalf("missing overlay must be ignored: %v", err)
	}
}
