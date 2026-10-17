package books_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

func loadFixture(t *testing.T, localRules string) (*connector.Extract, *books.Config) {
	t.Helper()
	root := t.TempDir()
	ex, err := connector.Read(testfixture.Extract(t, root))
	if err != nil {
		t.Fatal(err)
	}
	p := testfixture.Config(t, root)
	if localRules != "" {
		if err := os.WriteFile(filepath.Join(root, "rules.local.yaml"), []byte(localRules), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := books.LoadConfig(p.COA, p.Map, p.Rules, p.LocalRules, p.Transfers)
	if err != nil {
		t.Fatal(err)
	}
	return ex, cfg
}

func buildEx(t *testing.T, ex *connector.Extract, cfg *books.Config, pins map[string]books.OpeningPin) *books.Result {
	t.Helper()
	res, err := books.Build(books.Input{AsOf: ex.AsOf, Accounts: ex.Accounts, Txns: ex.Txns, PortfolioTxns: ex.PortfolioTxns,
		Holdings: ex.Holdings, OpeningPins: pins}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func openingOf(res *books.Result, account string) (amt int64, lines int) {
	for _, e := range res.Entries {
		if e.Type != "opening" {
			continue
		}
		for _, l := range e.Lines {
			if l.Account == account {
				amt += l.Amount
				lines++
			}
		}
	}
	return
}

// A security-less portfolio "cash" row is the brokerage side of a money movement. When the
// same transfer is already booked from the bank side (here by a local rule, like
// a-brokerage-proceeds-rule) it must not be booked again - least of all as (negative)
// dividend income. An unmatched one parks in the transfer clearing account, never in income.
func TestPortfolioCashTransferNotDoubleCounted(t *testing.T) {
	ex, cfg := loadFixture(t, `
rules:
  - {name: ira-contribution, match: '(?i)OUTGOING TRANSFER', direction: out, account: Assets:Investments:IRA, override_transfer: true}
`)
	base := buildEx(t, ex, cfg, nil)
	ex.PortfolioTxns = append(ex.PortfolioTxns,
		connector.PortfolioTxn{ID: "p-ach-in", AccountID: testfixture.IRA, Date: "2026-01-14", Type: "cash", Amount: "300.0000", Currency: "USD"},
		connector.PortfolioTxn{ID: "p-ach-out", AccountID: testfixture.IRA, Date: "2026-01-25", Type: "cash", Amount: "-40.0000", Currency: "USD"})
	res := buildEx(t, ex, cfg, nil)
	if res.Stats.PortfolioTransferMatched != 1 {
		t.Fatalf("matched %d, want 1", res.Stats.PortfolioTransferMatched)
	}
	b0, b1 := balances(base), balances(res)
	if b1[books.DividendsAcct] != b0[books.DividendsAcct] {
		t.Fatalf("dividends moved: %d -> %d", b0[books.DividendsAcct], b1[books.DividendsAcct])
	}
	for _, e := range res.Entries {
		for _, l := range e.Lines {
			if l.RawRef == "p-ach-in" {
				t.Fatalf("matched transfer booked again: %+v", e)
			}
		}
	}
	e := entryFor(t, res, "p-ach-out")
	if e.Lines[1].Account != cfg.Transfers.Unmatched || e.Lines[1].Amount != 4000 {
		t.Fatalf("unmatched cash row: %+v", e.Lines)
	}
}

// Investment-account openings are pinned like transaction-tracked ones: a later pull with new
// portfolio activity (reflected in holdings) and a market move leaves the openings untouched;
// the activity is booked as activity and the valuation-only account's value change goes to
// unrealized gains. History that contradicts a holdings pin lands in Pending, not the opening.
func TestInvestmentOpeningsPinned(t *testing.T) {
	ex, cfg := loadFixture(t, "")
	first := buildEx(t, ex, cfg, nil)
	pins := map[string]books.OpeningPin{}
	for _, p := range first.NewPins {
		pins[p.ExternalID] = p
	}
	bp, ok := pins["opening:"+testfixture.Brokerage]
	if !ok || len(bp.Lines) == 0 {
		t.Fatalf("holdings account not pinned per position: %+v", bp)
	}
	if _, ok := pins["opening:"+testfixture.IRA]; !ok {
		t.Fatal("valuation-only account not pinned")
	}
	brokOpen, brokLines := openingOf(first, "Assets:Investments:Brokerage")
	iraOpen, _ := openingOf(first, "Assets:Investments:IRA")

	// next pull: a 2.00 dividend kept as cash (holdings cash 50 -> 52), IRA value 2000 -> 2100
	for i := range ex.Accounts {
		switch ex.Accounts[i].ID {
		case testfixture.Brokerage:
			v := "1102.00"
			ex.Accounts[i].CurrentBalance = &v
		case testfixture.IRA:
			v := "2100.00"
			ex.Accounts[i].CurrentBalance = &v
		}
	}
	for i := range ex.Holdings {
		if ex.Holdings[i].Ticker == "CUR:USD" {
			ex.Holdings[i].Quantity, ex.Holdings[i].Value = "52.00", "52.00"
		}
	}
	ex.PortfolioTxns = append(ex.PortfolioTxns, connector.PortfolioTxn{ID: "p-div2", AccountID: testfixture.Brokerage, Date: "2026-01-25",
		Type: "cash", Ticker: "FUNDX", Amount: "2.0000", Currency: "USD"})
	for round := 0; round < 2; round++ { // and again: same result
		res := buildEx(t, ex, cfg, pins)
		if len(res.NewPins) != 0 {
			t.Fatalf("pinned accounts re-pinned: %+v", res.NewPins)
		}
		if a, n := openingOf(res, "Assets:Investments:Brokerage"); a != brokOpen || n != brokLines {
			t.Fatalf("brokerage opening moved %d/%d -> %d/%d", brokOpen, brokLines, a, n)
		}
		if a, _ := openingOf(res, "Assets:Investments:IRA"); a != iraOpen {
			t.Fatalf("IRA opening moved %d -> %d", iraOpen, a)
		}
		if len(res.Pending) != 0 {
			t.Fatalf("unexpected pending: %+v", res.Pending)
		}
		b := balances(res)
		if b["Assets:Investments:Brokerage"] != 110200 || b["Assets:Investments:IRA"] != 210000 {
			t.Fatalf("balances brokerage %d IRA %d", b["Assets:Investments:Brokerage"], b["Assets:Investments:IRA"])
		}
		var unreal int64
		for _, e := range res.Entries {
			if e.ExternalID == "valuation:"+testfixture.IRA {
				unreal += e.Lines[0].Amount
			}
		}
		if unreal != 10000 {
			t.Fatalf("IRA valuation change %d, want 10000", unreal)
		}
	}

	// older history that the pin already contains (a buy before the books): pending, not opening
	ex.PortfolioTxns = append(ex.PortfolioTxns, connector.PortfolioTxn{ID: "p-old", AccountID: testfixture.Brokerage, Date: "2026-01-02",
		Type: "buy", Ticker: "FUNDX", Amount: "-50.0000", Quantity: "0.5", Price: "100", Currency: "USD"})
	res := buildEx(t, ex, cfg, pins)
	if a, _ := openingOf(res, "Assets:Investments:Brokerage"); a != brokOpen {
		t.Fatalf("brokerage opening moved %d -> %d", brokOpen, a)
	}
	if len(res.Pending) != 1 || res.Pending[0].Account != "Assets:Investments:Brokerage" {
		t.Fatalf("pending: %+v", res.Pending)
	}
	if b := balances(res)["Assets:Investments:Brokerage"]; b != 110200 {
		t.Fatalf("brokerage %d", b)
	}
}

// The Fidelity feed dropped the money-market (SPAXX) holding rows and began reporting an
// available_balance, while current_balance still includes the sweep cash. A pinned account must
// keep reconciling: no pending entry, opening unmoved, balance equal to the reported balance.
func TestPinnedSweepRowDroppedNoPending(t *testing.T) {
	ex, cfg := loadFixture(t, "")
	set := func(bal string, sweep bool) {
		for i := range ex.Accounts {
			if ex.Accounts[i].ID == testfixture.Brokerage {
				v := bal
				ex.Accounts[i].CurrentBalance = &v
				ex.Accounts[i].AvailableBalance = nil
				if !sweep {
					a := "15.16"
					ex.Accounts[i].AvailableBalance = &a
				}
			}
		}
	}
	set("1115.16", true)
	ex.Holdings = append(ex.Holdings, connector.Holding{ID: "h-sweep", AccountID: testfixture.Brokerage, Ticker: "SPAXX",
		SecurityName: "Gov MM", SecurityType: "cash", Quantity: "15.16", Price: "1", Value: "15.16", Currency: "USD"})
	first := buildEx(t, ex, cfg, nil)
	pins := map[string]books.OpeningPin{}
	for _, p := range first.NewPins {
		pins[p.ExternalID] = p
	}
	want := balances(first)["Assets:Investments:Brokerage"]

	var kept []connector.Holding
	for _, h := range ex.Holdings {
		if h.Ticker != "SPAXX" {
			kept = append(kept, h)
		}
	}
	ex.Holdings = kept
	set("1115.16", false) // sweep row gone, available_balance now reported
	for round := 0; round < 2; round++ {
		res := buildEx(t, ex, cfg, pins)
		if len(res.Pending) != 0 {
			t.Fatalf("unexpected pending: %+v", res.Pending)
		}
		for _, e := range res.Entries {
			if e.Source == books.PendingSource {
				t.Fatalf("pending entry booked: %+v", e)
			}
		}
		if got := balances(res)["Assets:Investments:Brokerage"]; got != want || got != 111516 {
			t.Fatalf("brokerage balance %d, want %d (reported 111516)", got, want)
		}
	}
}
