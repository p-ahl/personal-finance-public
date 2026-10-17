// Package coinbase parses a saved Coinbase pull (raw/coinbase/<YYYY-MM-DD>/).
//
// Coinbase is read through the Coinbase MCP connector (read-only tools), not from Go: the
// agent saves each response body byte-for-byte, then `finance import coinbase` stages the
// folder. The connector has no deposit/withdrawal/send history, so consumer activity
// (simple buys, sends, rewards) comes from events.csv, one row per ledger movement, each
// with its evidence (Coinbase email message id, bank row, or "inferred").
//
// Files (all optional except MANIFEST.md and at least one balance source):
//
//	balance_<portfolio>.json    coinbase_balance: {"accounts":[{currency, available_balance, hold}]}
//	portfolio_<portfolio>.json  coinbase_portfolios_get: spot_positions (incl. staked balances)
//	fills_*.json                coinbase_orders_fills: {"fills":[...]} (Advanced Trade fills)
//	best_bid_ask.json           coinbase_products_best_bid_ask: ticker = (bid+ask)/2
//	candles_<BASE>-USD.json     coinbase_products_candles granularity 1d: daily closes (UTC day)
//	events.csv                  id,time,refid,type,asset,amount,fee,evidence,memo
package coinbase

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Row is one ledger movement in the shape of the exchange staging table.
type Row struct {
	ID, RefID   string
	Time        float64 // unix seconds
	Type        string  // deposit | withdrawal | trade | staking
	Asset       string  // commodity code (USD, BTC, SOL, USDC, ...)
	Amount, Fee string  // decimal strings; amount signed, fee positive (paid on top)
	Raw         string  // the source record as JSON
}

// Dec normalizes a JSON number or decimal string ("3e-9", "0.10", "1E+2") to plain decimal
// text rounded half away from zero to at most scale fractional digits, trailing zeros trimmed.
func Dec(s string, scale int) (string, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return "", fmt.Errorf("coinbase: invalid number %q", s)
	}
	return ratString(r, scale), nil
}

func ratString(r *big.Rat, scale int) string {
	out := r.FloatString(scale) // rounds half away from zero
	if strings.Contains(out, ".") {
		out = strings.TrimRight(strings.TrimRight(out, "0"), ".")
	}
	if out == "-0" {
		out = "0"
	}
	return out
}

type amount struct {
	Currency string      `json:"currency"`
	Value    json.Number `json:"value"`
}

