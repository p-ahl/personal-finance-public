package connector_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

func TestReadDedupAndPendingConflict(t *testing.T) {
	dir := testfixture.Extract(t, t.TempDir())
	ex, err := connector.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ex.AsOf != testfixture.AsOf {
		t.Fatalf("as-of from folder name: got %q", ex.AsOf)
	}
	if ex.PageRows != 17 || len(ex.Txns) != 15 {
		t.Fatalf("page rows %d unique %d, want 17 / 15", ex.PageRows, len(ex.Txns))
	}
	if len(ex.Conflicts) != 1 || ex.Conflicts[0] != "t-pend" {
		t.Fatalf("conflicts = %v, want [t-pend]", ex.Conflicts)
	}
	for _, tx := range ex.Txns {
		if tx.ID == "t-pend" && tx.Pending {
			t.Fatal("posted version should win over pending for the same id")
		}
		if tx.ID == "t-gro" && (tx.Kind != "liability" || tx.AccountID != testfixture.Card || tx.CategoryDetailed != "FOOD_AND_DRINK_GROCERIES") {
			t.Fatalf("liability row parsed wrong: %+v", tx)
		}
	}
	if len(ex.Holdings) != 2 || len(ex.PortfolioTxns) != 1 || len(ex.Accounts) != 6 {
		t.Fatalf("holdings %d portfolio %d accounts %d", len(ex.Holdings), len(ex.PortfolioTxns), len(ex.Accounts))
	}
}

func TestReadJSONPages(t *testing.T) {
	root := t.TempDir()
	dir := testfixture.Nightly(t, root)
	ex, err := connector.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ex.AsOf != testfixture.NightlyAsOf || ex.PageRows != 3 || len(ex.Txns) != 3 {
		t.Fatalf("json pages: as-of %s rows %d unique %d", ex.AsOf, ex.PageRows, len(ex.Txns))
	}
	// a truncated tool result (row_count disagrees with the csv) is an error
	bad := `{"format":"csv","csv":"id,account_id,date,amount\nx,acc-checking,2026-02-01,1.00\n","row_count":2,"next_cursor":null}`
	if err := os.WriteFile(filepath.Join(dir, "cash_txns_page002.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := connector.Read(dir); err == nil || !strings.Contains(err.Error(), "row_count") {
		t.Fatalf("want row_count error, got %v", err)
	}
	// a .csv twin takes precedence over the .json result
	if err := os.WriteFile(filepath.Join(dir, "cash_txns_page002.csv"), []byte("id,account_id,date,amount\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := connector.Read(dir); err != nil {
		t.Fatal(err)
	}
}

// Rows without a connector id get a stable synthetic key: re-reading the file gives the same
// keys, a page that repeats a row collapses onto it, and two genuinely identical rows in one
// file (two tickets) stay two rows.
func TestSyntheticIDsForIDlessRows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026-03-01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"connections.json": `{"connections":[{"id":"c1","provider":"csv","institution_id":"i","institution_name":"Bank","status":"ok"}]}`,
		"accounts.json":    `{"accounts":[{"id":"a1","connection_id":"c1","class":"cash","name":"Checking","type":"depository","subtype":"checking","current_balance":"0","currency":"USD"}]}`,
		"cash_txns_page001.csv": "id,account_id,date,amount,name,pending\n" +
			",a1,2026-02-01,-250.0000,AIRLINE TICKET,false\n" +
			",a1,2026-02-01,-250.00,Airline  Ticket,false\n" +
			",a1,2026-02-02,-4.00,COFFEE,false\n",
		// overlapping page: repeats one ticket and the coffee
		"cash_txns_page002.csv": "id,account_id,date,amount,name,pending\n" +
			",a1,2026-02-01,-250.00,AIRLINE TICKET,false\n" +
			",a1,2026-02-02,-4.0000,COFFEE,false\n",
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ids := func() []string {
		ex, err := connector.Read(dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, tx := range ex.Txns {
			if !strings.HasPrefix(tx.ID, "h-") || tx.Raw["id_synthetic"] != "true" {
				t.Fatalf("row without a synthetic id: %+v", tx)
			}
			out = append(out, tx.ID)
		}
		return out
	}
	a, b := ids(), ids()
	if len(a) != 3 {
		t.Fatalf("want 3 rows (2 tickets + coffee; the overlap collapses), got %d: %v", len(a), a)
	}
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("synthetic ids not stable: %v vs %v", a, b)
	}
}
