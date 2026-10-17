package testfixture

import (
	"path/filepath"
	"testing"
)

// Synthetic Coinbase scenario (all ids, times and amounts are made up). Books start 2025-12-31.
//
//	2025-12-01  USDC reward 5 (before the books -> opening 5 USDC at par)
//	2026-01-05  USD deposit 200 (bank row "COINBASE" -200 on 2026-01-07 -> in transit)
//	2026-01-05  fill: buy 0.002 BTC @ 98,000 = 196 + 4 commission (connector fills file)
//	2026-01-10  5 USDC -> 0.05 SOL (events.csv)
//	2026-01-12  send 0.05 SOL to own wallet (self-custody at cost 5.00)
//	2026-01-15  sell 0.001 BTC for 60 (cost 98 -> 38 short-term loss)
//	2026-01-26  USD withdrawal 60 (bank row "COINBASE" +60 the same day)
//
// Final balances: USD 0, BTC 0.001 (staked 0 SOL dust 0); best bid/ask BTC 99,990/100,010.
const CoinbaseAsOf = "2026-02-01"

const coinbaseEvents = `id,time,refid,type,asset,amount,fee,evidence,memo
p1,2025-12-01T18:00:00Z,p1,staking,USDC,5,0,inferred,reward before the books
d1,2026-01-05T18:00:00Z,d1,deposit,USD,200.00,0,email:fake-1,ACH deposit
s1-usdc,2026-01-10T18:00:00Z,s1,trade,USDC,-5,0,email:fake-2,USDC to SOL
s1-sol,2026-01-10T18:00:00Z,s1,trade,SOL,0.05,0,email:fake-2,USDC to SOL
w1,2026-01-12T18:00:00Z,w1,withdrawal,SOL,-0.05,0,email:fake-3,send to own wallet
x1-btc,2026-01-15T18:00:00Z,x1,trade,BTC,-0.001,0,inferred,sell BTC
x1-usd,2026-01-15T18:00:00Z,x1,trade,USD,60,0,inferred,sell BTC
w2,2026-01-26T18:00:00Z,w2,withdrawal,USD,-60.00,0,bank:fake,withdrawal to bank
`

const coinbaseFills = `{"fills":[{"entry_id":"fake-entry-1","trade_id":"fake-trade-1","order_id":"fake-order-1","trade_time":"2026-01-05T18:05:00.123Z","trade_type":"FILL","price":"98000","size":"0.002","commission":"4","product_id":"BTC-USD","side":"BUY","size_in_quote":false}],"cursor":""}`

const coinbaseBalance = `{"accounts":[{"currency":"BTC","available_balance":{"currency":"BTC","value":"0.001"},"hold":{"currency":"BTC","value":"0"}},{"currency":"USD","available_balance":{"currency":"USD","value":"0"},"hold":{"currency":"USD","value":"0"}},{"currency":"USDC","available_balance":{"currency":"USDC","value":"0"},"hold":{"currency":"USDC","value":"0"}},{"currency":"SOL","available_balance":{"currency":"SOL","value":"0"},"hold":{"currency":"SOL","value":"0"}}]}`

const coinbasePortfolio = `{"portfolio":{"name":"Default","uuid":"fake-portfolio"},"spot_positions":[{"asset":"BTC","total_balance_crypto":1e-3},{"asset":"USD","is_cash":true,"total_balance_crypto":0}]}`

const coinbaseBBA = `{"pricebooks":[{"product_id":"BTC-USD","bids":[{"price":"99990","size":"1"}],"asks":[{"price":"100010","size":"1"}]},{"product_id":"SOL-USD","bids":[{"price":"109.99","size":"1"}],"asks":[{"price":"110.01","size":"1"}]}]}`

const coinbaseSOLCandles = `{"candles":[{"start":"1768003200","close":"100","open":"100","high":"100","low":"100","volume":"1"}]}`

// CoinbasePull writes the synthetic pull to root/coinbase/<CoinbaseAsOf>/ and returns it.
func CoinbasePull(t testing.TB, root string) string {
	dir := filepath.Join(root, "coinbase", CoinbaseAsOf)
	write(t, filepath.Join(dir, "events.csv"), coinbaseEvents)
	write(t, filepath.Join(dir, "fills_all.json"), coinbaseFills)
	write(t, filepath.Join(dir, "balance_fake.json"), coinbaseBalance)
	write(t, filepath.Join(dir, "portfolio_fake.json"), coinbasePortfolio)
	write(t, filepath.Join(dir, "best_bid_ask.json"), coinbaseBBA)
	write(t, filepath.Join(dir, "candles_SOL-USD.json"), coinbaseSOLCandles)
	write(t, filepath.Join(dir, "MANIFEST.md"), "# synthetic Coinbase pull (test fixture)\n")
	return dir
}
