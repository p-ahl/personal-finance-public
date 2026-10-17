package coinbase

import (
	"strings"
	"testing"
)

func TestDec(t *testing.T) {
	for in, want := range map[string]string{"3e-9": "0.000000003", "0.10": "0.1", "1E+2": "100", "-0.00000000004": "0", "0.123456789012": "0.123456789"} {
		got, err := Dec(in, 10)
		if err != nil || got != want {
			t.Errorf("Dec(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Dec("abc", 10); err == nil {
		t.Error("want error")
	}
}

func TestParsePortfolioKeepsTinyStakedPositions(t *testing.T) {
	m, err := ParsePortfolio([]byte(`{"spot_positions":[{"asset":"SOL","total_balance_crypto":3e-9},{"asset":"USD","is_cash":true}]}`))
	if err != nil || len(m) != 1 || m["SOL"] != "0.000000003" {
		t.Fatalf("%v %v", m, err)
	}
}

func TestParseBalancesSumsHold(t *testing.T) {
	m, err := ParseBalances([]byte(`{"accounts":[{"currency":"btc","available_balance":{"value":"0.5"},"hold":{"value":"0.25"}},{"currency":"USD","available_balance":{"value":"0"},"hold":{"value":"0"}}]}`))
	if err != nil || m["BTC"] != "0.75" || m["USD"] != "0" {
		t.Fatalf("%v %v", m, err)
	}
}

func TestParseFills(t *testing.T) {
	rows, err := ParseFills([]byte(`{"fills":[
		{"entry_id":"e1","order_id":"o1","trade_time":"2026-01-05T18:05:00Z","price":"98000","size":"0.002","commission":"4","product_id":"BTC-USD","side":"BUY"},
		{"entry_id":"e2","order_id":"o2","trade_time":"2026-01-06T18:05:00Z","price":"0.1","size":"10","commission":"0.05","product_id":"DOGE-USD","side":"SELL","size_in_quote":true}]}`))
	if err != nil || len(rows) != 4 {
		t.Fatalf("%v %v", rows, err)
	}
	if rows[0].Asset != "BTC" || rows[0].Amount != "0.002" || rows[1].Asset != "USD" || rows[1].Amount != "-196" || rows[1].Fee != "4" || rows[1].RefID != "fill:o1" {
		t.Fatalf("buy: %+v %+v", rows[0], rows[1])
	}
	if rows[2].Amount != "-100" || rows[3].Amount != "10" || rows[3].Fee != "0.05" {
		t.Fatalf("sell in quote: %+v %+v", rows[2], rows[3])
	}
	if empty, err := ParseFills([]byte(`{}`)); err != nil || len(empty) != 0 {
		t.Fatalf("empty: %v %v", empty, err)
	}
}

func TestParseBestBidAskAndCandles(t *testing.T) {
	m, err := ParseBestBidAsk([]byte(`{"pricebooks":[{"product_id":"SOL-USD","bids":[{"price":"114.90"}],"asks":[{"price":"114.92"}]}]}`))
	if err != nil || m["SOL-USD"] != "114.91" {
		t.Fatalf("%v %v", m, err)
	}
	c, err := ParseCandles([]byte(`{"candles":[{"start":"1765756800","close":"127.8"}]}`))
	if err != nil || len(c) != 1 || c[0].Start != 1765756800 || c[0].Close != "127.8" {
		t.Fatalf("%v %v", c, err)
	}
}

func TestParseEvents(t *testing.T) {
	const hdr = "id,time,refid,type,asset,amount,fee,evidence,memo\n"
	rows, err := ParseEvents(strings.NewReader(hdr +
		"a,2025-12-16T05:27:52Z,t1,trade,usdc,-490.71,9.20,email:x,swap\n" +
		"b,2025-12-16T05:27:52Z,t1,trade,SOL,3.848248574,0,email:x,swap\n"))
	if err != nil || len(rows) != 2 || rows[0].ID != "ev:a" || rows[0].Asset != "USDC" || rows[0].Fee != "9.2" || rows[1].Time != 1765862872 {
		t.Fatalf("%+v %v", rows, err)
	}
	for name, body := range map[string]string{
		"duplicate id": "a,2025-12-16T05:27:52Z,,deposit,USD,1,0,e,\na,2025-12-16T05:27:52Z,,deposit,USD,1,0,e,\n",
		"no evidence":  "a,2025-12-16T05:27:52Z,,deposit,USD,1,0,,\n",
		"unknown type": "a,2025-12-16T05:27:52Z,,transfer,USD,1,0,e,\n",
		"bad time":     "a,12/16/2025,,deposit,USD,1,0,e,\n",
		"bad amount":   "a,2025-12-16T05:27:52Z,,deposit,USD,1.2.3,0,e,\n",
	} {
		if _, err := ParseEvents(strings.NewReader(hdr + body)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := ParseEvents(strings.NewReader("id,time\n")); err == nil {
		t.Error("bad header: want error")
	}
}
