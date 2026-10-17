package kraken

import (
	"testing"

	"github.com/p-ahl/personal-finance/internal/testfixture"
)

func fixturePairs(t *testing.T) *Pairs {
	t.Helper()
	p, err := ParsePairs([]byte(testfixture.KrakenAssetPairsJSON), []byte(testfixture.KrakenAssetsJSON))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseLedgerPage(t *testing.T) {
	rows, err := ParseLedgerPage([]byte(testfixture.KrakenLedgerJSON()))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 13 {
		t.Fatalf("rows = %d", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Time < rows[i-1].Time {
			t.Fatalf("rows not in time order at %d", i)
		}
	}
	first := rows[0]
	if first.ID != "LFAKE1-AAAAA-000001" || first.Type != "deposit" || first.Asset != "USD.HOLD" || first.Amount != "100.0000" || first.RefID != "FTFAKE-0001" {
		t.Fatalf("first row: %+v", first)
	}
	if len(first.Raw) == 0 {
		t.Fatal("raw json not kept")
	}
	var trade LedgerEntry
	for _, r := range rows {
		if r.ID == "LFAKE1-AAAAA-000006" {
			trade = r
		}
	}
	if trade.Subtype != "tradespot" || trade.Fee != "0.6000" || trade.Balance != "49.4000" {
		t.Fatalf("trade row: %+v", trade)
	}
}

func TestParseErrorsAndEmptyPages(t *testing.T) {
	if _, err := ParseLedgerPage([]byte(`{"error":["EGeneral:Permission denied"]}`)); err == nil {
		t.Fatal("want error for API error body")
	}
	rows, err := ParseLedgerPage([]byte(`{"error":[],"result":{"ledger":{},"count":0}}`))
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty page: %v %v", rows, err)
	}
	if _, err := ParseLedgerPage([]byte(`not json`)); err == nil {
		t.Fatal("want error for invalid json")
	}
}

func TestParseTradesPage(t *testing.T) {
	tr, err := ParseTradesPage([]byte(testfixture.KrakenTradesJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr) != 2 {
		t.Fatalf("trades = %d", len(tr))
	}
	byID := map[string]Trade{}
	for _, x := range tr {
		byID[x.ID] = x
	}
	b := byID["TFAKE1-BBBBB-000001"]
	if b.Pair != "XXBTZUSD" || b.Type != "buy" || b.Cost != "150.0000" || b.Vol != "0.0030000000" || len(b.Ledgers) != 2 {
		t.Fatalf("buy: %+v", b)
	}
}

func TestParseBalanceTickerOHLC(t *testing.T) {
	bal, err := ParseBalance([]byte(testfixture.KrakenBalanceJSON))
	if err != nil || bal["XXBT"] != "0.0019900000" || bal["ZUSD"] != "29.2800" || len(bal) != 4 {
		t.Fatalf("balance %v %v", bal, err)
	}
	ex, err := ParseBalanceEx([]byte(`{"error":[],"result":{"ZUSD":{"balance":"29.2800","hold_trade":"0.0000"},"KAS":{"balance":"600.00000","hold_trade":"10.00000"}}}`))
	if err != nil || ex["KAS"]["hold_trade"] != "10.00000" || ex["ZUSD"]["balance"] != "29.2800" {
		t.Fatalf("balanceex %v %v", ex, err)
	}
	tk, err := ParseTicker([]byte(testfixture.KrakenTickerJSON))
	if err != nil || tk["XXBTZUSD"] != "100000.0" || tk["KASUSD"] != "0.06" {
		t.Fatalf("ticker %v %v", tk, err)
	}
	pair, candles, err := ParseOHLC([]byte(testfixture.KrakenOHLCJSON("KASUSD")))
	if err != nil || pair != "KASUSD" || len(candles) != 2 || candles[0].Close != "0.05" || candles[0].Time != 1768003200 {
		t.Fatalf("ohlc %s %v %v", pair, candles, err)
	}
}

func TestAssetMapping(t *testing.T) {
	p := fixturePairs(t)
	cases := []struct{ code, spot, commodity, usdPair string }{
		{"XXBT", "XXBT", "BTC", "XXBTZUSD"},
		{"XBT.M", "XXBT", "BTC", "XXBTZUSD"},
		{"KAS", "KAS", "KAS", "KASUSD"},
		{"SOL.F", "SOL", "SOL", "SOLUSD"},
		{"ZUSD", "ZUSD", "USD", ""},
		{"USD.HOLD", "ZUSD", "USD", ""},
		{"XXMR", "XXMR", "XMR", ""}, // not in the fixture's Assets: legacy table
		{"XXDG", "XXDG", "DOGE", ""},
	}
	for _, c := range cases {
		if got := p.SpotAsset(c.code); got != c.spot {
			t.Errorf("SpotAsset(%s) = %s want %s", c.code, got, c.spot)
		}
		if got := p.Commodity(c.code); got != c.commodity {
			t.Errorf("Commodity(%s) = %s want %s", c.code, got, c.commodity)
		}
		if got := p.USDPair(c.code); got != c.usdPair {
			t.Errorf("USDPair(%s) = %q want %q", c.code, got, c.usdPair)
		}
	}
	for _, c := range []string{"ZUSD", "USD.HOLD", "USD.M"} {
		if !IsUSD(c) {
			t.Errorf("IsUSD(%s) = false", c)
		}
	}
	if IsUSD("USDC") || IsUSD("XXBT") {
		t.Error("IsUSD true for a non-USD asset")
	}
	if b, q, ok := p.PairAssets("XBTUSD"); !ok || b != "XXBT" || q != "ZUSD" {
		t.Errorf("PairAssets(XBTUSD) = %s %s %v", b, q, ok)
	}
	if b, q, ok := p.PairAssets("XBTUSDC"); !ok || b != "XXBT" || q != "USDC" {
		t.Errorf("PairAssets(XBTUSDC) = %s %s %v", b, q, ok)
	}
}
