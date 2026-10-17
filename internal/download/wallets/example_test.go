package wallets

import "testing"

// The committed template must stay loadable (placeholders only).
func TestExampleConfigLoads(t *testing.T) {
	c, err := LoadConfig("../../../config/wallets.data.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Wallets) != 3 || len(c.Passthrough) != 1 {
		t.Fatalf("example: %d wallets, %d pass-through windows", len(c.Wallets), len(c.Passthrough))
	}
}
