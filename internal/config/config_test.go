package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndFetchable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wallets.yaml")
	content := `
kaspa:
  wallets:
    - name: Tangem
      address: kaspa:qyp9test
    - name: Kaspium
      kpub: kpubABC
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	fetchable := root.Kaspa.FetchableAddresses()
	if len(fetchable) != 1 || fetchable[0].Name != "Tangem" {
		t.Fatalf("fetchable=%v", fetchable)
	}
	kpubOnly := root.Kaspa.KPubOnly()
	if len(kpubOnly) != 1 || kpubOnly[0].Name != "Kaspium" {
		t.Fatalf("kpubOnly=%v", kpubOnly)
	}
}
