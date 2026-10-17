package testfixture

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Synthetic Kraken scenario (all ids, times and amounts are made up). Books start 2025-12-31.
//
//	2025-12-01  USD.HOLD deposit 100, buy 1000 KAS for 99 + 1 fee      (before the books)
//	2025-12-02  withdraw 500 KAS (+1 KAS fee) to self-custody           (before the books)
//	2026-01-05  ZUSD deposit 200 (bank row "KRAKEN" -200 on 2026-01-07 -> in transit)
//	2026-01-05  buy 0.003 BTC for 150 + 0.60 fee
//	2026-01-10  staking reward 10 KAS (close 0.05 -> 0.50 income)
//	2026-01-15  sell 509 KAS for 30 - 0.12 fee
//	2026-01-20  withdraw 0.001 BTC (+0.00001 fee) to self-custody
//	2026-01-25  deposit 600 KAS from self-custody (500 from the pre-books lot, 100 unknown basis)
//	2026-01-26  ZUSD withdrawal 50 (bank row "KRAKEN" +50 the same day)
//
// Final balances: ZUSD 29.28, BTC 0.00199, KAS 600; ticker BTC 100000, KAS 0.06.
const KrakenAsOf = "2026-02-01"

type krRow struct {
	id, ref, typ, sub, asset, amount, fee, balance string
	t                                              time.Time
}

func at(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 18, 0, 0, 0, time.UTC) }

var krRows = []krRow{
	{"LFAKE1-AAAAA-000001", "FTFAKE-0001", "deposit", "", "USD.HOLD", "100.0000", "0.0000", "100.0000", at(2025, 12, 1)},
	{"LFAKE1-AAAAA-000002", "TSFAKE-0001", "spend", "", "USD.HOLD", "-99.0000", "1.0000", "0.0000", at(2025, 12, 1).Add(time.Second)},
	{"LFAKE1-AAAAA-000003", "TSFAKE-0001", "receive", "", "KAS", "1000.00000", "0.00000", "1000.00000", at(2025, 12, 1).Add(time.Second)},
	{"LFAKE1-AAAAA-000004", "FTFAKE-0002", "withdrawal", "", "KAS", "-500.00000", "1.00000", "499.00000", at(2025, 12, 2)},
	{"LFAKE1-AAAAA-000005", "FTFAKE-0003", "deposit", "", "ZUSD", "200.0000", "0.0000", "200.0000", at(2026, 1, 5)},
	{"LFAKE1-AAAAA-000006", "TFAKE1-BBBBB-000001", "trade", "tradespot", "ZUSD", "-150.0000", "0.6000", "49.4000", at(2026, 1, 5).Add(time.Minute)},
	{"LFAKE1-AAAAA-000007", "TFAKE1-BBBBB-000001", "trade", "tradespot", "XXBT", "0.0030000000", "0.0000000000", "0.0030000000", at(2026, 1, 5).Add(time.Minute)},
	{"LFAKE1-AAAAA-000008", "ELFAKE-0001", "staking", "", "KAS", "10.00000", "0.00000", "509.00000", at(2026, 1, 10)},
	{"LFAKE1-AAAAA-000009", "TFAKE1-BBBBB-000002", "trade", "tradespot", "KAS", "-509.00000", "0.00000", "0.00000", at(2026, 1, 15)},
	{"LFAKE1-AAAAA-000010", "TFAKE1-BBBBB-000002", "trade", "tradespot", "ZUSD", "30.0000", "0.1200", "79.2800", at(2026, 1, 15)},
	{"LFAKE1-AAAAA-000011", "FTFAKE-0004", "withdrawal", "", "XXBT", "-0.0010000000", "0.0000100000", "0.0019900000", at(2026, 1, 20)},
	{"LFAKE1-AAAAA-000012", "FTFAKE-0005", "deposit", "", "KAS", "600.00000", "0.00000", "600.00000", at(2026, 1, 25)},
	{"LFAKE1-AAAAA-000013", "FTFAKE-0006", "withdrawal", "", "ZUSD", "-50.0000", "0.0000", "29.2800", at(2026, 1, 26)},
}

func ts(t time.Time) string { return fmt.Sprintf("%d.1234", t.Unix()) }

// KrakenLedgerJSON is the Ledgers response body for the synthetic scenario (one page).
func KrakenLedgerJSON() string {
	var parts []string
	for i := len(krRows) - 1; i >= 0; i-- { // Kraken returns newest first
		r := krRows[i]
		parts = append(parts, fmt.Sprintf(`"%s":{"aclass":"currency","amount":"%s","asset":"%s","balance":"%s","fee":"%s","refid":"%s","time":%s,"type":"%s","subtype":"%s"}`,
			r.id, r.amount, r.asset, r.balance, r.fee, r.ref, ts(r.t), r.typ, r.sub))
	}
	return `{"error":[],"result":{"ledger":{` + strings.Join(parts, ",") + fmt.Sprintf(`},"count":%d}}`, len(krRows))
}

