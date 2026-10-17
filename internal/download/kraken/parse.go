package kraken

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// LedgerEntry is one row of the Ledgers result (amounts stay decimal strings, exactly as sent).
type LedgerEntry struct {
	ID      string          `json:"-"`
	RefID   string          `json:"refid"`
	Time    float64         `json:"time"`
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	AClass  string          `json:"aclass"`
	Asset   string          `json:"asset"`
	Amount  string          `json:"amount"`
	Fee     string          `json:"fee"`
	Balance string          `json:"balance"`
	Raw     json.RawMessage `json:"-"`
}

// Trade is one row of the TradesHistory result.
type Trade struct {
	ID        string          `json:"-"`
	OrderTxID string          `json:"ordertxid"`
	Pair      string          `json:"pair"`
	Time      float64         `json:"time"`
	Type      string          `json:"type"` // buy | sell
	OrderType string          `json:"ordertype"`
	Price     string          `json:"price"`
	Cost      string          `json:"cost"`
	Fee       string          `json:"fee"`
	Vol       string          `json:"vol"`
	Ledgers   []string        `json:"ledgers"`
	Raw       json.RawMessage `json:"-"`
}

func result(body []byte) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	if len(env.Error) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(env.Error, "; "))
	}
	return env.Result, nil
}

// ParseLedgerPage parses one saved Ledgers response; rows are sorted by time, then id.
func ParseLedgerPage(body []byte) ([]LedgerEntry, error) {
	res, err := result(body)
	if err != nil {
		return nil, err
	}
	var r struct {
		Ledger map[string]json.RawMessage `json:"ledger"`
		Count  int                        `json:"count"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	out := make([]LedgerEntry, 0, len(r.Ledger))
	for id, raw := range r.Ledger {
		var e LedgerEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("ledger %s: %w", id, err)
		}
		e.ID, e.Raw = id, raw
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time < out[j].Time
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ParseTradesPage parses one saved TradesHistory response.
func ParseTradesPage(body []byte) ([]Trade, error) {
	res, err := result(body)
	if err != nil {
		return nil, err
	}
	var r struct {
		Trades map[string]json.RawMessage `json:"trades"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	out := make([]Trade, 0, len(r.Trades))
	for id, raw := range r.Trades {
		var t Trade
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("trade %s: %w", id, err)
		}
		t.ID, t.Raw = id, raw
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time < out[j].Time
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ParseBalance parses a Balance response: asset -> decimal string.
func ParseBalance(body []byte) (map[string]string, error) {
	res, err := result(body)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	return out, json.Unmarshal(res, &out)
}

// ParseBalanceEx parses a BalanceEx response: asset -> {balance, hold_trade, credit, ...}.
func ParseBalanceEx(body []byte) (map[string]map[string]string, error) {
	res, err := result(body)
	if err != nil {
		return nil, err
	}
	var raw map[string]map[string]json.RawMessage
	if err := json.Unmarshal(res, &raw); err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	for a, m := range raw {
		out[a] = map[string]string{}
		for k, v := range m {
			var s string
			if json.Unmarshal(v, &s) != nil {
				s = string(v)
			}
			out[a][k] = s
		}
	}
	return out, nil
}

// ParseTicker returns pair -> last trade price ("c"[0]).
func ParseTicker(body []byte) (map[string]string, error) {
	res, err := result(body)
	if err != nil {
		return nil, err
	}
	var r map[string]struct {
		C []string `json:"c"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for p, v := range r {
		if len(v.C) > 0 {
			out[p] = v.C[0]
		}
	}
	return out, nil
}

// Candle is one daily OHLC row: UTC day start (unix seconds) and the close price.
type Candle struct {
	Time  int64
	Close string
}

// ParseOHLC returns the pair name used in the response and its candles.
func ParseOHLC(body []byte) (string, []Candle, error) {
	res, err := result(body)
	if err != nil {
		return "", nil, err
	}
	var r map[string]json.RawMessage
	if err := json.Unmarshal(res, &r); err != nil {
		return "", nil, err
	}
	for k, v := range r {
		if k == "last" {
			continue
		}
		var rows [][]json.RawMessage
		if err := json.Unmarshal(v, &rows); err != nil {
			return "", nil, fmt.Errorf("ohlc %s: %w", k, err)
		}
		out := make([]Candle, 0, len(rows))
		for _, row := range rows {
			if len(row) < 5 {
				continue
			}
			var t int64
			var c string
			if err := json.Unmarshal(row[0], &t); err != nil {
				return "", nil, err
			}
			if err := json.Unmarshal(row[4], &c); err != nil {
				return "", nil, err
			}
			out = append(out, Candle{Time: t, Close: c})
		}
		return k, out, nil
	}
	return "", nil, fmt.Errorf("ohlc: no pair in result")
}

// ---------- assets and pairs ----------

type assetInfo struct {
	AltName string `json:"altname"`
}

type pairInfo struct {
	AltName string `json:"altname"`
	WSName  string `json:"wsname"`
	Base    string `json:"base"`
	Quote   string `json:"quote"`
}

// Pairs maps Kraken asset codes to USD trading pairs and canonical commodity codes.
type Pairs struct {
	assets map[string]assetInfo
	pairs  map[string]pairInfo
	byAlt  map[string]string // asset altname -> asset code
}

// ParsePairs builds the lookup from saved AssetPairs (required) and Assets (optional) bodies.
func ParsePairs(assetPairs, assets []byte) (*Pairs, error) {
	p := &Pairs{assets: map[string]assetInfo{}, pairs: map[string]pairInfo{}, byAlt: map[string]string{}}
	res, err := result(assetPairs)
	if err != nil {
		return nil, fmt.Errorf("AssetPairs: %w", err)
	}
	if err := json.Unmarshal(res, &p.pairs); err != nil {
		return nil, fmt.Errorf("AssetPairs: %w", err)
	}
	if len(assets) > 0 {
		if res, err := result(assets); err == nil {
			_ = json.Unmarshal(res, &p.assets)
		}
	}
	for code, a := range p.assets {
		if a.AltName != "" {
			p.byAlt[a.AltName] = code
		}
	}
	return p, nil
}

// SpotAsset strips Kraken's earn/staking/hold suffixes and maps the old staking codes to the
// spot asset code: "DOT.S" -> "DOT", "XBT.M" -> "XXBT", "ETH2.S" -> "XETH", "USD.HOLD" -> "ZUSD".
func (p *Pairs) SpotAsset(code string) string {
	base := code
	if i := strings.IndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	switch base {
	case "ETH2":
		base = "ETH"
	case "USD":
		base = "ZUSD"
	}
	if p != nil {
		if _, ok := p.assets[base]; ok {
			return base
		}
		if c, ok := p.byAlt[base]; ok {
			return c
		}
	}
	if legacy[base] != "" {
		return legacy[base]
	}
	return base
}

var legacy = map[string]string{"XBT": "XXBT", "ETH": "XETH", "XDG": "XXDG", "LTC": "XLTC", "XRP": "XXRP",
	"XLM": "XXLM", "XMR": "XXMR", "ZEC": "XZEC", "ETC": "XETC", "USD": "ZUSD", "EUR": "ZEUR"}

// Commodity is the ledger commodity code for a Kraken asset: BTC, ETH, DOGE, SOL, ... and USD
// for Kraken's USD balances.
func (p *Pairs) Commodity(code string) string {
	spot := p.SpotAsset(code)
	alt := spot
	if p != nil {
		if a, ok := p.assets[spot]; ok && a.AltName != "" {
			alt = a.AltName
		}
	}
	if alt == spot && len(spot) == 4 && (spot[0] == 'X' || spot[0] == 'Z') {
		for k, v := range legacy {
			if v == spot {
				alt = k
			}
		}
	}
	switch alt {
	case "XBT":
		return "BTC"
	case "XDG":
		return "DOGE"
	}
	return alt
}

// IsUSD reports whether a Kraken asset code is US dollars (cash, held, or earning).
func IsUSD(code string) bool {
	return (&Pairs{}).SpotAsset(code) == "ZUSD"
}

// USDPair returns the name of the spot pair <asset>/USD (e.g. XXBTZUSD, DOTUSD), or "".
func (p *Pairs) USDPair(code string) string {
	spot := p.SpotAsset(code)
	if spot == "ZUSD" {
		return ""
	}
	var names []string
	for n := range p.pairs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		pi := p.pairs[n]
		if pi.Base == spot && (pi.Quote == "ZUSD" || pi.Quote == "USD") && !strings.HasSuffix(n, ".d") {
			return n
		}
	}
	return ""
}

// PairAssets returns the base and quote asset codes of a pair name (or altname).
func (p *Pairs) PairAssets(pair string) (string, string, bool) {
	if pi, ok := p.pairs[pair]; ok {
		return pi.Base, pi.Quote, true
	}
	for _, pi := range p.pairs {
		if pi.AltName == pair {
			return pi.Base, pi.Quote, true
		}
	}
	return "", "", false
}