// ParseBalances sums available + hold per currency from a coinbase_balance response.
func ParseBalances(body []byte) (map[string]string, error) {
	var r struct {
		Accounts []struct {
			Currency  string `json:"currency"`
			Available amount `json:"available_balance"`
			Hold      amount `json:"hold"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	sum := map[string]*big.Rat{}
	for _, a := range r.Accounts {
		c := strings.ToUpper(a.Currency)
		if sum[c] == nil {
			sum[c] = new(big.Rat)
		}
		for _, v := range []json.Number{a.Available.Value, a.Hold.Value} {
			if v == "" {
				continue
			}
			x, ok := new(big.Rat).SetString(string(v))
			if !ok {
				return nil, fmt.Errorf("balance %s: invalid value %q", c, v)
			}
			sum[c].Add(sum[c], x)
		}
	}
	out := map[string]string{}
	for c, v := range sum {
		out[c] = ratString(v, 10)
	}
	return out, nil
}

// ParsePortfolio returns the crypto quantity of each non-cash spot position (these include
// staked balances that coinbase_balance leaves out).
func ParsePortfolio(body []byte) (map[string]string, error) {
	var r struct {
		Spot []struct {
			Asset  string      `json:"asset"`
			Qty    json.Number `json:"total_balance_crypto"`
			IsCash bool        `json:"is_cash"`
		} `json:"spot_positions"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&r); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range r.Spot {
		if p.IsCash || p.Qty == "" {
			continue
		}
		v, err := Dec(string(p.Qty), 10)
		if err != nil {
			return nil, fmt.Errorf("position %s: %w", p.Asset, err)
		}
		out[strings.ToUpper(p.Asset)] = v
	}
	return out, nil
}

// ParseFills turns coinbase_orders_fills into ledger rows: per fill one base and one quote
// row under refid "fill:<order_id>", commission charged in the quote currency.
func ParseFills(body []byte) ([]Row, error) {
	var r struct {
		Fills []json.RawMessage `json:"fills"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	var out []Row
	for _, raw := range r.Fills {
		var f struct {
			EntryID     string `json:"entry_id"`
			TradeID     string `json:"trade_id"`
			OrderID     string `json:"order_id"`
			TradeTime   string `json:"trade_time"`
			Price       string `json:"price"`
			Size        string `json:"size"`
			Commission  string `json:"commission"`
			ProductID   string `json:"product_id"`
			Side        string `json:"side"`
			SizeInQuote bool   `json:"size_in_quote"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
		base, quote, ok := strings.Cut(strings.ToUpper(f.ProductID), "-")
		if !ok {
			return nil, fmt.Errorf("fill %s: product %q", f.EntryID, f.ProductID)
		}
		t, err := time.Parse(time.RFC3339Nano, f.TradeTime)
		if err != nil {
			return nil, fmt.Errorf("fill %s: trade_time: %w", f.EntryID, err)
		}
		price, ok1 := new(big.Rat).SetString(f.Price)
		size, ok2 := new(big.Rat).SetString(f.Size)
		comm, ok3 := new(big.Rat).SetString(orZero(f.Commission))
		if !ok1 || !ok2 || !ok3 || price.Sign() <= 0 {
			return nil, fmt.Errorf("fill %s: invalid price/size/commission", f.EntryID)
		}
		baseQty, quoteAmt := new(big.Rat).Set(size), new(big.Rat).Mul(price, size)
		if f.SizeInQuote {
			quoteAmt.Set(size)
			baseQty.Quo(size, price)
		}
		qs := 10
		if quote == "USD" {
			qs = 4
		}
		id := f.EntryID
		if id == "" {
			id = f.TradeID
		}
		ref := "fill:" + f.OrderID
		ts := float64(t.UnixNano()) / 1e9
		var b, q Row
		switch strings.ToUpper(f.Side) {
		case "BUY":
			b = Row{Amount: ratString(baseQty, 10), Fee: "0"}
			q = Row{Amount: "-" + ratString(quoteAmt, qs), Fee: ratString(comm, qs)}
		case "SELL":
			b = Row{Amount: "-" + ratString(baseQty, 10), Fee: "0"}
			q = Row{Amount: ratString(quoteAmt, qs), Fee: ratString(comm, qs)}
		default:
			return nil, fmt.Errorf("fill %s: side %q", f.EntryID, f.Side)
		}
		b.ID, b.RefID, b.Time, b.Type, b.Asset, b.Raw = "fill:"+id+":"+base, ref, ts, "trade", base, string(raw)
		q.ID, q.RefID, q.Time, q.Type, q.Asset, q.Raw = "fill:"+id+":"+quote, ref, ts, "trade", quote, string(raw)
		out = append(out, b, q)
	}
	return out, nil
}

func orZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

// Candle is one daily close.
type Candle struct {
	Start int64 // unix seconds (UTC midnight)
	Close string
}

// ParseCandles reads a coinbase_products_candles response.
func ParseCandles(body []byte) ([]Candle, error) {
	var r struct {
		Candles []struct {
			Start string `json:"start"`
			Close string `json:"close"`
		} `json:"candles"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := make([]Candle, 0, len(r.Candles))
	for _, c := range r.Candles {
		s, err := strconv.ParseInt(c.Start, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("candle start %q", c.Start)
		}
		v, err := Dec(c.Close, 10)
		if err != nil {
			return nil, err
		}
		out = append(out, Candle{Start: s, Close: v})
	}
	return out, nil
}

// ParseBestBidAsk returns product id -> mid price (bid+ask)/2 for each pricebook.
func ParseBestBidAsk(body []byte) (map[string]string, error) {
	var r struct {
		Pricebooks []struct {
			ProductID string `json:"product_id"`
			Bids      []struct {
				Price string `json:"price"`
			} `json:"bids"`
			Asks []struct {
				Price string `json:"price"`
			} `json:"asks"`
		} `json:"pricebooks"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range r.Pricebooks {
		if len(p.Bids) == 0 || len(p.Asks) == 0 {
			continue
		}
		b, ok1 := new(big.Rat).SetString(p.Bids[0].Price)
		a, ok2 := new(big.Rat).SetString(p.Asks[0].Price)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("pricebook %s: invalid price", p.ProductID)
		}
		mid := new(big.Rat).Add(a, b)
		mid.Quo(mid, big.NewRat(2, 1))
		out[strings.ToUpper(p.ProductID)] = ratString(mid, 10)
	}
	return out, nil
}

var eventCols = []string{"id", "time", "refid", "type", "asset", "amount", "fee", "evidence", "memo"}

var eventTypes = map[string]bool{"deposit": true, "withdrawal": true, "trade": true, "staking": true}

// ParseEvents reads events.csv (header: id,time,refid,type,asset,amount,fee,evidence,memo;
// time RFC 3339). Ids must be unique; every row needs evidence ("inferred" is allowed).
func ParseEvents(r io.Reader) ([]Row, error) {
	cr := csv.NewReader(r)
	hdr, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("events.csv: %w", err)
	}
	if strings.Join(hdr, ",") != strings.Join(eventCols, ",") {
		return nil, fmt.Errorf("events.csv: header must be %s", strings.Join(eventCols, ","))
	}
	seen := map[string]bool{}
	var out []Row
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("events.csv line %d: %w", line, err)
		}
		m := map[string]string{}
		for i, c := range eventCols {
			m[c] = strings.TrimSpace(rec[i])
		}
		if m["id"] == "" || seen[m["id"]] {
			return nil, fmt.Errorf("events.csv line %d: missing or duplicate id %q", line, m["id"])
		}
		seen[m["id"]] = true
		if !eventTypes[m["type"]] {
			return nil, fmt.Errorf("events.csv line %d: type %q (deposit|withdrawal|trade|staking)", line, m["type"])
		}
		if m["evidence"] == "" {
			return nil, fmt.Errorf("events.csv line %d: evidence is required", line)
		}
		t, err := time.Parse(time.RFC3339Nano, m["time"])
		if err != nil {
			return nil, fmt.Errorf("events.csv line %d: time: %w", line, err)
		}
		asset := strings.ToUpper(m["asset"])
		scale := 10
		if asset == "USD" {
			scale = 4
		}
		amt, err := Dec(m["amount"], scale)
		if err != nil {
			return nil, fmt.Errorf("events.csv line %d: %w", line, err)
		}
		fee, err := Dec(orZero(m["fee"]), scale)
		if err != nil {
			return nil, fmt.Errorf("events.csv line %d: %w", line, err)
		}
		ref := m["refid"]
		if ref == "" {
			ref = m["id"]
		}
		raw, _ := json.Marshal(m)
		out = append(out, Row{ID: "ev:" + m["id"], RefID: ref, Time: float64(t.UnixNano()) / 1e9, Type: m["type"],
			Asset: asset, Amount: amt, Fee: fee, Raw: string(raw)})
	}
	return out, nil
}