const (
	KrakenAssetsJSON     = `{"error":[],"result":{"ZUSD":{"aclass":"currency","altname":"USD","decimals":4,"display_decimals":2},"USD.HOLD":{"aclass":"currency","altname":"USD.HOLD","decimals":4,"display_decimals":2},"XXBT":{"aclass":"currency","altname":"XBT","decimals":10,"display_decimals":5},"KAS":{"aclass":"currency","altname":"KAS","decimals":5,"display_decimals":5},"SOL":{"aclass":"currency","altname":"SOL","decimals":10,"display_decimals":5},"SOL.F":{"aclass":"currency","altname":"SOL.F","decimals":10,"display_decimals":5}}}`
	KrakenAssetPairsJSON = `{"error":[],"result":{"XXBTZUSD":{"altname":"XBTUSD","wsname":"XBT/USD","base":"XXBT","quote":"ZUSD"},"XBTUSDC":{"altname":"XBTUSDC","wsname":"XBT/USDC","base":"XXBT","quote":"USDC"},"KASUSD":{"altname":"KASUSD","wsname":"KAS/USD","base":"KAS","quote":"ZUSD"},"SOLUSD":{"altname":"SOLUSD","wsname":"SOL/USD","base":"SOL","quote":"ZUSD"}}}`
	KrakenBalanceJSON    = `{"error":[],"result":{"KAS":"600.00000","USD.HOLD":"0.0000","XXBT":"0.0019900000","ZUSD":"29.2800"}}`
	KrakenTickerJSON     = `{"error":[],"result":{"KASUSD":{"a":["0.0601","1","1.000"],"b":["0.0599","1","1.000"],"c":["0.06","100"]},"XXBTZUSD":{"a":["100001.0","1","1.000"],"b":["99999.0","1","1.000"],"c":["100000.0","0.001"]}}}`
	KrakenTradesJSON     = `{"error":[],"result":{"trades":{"TFAKE1-BBBBB-000002":{"ordertxid":"OFAKE-2","pair":"KASUSD","time":1768500000.1,"type":"sell","ordertype":"market","price":"0.05894","cost":"30.0000","fee":"0.1200","vol":"509.00000","ledgers":["LFAKE1-AAAAA-000009","LFAKE1-AAAAA-000010"]},"TFAKE1-BBBBB-000001":{"ordertxid":"OFAKE-1","pair":"XXBTZUSD","time":1767636060.1,"type":"buy","ordertype":"market","price":"50000.0","cost":"150.0000","fee":"0.6000","vol":"0.0030000000","ledgers":["LFAKE1-AAAAA-000006","LFAKE1-AAAAA-000007"]}},"count":2}}`
)

func day(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() }

// KrakenOHLCJSON returns a daily OHLC body for KASUSD or XXBTZUSD.
func KrakenOHLCJSON(pair string) string {
	switch pair {
	case "KASUSD":
		return fmt.Sprintf(`{"error":[],"result":{"KASUSD":[[%d,"0.05","0.05","0.05","0.05","0.05","1000",10],[%d,"0.06","0.06","0.06","0.06","0.06","1000",10]],"last":%d}}`,
			day(2026, 1, 10), day(2026, 1, 25), day(2026, 1, 25))
	default:
		return fmt.Sprintf(`{"error":[],"result":{"XXBTZUSD":[[%d,"50000.0","50000.0","50000.0","50000.0","50000.0","1",10]],"last":%d}}`,
			day(2026, 1, 5), day(2026, 1, 5))
	}
}

// KrakenPull writes the synthetic pull folder (as `finance download kraken` would) under
// root/kraken/<KrakenAsOf> and returns it.
func KrakenPull(t testing.TB, root string) string {
	dir := filepath.Join(root, "kraken", KrakenAsOf)
	write(t, filepath.Join(dir, "Assets.json"), KrakenAssetsJSON)
	write(t, filepath.Join(dir, "AssetPairs.json"), KrakenAssetPairsJSON)
	write(t, filepath.Join(dir, "Balance.json"), KrakenBalanceJSON)
	write(t, filepath.Join(dir, "Ledgers_page001.json"), KrakenLedgerJSON())
	write(t, filepath.Join(dir, "TradesHistory_page001.json"), KrakenTradesJSON)
	write(t, filepath.Join(dir, "Ticker.json"), KrakenTickerJSON)
	write(t, filepath.Join(dir, "OHLC_KASUSD.json"), KrakenOHLCJSON("KASUSD"))
	write(t, filepath.Join(dir, "OHLC_XXBTZUSD.json"), KrakenOHLCJSON("XXBTZUSD"))
	write(t, filepath.Join(dir, "MANIFEST.md"), "# synthetic Kraken pull (test fixture)\n")
	return dir
}
