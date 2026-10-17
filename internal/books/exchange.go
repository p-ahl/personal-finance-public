package books

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/p-ahl/personal-finance/internal/money"
)

// Exchange (Kraken) books. The exchange account is rebuilt from the exchange's own ledger:
//
//   - history up to books_start rolls into one opening entry (positions at FIFO cost, USD cash);
//   - USD deposits/withdrawals are matched to the bank rows that already move money to/from the
//     exchange account (transfer hint, one-sided). Same day: the bank entry stands. Different
//     days: both legs go through Assets:Transfers:InTransit with a shared link id. Unmatched:
//     Assets:Transfers:Unmatched (listed by `finance check`);
//   - crypto deposits/withdrawals move FIFO lots to/from the self-custody account at cost;
//     crypto coming back first uses lots booked there, then lots withdrawn before the books
//     started (evidenced exchange cost, against Equity:Opening-Balances), then basis 0;
//   - trades and spend/receive pairs relieve FIFO lots; realized gains short/long term; fees
//     (USD or crypto at cost) go to Expenses:Fees:Investment; crypto-to-crypto trades are
//     valued at the daily USD close;
//   - staking rewards are income at the daily USD close;
//   - a mark-to-market entry values every position at the pull's ticker price, and a balance
//     assertion (source = the exchange name) ties the account to the exchange-reported balance.
const (
	CryptoScale     = 10 // minor-unit digits for exchange crypto quantities
	exUSDScale      = 4  // Kraken USD precision
	StakingIncome   = "Income:Crypto:Staking"
	SelfCustodyAcct = "Assets:Crypto:SelfCustody"
	RealizedST      = "Income:Investments:Realized-Gains:ShortTerm"
	RealizedLT      = "Income:Investments:Realized-Gains:LongTerm"
)

// ExchangeLedgerRow is one staged exchange ledger entry (decimal strings exactly as received).
type ExchangeLedgerRow struct {
	ID, RefID, Time, Date string
	Type, Subtype         string
	Asset, Commodity      string
	Amount, Fee, Balance  string
}

// ExchangeBalance is one asset balance from the newest pull.
type ExchangeBalance struct{ Asset, Commodity, Balance string }

// ExchangeData is everything staged for one exchange.
type ExchangeData struct {
	Name     string // kraken (also the CoA key of the exchange account)
	AsOf     string // date of the newest balance pull
	Ledger   []ExchangeLedgerRow
	Balances []ExchangeBalance
	Closes   map[string]map[string]string // commodity -> UTC date -> daily close (USD)
	Ticker   map[string]string            // commodity -> last price at the pull
}

type lot struct {
	qty, cost int64
	acquired  string
	inBooks   bool
}

type piece struct {
	qty, cost int64
	acquired  string
	inBooks   bool
	unknown   bool // no lot: basis 0
}

type pool map[string][]*lot // commodity -> FIFO lots

func (p pool) add(c string, l *lot) {
	if l.qty == 0 && l.cost == 0 {
		return
	}
	p[c] = append(p[c], l)
}

func (p pool) qty(c string) int64 {
	var q int64
	for _, l := range p[c] {
		q += l.qty
	}
	return q
}

// take relieves qty FIFO (booked lots first when booksFirst), returning the pieces; any
// shortfall is one unknown-basis piece.
func (p pool) take(c string, qty int64, booksFirst bool) []piece {
	if qty <= 0 {
		return nil
	}
	var out []piece
	order := make([]*lot, 0, len(p[c]))
	if booksFirst {
		for _, l := range p[c] {
			if l.inBooks {
				order = append(order, l)
			}
		}
		for _, l := range p[c] {
			if !l.inBooks {
				order = append(order, l)
			}
		}
	} else {
		order = append(order, p[c]...)
	}
	for _, l := range order {
		if qty == 0 {
			break
		}
		if l.qty <= 0 {
			continue
		}
		q := l.qty
		if q > qty {
			q = qty
		}
		cost := l.cost
		if q != l.qty {
			cost = mulDiv(l.cost, q, l.qty)
		}
		l.qty -= q
		l.cost -= cost
		qty -= q
		out = append(out, piece{qty: q, cost: cost, acquired: l.acquired, inBooks: l.inBooks})
	}
	kept := p[c][:0]
	for _, l := range p[c] {
		if l.qty > 0 {
			kept = append(kept, l)
		}
	}
	p[c] = kept
	if qty > 0 {
		out = append(out, piece{qty: qty, unknown: true})
	}
	return out
}

// mulDiv returns round(a*b/c) half away from zero without overflow.
func mulDiv(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	r := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(a), big.NewInt(b)), big.NewInt(c))
	return ratRound(r)
}

func ratRound(r *big.Rat) int64 {
	num, den := new(big.Int).Set(r.Num()), r.Denom()
	neg := num.Sign() < 0
	num.Abs(num)
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Mul(m, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	return q.Int64()
}

// valueCents = round(qty / 10^scale * price * 100).
func valueCents(qty int64, scale int, price string) (int64, bool) {
	p, ok := new(big.Rat).SetString(strings.TrimSpace(price))
	if !ok || price == "" {
		return 0, false
	}
	r := new(big.Rat).Mul(new(big.Rat).SetInt64(qty), p)
	r.Mul(r, big.NewRat(100, 1))
	r.Quo(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)))
	return ratRound(r), true
}

func sum(ps []piece) (q, c int64) {
	for _, p := range ps {
		q += p.qty
		c += p.cost
	}
	return
}

type exEvent struct {
	ref, date, first string
	t                float64
	rows             []ExchangeLedgerRow
}

type usdXfer struct {
	ev      *exEvent
	entry   int   // index into the exchange's pending entries (-1 when only the bank side books it)
	cents   int64 // + into the exchange
	date    string
	matched int // index into bank candidates, -1 if none
}

// scMove is one crypto withdrawal to (or deposit from) self-custody seen by an exchange
// builder: the self-custody wallet layer (wallets.go) attributes it to a wallet.
type scMove struct {
	Key       string // <exchange>:<refid>
	XID       string // journal entry external id (in-books moves)
	Date      string
	Commodity string
	Qty       int64   // quantity moved (withdrawal: sent to the wallet; deposit: credited)
	Pieces    []piece // withdrawal: lots at exchange FIFO cost
	Deposit   bool
	InBooks   bool
}

// cryptoCtx carries what the exchange builders and the wallet layer exchange with each other.
type cryptoCtx struct {
	// depositCost: lots (cost basis carried through a wallet) for crypto deposits that came
	// from a known wallet, keyed <exchange>:<refid>; replaces the exchange's own FIFO guess.
	depositCost map[string][]piece
	moves       []scMove
}

// buildExchanges books every staged exchange. It runs after the connector rows are booked so it
// can match USD transfers against the bank side.
func buildExchanges(in Input, cfg *Config, res *Result, byKey map[string]string, ctx *cryptoCtx) error {
	if ctx == nil {
		ctx = &cryptoCtx{}
	}
	for _, ex := range in.Exchanges {
		if err := buildExchange(ex, cfg, res, byKey, ctx); err != nil {
			return fmt.Errorf("%s: %w", ex.Name, err)
		}
	}
	return nil
}

func buildExchange(ex ExchangeData, cfg *Config, res *Result, byKey map[string]string, ctx *cryptoCtx) error {
	acct, ok := byKey[ex.Name]
	if !ok {
		return fmt.Errorf("no account with key %q in config/coa.yaml", ex.Name)
	}
	self := SelfCustodyAcct
	if n, ok := byKey["crypto_selfcustody"]; ok {
		self = n
	}
	cut := cfg.Map.BooksStart
	cp := strings.ToUpper(ex.Name[:1]) + ex.Name[1:]

	// ---- group ledger rows into events (refid), in time order ----
	byRef := map[string]*exEvent{}
	var events []*exEvent
	for _, r := range ex.Ledger {
		t, err := strconv.ParseFloat(r.Time, 64)
		if err != nil {
			return fmt.Errorf("ledger %s time: %w", r.ID, err)
		}
		e := byRef[r.RefID]
		if e == nil {
			e = &exEvent{ref: r.RefID, date: r.Date, t: t, first: r.ID}
			byRef[r.RefID] = e
			events = append(events, e)
		}
		if t < e.t || t == e.t && r.ID < e.first {
			e.t, e.date, e.first = t, r.Date, r.ID
		}
		e.rows = append(e.rows, r)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].t != events[j].t {
			return events[i].t < events[j].t
		}
		return events[i].ref < events[j].ref
	})

	// ---- bank-side candidates: connector entries moving USD to/from the exchange account ----
	type bankCand struct {
		idx   int
		cents int64 // + into the exchange
		date  string
		used  bool
	}
	var banks []*bankCand
	firstActivity := ""
	for i, e := range res.Entries {
		if e.Source != "connector" || len(e.Lines) != 2 {
			continue
		}
		for _, l := range e.Lines {
			if l.Account == acct && l.Commodity == "" {
				banks = append(banks, &bankCand{idx: i, cents: l.Amount, date: e.Date})
				if firstActivity == "" || e.Date < firstActivity {
					firstActivity = e.Date
				}
			}
		}
	}
	for _, e := range events {
		if e.date > cut && (firstActivity == "" || e.date < firstActivity) {
			firstActivity = e.date
		}
	}
	openDate := cut
	if firstActivity != "" {
		if d := dayBefore(firstActivity); d > openDate {
			openDate = d
		}
	}

	kr, sc := pool{}, pool{}
	var usdExact int64          // exchange USD at 1e-4
	var usdBooked int64         // USD cents booked on the exchange account (cash lines)
	book := map[string]int64{}  // commodity -> ledger value (cents) of the exchange account
	qtyBk := map[string]int64{} // commodity -> ledger quantity
	var entries []Entry
	var xfers []*usdXfer
	opened := false
	notes := map[string]bool{}

	addCom := func(c string) {
		if c == USD {
			return
		}
		if _, ok := res.Commodities[c]; !ok {
			res.Commodities[c] = Commodity{Code: c, Scale: CryptoScale, Kind: "crypto", Name: c}
		}
	}
	qptr := func(q int64) *int64 { return &q }
	exLine := func(c string, amt, qty int64, cs, memo, ref string) Line {
		if c == USD {
			usdBooked += amt
			return Line{Account: acct, Amount: amt, Currency: USD, CategorySource: cs, Memo: memo, RawRef: ref}
		}
		addCom(c)
		book[c] += amt
		qtyBk[c] += qty
		return Line{Account: acct, Amount: amt, Currency: USD, Quantity: qptr(qty), Commodity: c, CategorySource: cs, Memo: memo, RawRef: ref}
	}
	usdDelta := func(newExact int64) int64 {
		usdExact = newExact
		target := mulDiv(newExact, 1, 100) // 1e-4 -> cents, half away from zero
		return target - usdBooked
	}
	price := func(c, date string, t float64) (string, bool) {
		cl := ex.Closes[c]
		if cl == nil {
			return "", false
		}
		d := time.Unix(int64(t), 0).UTC()
		for i := 0; i < 7; i++ {
			if p, ok := cl[d.AddDate(0, 0, -i).Format("2006-01-02")]; ok {
				return p, true
			}
		}
		return "", false
	}
	open := func() {
		if opened {
			return
		}
		opened = true
		usdBooked = 0 // pre-books USD is booked by the opening line below
		var lines []Line
		var total int64
		if c := mulDiv(usdExact, 1, 100); c != 0 {
			lines = append(lines, exLine(USD, c, 0, "opening", "USD cash", ""))
			total += c
		}
		var coms []string
		for c := range kr {
			coms = append(coms, c)
		}
		sort.Strings(coms)
		for _, c := range coms {
			q, cost := sum(lotsAsPieces(kr[c]))
			for _, l := range kr[c] {
				l.inBooks = true
			}
			if q == 0 && cost == 0 {
				continue
			}
			lines = append(lines, exLine(c, cost, q, "opening", "position at FIFO cost from exchange history", ""))
			total += cost
		}
		if len(lines) == 0 {
			lines = append(lines, Line{Account: acct, Amount: 0, Currency: USD, CategorySource: "opening"})
		}
		lines = append(lines, Line{Account: OpeningEquity, Amount: -total, Currency: USD, CategorySource: "opening"})
		res.Openings = append(res.Openings, Opening{Account: acct, Date: openDate, Amount: total, Basis: ex.Name + " history to " + cut + " (positions at FIFO cost)"})
		entries = append(entries, Entry{Date: openDate, Description: "Opening balance (" + ex.Name + " history to " + cut + ", positions at cost)", Type: "opening",
			Source: "opening", ExternalID: "opening:" + ex.Name, Lines: lines})
	}

	for _, ev := range events {
		inBooks := ev.date > cut
		if inBooks {
			open()
		}
		// per-commodity gross amounts and fees
		type agg struct{ amt, fee int64 }
		per := map[string]*agg{}
		var coms []string
		types := map[string]bool{}
		subtypes := map[string]bool{}
		for _, r := range ev.rows {
			scale := CryptoScale
			if r.Commodity == USD {
				scale = exUSDScale
			}
			a, err := money.ParseMinor(r.Amount, scale)
			if err != nil {
				return fmt.Errorf("ledger %s amount: %w", r.ID, err)
			}
			f, err := money.ParseMinor(orZero(r.Fee), scale)
			if err != nil {
				return fmt.Errorf("ledger %s fee: %w", r.ID, err)
			}
			if per[r.Commodity] == nil {
				per[r.Commodity] = &agg{}
				coms = append(coms, r.Commodity)
			}
			per[r.Commodity].amt += a
			per[r.Commodity].fee += f
			types[r.Type] = true
			subtypes[r.Subtype] = true
		}
		sort.Strings(coms)
		xid := ex.Name + ":" + ev.ref
		ref := ev.first
		var lines []Line
		desc := ""
		typ := "normal"
		counter := InvFeesAcct // zero-value counter line when only quantities move

		only := func(ts ...string) bool {
			for t := range types {
				found := false
				for _, x := range ts {
					if t == x {
						found = true
					}
				}
				if !found {
					return false
				}
			}
			return true
		}
		fees := func(usdFee int64, crypto int64) []Line {
			if v := usdFee + crypto; v != 0 {
				return []Line{{Account: InvFeesAcct, Amount: v, Currency: USD, CategorySource: "exchange:fee", Counterparty: cp}}
			}
			return nil
		}

		switch {
		case only("deposit", "withdrawal") && len(coms) == 1:
			c := coms[0]
			a := per[c]
			if c == USD {
				delta := usdDelta(usdExact + a.amt - a.fee)
				xfer := mulDiv(a.amt, 1, 100)
				feeC := xfer - delta
				desc = cp + " USD " + map[bool]string{true: "deposit", false: "withdrawal"}[a.amt > 0]
				typ = "transfer"
				if !inBooks {
					usdBooked += delta
					continue
				}
				// the transfer leg itself is placed after matching; fees are booked here
				usdBooked += xfer
				x := &usdXfer{ev: ev, cents: xfer, date: ev.date, matched: -1, entry: len(entries)}
				xfers = append(xfers, x)
				lines = append(lines, Line{Account: acct, Amount: xfer, Currency: USD, CategorySource: "transfer:" + ex.Name, RawRef: ref})
				if feeC != 0 {
					lines = append(lines, exLine(USD, -feeC, 0, "exchange:fee", "withdrawal/deposit fee", ""))
					lines = append(lines, fees(feeC, 0)...)
				}
				entries = append(entries, Entry{Date: ev.date, Description: desc, Counterparty: "self", Type: typ, Source: "connector", ExternalID: xid, Lines: lines})
				continue
			}
			addCom(c)
			if a.amt > 0 { // crypto deposit from self-custody
				desc = cp + " " + c + " deposit (from self-custody)"
				counter = OpeningEquity
				typ = "transfer"
				var ps []piece
				if ov, ok := ctx.depositCost[ex.Name+":"+ev.ref]; ok {
					ps = fitPieces(ov, a.amt) // cost carried through the sending wallet
					desc = cp + " " + c + " deposit (from self-custody wallet)"
				} else {
					ps = sc.take(c, a.amt, true)
				}
				ctx.moves = append(ctx.moves, scMove{Key: ex.Name + ":" + ev.ref, XID: xid, Date: ev.date, Commodity: c, Qty: a.amt, Deposit: true, InBooks: inBooks})
				var inQ, inC, preC int64
				var pre []string
				for _, p := range ps {
					kr.add(c, &lot{qty: p.qty, cost: p.cost, acquired: firstNonEmptyS(p.acquired, ev.date), inBooks: true})
					switch {
					case p.unknown:
						notes[fmt.Sprintf("%s %s: %s %s deposited with no known lot (basis 0)", ev.date, ev.ref, money.Format(p.qty, CryptoScale), c)] = true
					case p.inBooks:
						inQ += p.qty
						inC += p.cost
					default:
						preC += p.cost
						pre = append(pre, p.acquired)
					}
				}
				_, totC := sum(ps)
				if inBooks {
					lines = append(lines, exLine(c, totC, a.amt, "transfer:selfcustody", "", ref))
					if inQ != 0 || inC != 0 {
						lines = append(lines, Line{Account: self, Amount: -inC, Currency: USD, Quantity: qptr(-inQ), Commodity: c, CategorySource: "transfer:selfcustody"})
					}
					if totC-inC != 0 || len(lines) < 2 {
						lines = append(lines, Line{Account: OpeningEquity, Amount: -(totC - inC), Currency: USD, CategorySource: "opening",
							Memo: "self-custody " + c + " not previously in the books (lots withdrawn before books start at exchange cost; otherwise basis 0)"})
					}
				}
				if a.fee != 0 {
					fq, fc := sum(kr.take(c, a.fee, false))
					if inBooks {
						lines = append(lines, exLine(c, -fc, -fq, "exchange:fee", "deposit fee", ""))
						lines = append(lines, fees(0, fc)...)
					}
				}
			} else { // crypto withdrawal to self-custody
				desc = cp + " " + c + " withdrawal (to self-custody)"
				typ = "transfer"
				ps := kr.take(c, -a.amt, false)
				q, cost := sum(ps)
				for _, p := range ps {
					sc.add(c, &lot{qty: p.qty, cost: p.cost, acquired: firstNonEmptyS(p.acquired, ev.date), inBooks: inBooks})
				}
				mv := scMove{Key: ex.Name + ":" + ev.ref, XID: xid, Date: ev.date, Commodity: c, Qty: q, InBooks: inBooks}
				for _, p := range ps {
					mv.Pieces = append(mv.Pieces, piece{qty: p.qty, cost: p.cost, acquired: firstNonEmptyS(p.acquired, ev.date)})
				}
				ctx.moves = append(ctx.moves, mv)
				if inBooks {
					lines = append(lines, exLine(c, -cost, -q, "transfer:selfcustody", "", ref))
					lines = append(lines, Line{Account: self, Amount: cost, Currency: USD, Quantity: qptr(q), Commodity: c, CategorySource: "transfer:selfcustody"})
				}
				if a.fee != 0 {
					fq, fc := sum(kr.take(c, a.fee, false))
					if inBooks {
						lines = append(lines, exLine(c, -fc, -fq, "exchange:fee", "withdrawal fee", ""))
						lines = append(lines, fees(0, fc)...)
					}
				}
			}

		case only("staking") || only("earn") && subtypes["reward"]:
			desc = cp + " staking reward"
			counter = StakingIncome
			for _, c := range coms {
				a := per[c]
				n := a.amt - a.fee
				if c == USD {
					d := usdDelta(usdExact + n)
					if inBooks {
						lines = append(lines, exLine(USD, d, 0, "exchange:staking", "", ref))
						lines = append(lines, Line{Account: StakingIncome, Amount: -d, Currency: USD, CategorySource: "exchange:staking", Counterparty: cp})
					} else {
						usdBooked += d
					}
					continue
				}
				addCom(c)
				var v int64
				if p, ok := price(c, ev.date, ev.t); ok {
					v, _ = valueCents(n, CryptoScale, p)
				} else {
					notes[fmt.Sprintf("%s: no %s USD close for staking reward %s (income 0)", ev.date, c, ev.ref)] = true
				}
				if n > 0 {
					kr.add(c, &lot{qty: n, cost: v, acquired: ev.date, inBooks: inBooks})
				} else if n < 0 {
					_, v2 := sum(kr.take(c, -n, false))
					v = -v2
				}
				if inBooks {
					lines = append(lines, exLine(c, v, n, "exchange:staking", "", ref))
					lines = append(lines, Line{Account: StakingIncome, Amount: -v, Currency: USD, CategorySource: "exchange:staking", Counterparty: cp})
				}
			}

		case only("trade", "spend", "receive"):
			var sold, bought []string
			for _, c := range coms {
				switch a := per[c]; {
				case a.amt < 0:
					sold = append(sold, c)
				case a.amt > 0:
					bought = append(bought, c)
				default:
					if a.fee != 0 {
						sold = append(sold, c)
					}
				}
			}
			if len(sold) != 1 || len(bought) != 1 {
				return fmt.Errorf("trade %s: expected one sold and one bought asset, got %v / %v", ev.ref, sold, bought)
			}
			s, b := sold[0], bought[0]
			as, ab := per[s], per[b]
			desc = fmt.Sprintf("%s trade %s -> %s", cp, s, b)
			counter = RealizedST
			switch {
			case s == USD: // buy crypto with USD
				addCom(b)
				delta := usdDelta(usdExact + as.amt - as.fee) // negative
				usdFee := mulDiv(as.fee, 1, 100)
				spent := -delta - usdFee // USD paid for the crypto (before fees in the crypto)
				cfv := int64(0)
				if ab.fee != 0 && ab.amt != 0 {
					cfv = mulDiv(spent, ab.fee, ab.amt)
				}
				nq := ab.amt - ab.fee
				kr.add(b, &lot{qty: nq, cost: spent - cfv, acquired: ev.date, inBooks: inBooks})
				if inBooks {
					lines = append(lines, exLine(USD, delta, 0, "exchange:trade", "", ref))
					lines = append(lines, exLine(b, spent-cfv, nq, "exchange:trade", "buy", ""))
					lines = append(lines, fees(usdFee, cfv)...)
				} else {
					usdBooked += delta
				}
			case b == USD: // sell crypto for USD
				psSold := kr.take(s, -as.amt, false)
				psFee := kr.take(s, as.fee, false)
				delta := usdDelta(usdExact + ab.amt - ab.fee) // positive
				usdFee := mulDiv(ab.fee, 1, 100)
				proceeds := delta + usdFee
				st, lt := gains(psSold, proceeds, ev.date)
				q1, c1 := sum(psSold)
				q2, c2 := sum(psFee)
				if inBooks {
					addCom(s)
					lines = append(lines, exLine(s, -(c1+c2), -(q1+q2), "exchange:trade", "sell", ref))
					lines = append(lines, exLine(USD, delta, 0, "exchange:trade", "", ""))
					lines = append(lines, fees(usdFee, c2)...)
					lines = append(lines, gainLines(st, lt, cp)...)
				} else {
					usdBooked += delta
				}
			default: // crypto -> crypto, valued at the daily USD close
				addCom(s)
				addCom(b)
				psSold := kr.take(s, -as.amt, false)
				psFee := kr.take(s, as.fee, false)
				q1, c1 := sum(psSold)
				q2, c2 := sum(psFee)
				var v int64
				valued := false
				if p, ok := price(s, ev.date, ev.t); ok {
					v, valued = valueCents(-as.amt, CryptoScale, p)
				}
				if !valued {
					if p, ok := price(b, ev.date, ev.t); ok {
						v, valued = valueCents(ab.amt, CryptoScale, p)
					}
				}
				if !valued {
					v = c1 // no price: carry the basis over, no gain
					notes[fmt.Sprintf("%s %s: no USD close for %s or %s; basis carried over", ev.date, ev.ref, s, b)] = true
				}
				fyv := int64(0)
				if ab.fee != 0 && ab.amt != 0 {
					fyv = mulDiv(v, ab.fee, ab.amt)
				}
				nq := ab.amt - ab.fee
				kr.add(b, &lot{qty: nq, cost: v - fyv, acquired: ev.date, inBooks: inBooks})
				st, lt := gains(psSold, v, ev.date)
				if inBooks {
					lines = append(lines, exLine(s, -(c1+c2), -(q1+q2), "exchange:trade", "sell", ref))
					lines = append(lines, exLine(b, v-fyv, nq, "exchange:trade", "buy", ""))
					lines = append(lines, fees(0, c2+fyv)...)
					lines = append(lines, gainLines(st, lt, cp)...)
				}
			}

		default: // internal moves (spot <-> earn, autoallocation, migrations) and anything else
			desc = cp + " " + strings.Join(sortedKeys(types), "/")
			counter = cfg.Transfers.Unmatched
			for _, c := range coms {
				a := per[c]
				n := a.amt - a.fee
				if n == 0 {
					continue
				}
				if c == USD {
					d := usdDelta(usdExact + n)
					if inBooks {
						lines = append(lines, exLine(USD, d, 0, "unmatched:"+ex.Name+"-"+desc, "", ref))
						lines = append(lines, Line{Account: cfg.Transfers.Unmatched, Amount: -d, Currency: USD, CategorySource: "unmatched:" + ex.Name})
					} else {
						usdBooked += d
					}
					continue
				}
				addCom(c)
				var v int64
				if n > 0 {
					if p, ok := price(c, ev.date, ev.t); ok {
						v, _ = valueCents(n, CryptoScale, p)
					}
					kr.add(c, &lot{qty: n, cost: v, acquired: ev.date, inBooks: inBooks})
				} else {
					_, cost := sum(kr.take(c, -n, false))
					v = -cost
				}
				if inBooks {
					typ = "transfer"
					lines = append(lines, exLine(c, v, n, "unmatched:"+ex.Name, "", ref))
					lines = append(lines, Line{Account: cfg.Transfers.Unmatched, Amount: -v, Currency: USD, CategorySource: "unmatched:" + ex.Name})
				}
			}
		}
		if !inBooks {
			continue
		}
		lines = compact(lines)
		if len(lines) == 0 {
			continue
		}
		if len(lines) == 1 {
			lines = append(lines, Line{Account: counter, Amount: 0, Currency: USD, CategorySource: lines[0].CategorySource})
		}
		entries = append(entries, Entry{Date: ev.date, Description: desc, Counterparty: cp, Type: typ, Source: "connector", ExternalID: xid, Lines: lines})
	}
	open() // an exchange with history only before the books still gets its opening

	// ---- match USD transfers to bank rows ----
	type cand struct {
		x   *usdXfer
		b   *bankCand
		gap int
	}
	var cands []cand
	for _, x := range xfers {
		for _, b := range banks {
			if b.cents != x.cents {
				continue
			}
			g := dayGap(x.date, b.date)
			if g <= cfg.Transfers.WindowHinted {
				cands = append(cands, cand{x, b, g})
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.gap != b.gap {
			return a.gap < b.gap
		}
		if a.x.date != b.x.date {
			return a.x.date < b.x.date
		}
		if a.x.ev.ref != b.x.ev.ref {
			return a.x.ev.ref < b.x.ev.ref
		}
		return res.Entries[a.b.idx].ExternalID < res.Entries[b.b.idx].ExternalID
	})
	for _, c := range cands {
		if c.x.matched >= 0 || c.b.used {
			continue
		}
		c.b.used = true
		for i, b := range banks {
			if b == c.b {
				c.x.matched = i
			}
		}
	}
	drop := map[int]bool{}
	for _, x := range xfers {
		e := &entries[x.entry]
		if x.matched < 0 {
			e.Type = "transfer"
			e.Lines = append(e.Lines, Line{Account: cfg.Transfers.Unmatched, Amount: -x.cents, Currency: USD, CategorySource: "unmatched:" + ex.Name + "-usd"})
			e.Lines[0].CategorySource = "unmatched:" + ex.Name + "-usd"
			continue
		}
		b := banks[x.matched]
		be := &res.Entries[b.idx]
		bankRef := ""
		for _, l := range be.Lines {
			if l.RawRef != "" {
				bankRef = l.RawRef
			}
		}
		res.Pairs = append(res.Pairs, Pair{A: bankRef, B: ex.Name + ":" + x.ev.first, Kind: map[bool]string{true: "same-day", false: "in-transit"}[b.date == x.date],
			Hint: ex.Name, GapDays: dayGap(b.date, x.date), AmountAbs: absMinor(x.cents)})
		if b.date == x.date {
			// the bank entry already moves the money into/out of the exchange account
			e.Lines = e.Lines[1:]
			if len(e.Lines) == 0 {
				drop[x.entry] = true
			} else {
				e.Type = "normal"
			}
			continue
		}
		link := "xfer:" + bankRef + "+" + ex.Name + ":" + x.ev.first
		for i := range be.Lines {
			if be.Lines[i].Account == acct && be.Lines[i].Commodity == "" {
				be.Lines[i].Account = cfg.Transfers.InTransit
				be.Lines[i].CategorySource = "transfer:" + ex.Name
			}
		}
		be.LinkID = link
		e.LinkID = link
		e.Lines = append([]Line{e.Lines[0], {Account: cfg.Transfers.InTransit, Amount: -x.cents, Currency: USD, CategorySource: "transfer:" + ex.Name}}, e.Lines[1:]...)
	}
	for i, e := range entries {
		if !drop[i] {
			res.Entries = append(res.Entries, e)
		}
	}

	// ---- mark to market at the pull's ticker prices + assertion ----
	if ex.AsOf != "" {
		var mtm []Line
		var mtmTotal int64
		var coms []string
		for c := range qtyBk {
			coms = append(coms, c)
		}
		sort.Strings(coms)
		for _, c := range coms {
			p := ex.Ticker[c]
			if p == "" {
				if cl := ex.Closes[c]; cl != nil {
					last := ""
					for d := range cl {
						if d <= ex.AsOf && d > last {
							last = d
						}
					}
					p = cl[last]
				}
			}
			mv := int64(0)
			if qtyBk[c] != 0 {
				var ok bool
				if mv, ok = valueCents(qtyBk[c], CryptoScale, p); !ok {
					notes[fmt.Sprintf("no USD price for %s; position left at cost", c)] = true
					continue
				}
				res.Prices = append(res.Prices, Price{Commodity: c, Date: ex.AsOf, Price: p, Source: ex.Name + "-ticker"})
			}
			if d := mv - book[c]; d != 0 {
				mtm = append(mtm, Line{Account: acct, Amount: d, Currency: USD, Quantity: qptr(0), Commodity: c, UnitPrice: p, CategorySource: "valuation", Memo: "mark to market"})
				mtmTotal += d
			}
		}
		if len(mtm) > 0 {
			mtm = append(mtm, Line{Account: UnrealizedAcc, Amount: -mtmTotal, Currency: USD, CategorySource: "valuation"})
			res.Entries = append(res.Entries, Entry{Date: ex.AsOf, Description: "Mark to market (" + ex.Name + " ticker)", Type: "valuation",
				Source: "valuation", ExternalID: "valuation:" + ex.Name + ":" + ex.AsOf, Lines: mtm})
		}
		// expected value from the exchange-reported balances
		rep := map[string]int64{}
		var repUSD int64
		for _, b := range ex.Balances {
			if b.Commodity == USD {
				v, err := money.ParseMinor(b.Balance, exUSDScale)
				if err != nil {
					return fmt.Errorf("balance %s: %w", b.Asset, err)
				}
				repUSD += v
				continue
			}
			v, err := money.ParseMinor(b.Balance, CryptoScale)
			if err != nil {
				return fmt.Errorf("balance %s: %w", b.Asset, err)
			}
			rep[b.Commodity] += v
		}
		expected := mulDiv(repUSD, 1, 100)
		var allComs []string
		for c := range rep {
			allComs = append(allComs, c)
		}
		sort.Strings(allComs)
		for _, c := range allComs {
			if rep[c] == 0 {
				continue
			}
			p := ex.Ticker[c]
			if p == "" {
				for _, l := range res.Prices {
					if l.Commodity == c && l.Source == ex.Name+"-ticker" {
						p = l.Price
					}
				}
			}
			v, ok := valueCents(rep[c], CryptoScale, p)
			if !ok {
				notes[fmt.Sprintf("no USD price for reported %s; valued at 0 in the balance assertion", c)] = true
			}
			expected += v
		}
		res.Assertions = append(res.Assertions, Assertion{Account: acct, AsOf: ex.AsOf, Amount: expected, Source: ex.Name})
	}
	for n := range notes {
		res.Notes = append(res.Notes, ex.Name+": "+n)
	}
	sort.Strings(res.Notes)
	return nil
}

// fitPieces copies wallet lots for an exchange deposit: all are in the books (the wallet carried
// them) and the quantities are scaled so they sum to the credited quantity (cost unchanged).
func fitPieces(ps []piece, qty int64) []piece {
	out := make([]piece, len(ps))
	var tot int64
	for i, p := range ps {
		out[i] = piece{qty: p.qty, cost: p.cost, acquired: p.acquired, inBooks: true}
		tot += p.qty
	}
	if len(out) == 0 {
		return []piece{{qty: qty, inBooks: true}}
	}
	if tot != qty {
		var acc int64
		for i := range out {
			if i == len(out)-1 {
				out[i].qty = qty - acc
				break
			}
			out[i].qty = mulDiv(out[i].qty, qty, tot)
			acc += out[i].qty
		}
	}
	return out
}

func lotsAsPieces(ls []*lot) []piece {
	out := make([]piece, 0, len(ls))
	for _, l := range ls {
		out = append(out, piece{qty: l.qty, cost: l.cost, acquired: l.acquired, inBooks: l.inBooks})
	}
	return out
}

// gains splits proceeds over the relieved pieces by quantity and returns short- and long-term
// gains (held more than one year = long term).
func gains(ps []piece, proceeds int64, date string) (st, lt int64) {
	q, _ := sum(ps)
	if q == 0 {
		return proceeds, 0
	}
	rest := proceeds
	for i, p := range ps {
		pr := mulDiv(proceeds, p.qty, q)
		if i == len(ps)-1 {
			pr = rest
		}
		rest -= pr
		g := pr - p.cost
		if !p.unknown && p.acquired != "" && longTerm(p.acquired, date) {
			lt += g
		} else {
			st += g
		}
	}
	return st, lt
}

func longTerm(acquired, sold string) bool {
	a, err1 := time.Parse("2006-01-02", acquired)
	s, err2 := time.Parse("2006-01-02", sold)
	if err1 != nil || err2 != nil {
		return false
	}
	return s.After(a.AddDate(1, 0, 0))
}

func gainLines(st, lt int64, cp string) []Line {
	var out []Line
	if st != 0 {
		out = append(out, Line{Account: RealizedST, Amount: -st, Currency: USD, CategorySource: "exchange:trade", Counterparty: cp})
	}
	if lt != 0 {
		out = append(out, Line{Account: RealizedLT, Amount: -lt, Currency: USD, CategorySource: "exchange:trade", Counterparty: cp})
	}
	return out
}

// compact drops zero-value lines that carry no quantity.
func compact(ls []Line) []Line {
	out := ls[:0]
	for _, l := range ls {
		if l.Amount == 0 && (l.Quantity == nil || *l.Quantity == 0) {
			continue
		}
		out = append(out, l)
	}
	return out
}

func dayBefore(d string) string {
	t, _ := time.Parse("2006-01-02", d)
	return t.AddDate(0, 0, -1).Format("2006-01-02")
}

func dayGap(a, b string) int {
	ta, _ := time.Parse("2006-01-02", a)
	tb, _ := time.Parse("2006-01-02", b)
	g := int(ta.Sub(tb).Hours() / 24)
	if g < 0 {
		g = -g
	}
	return g
}

func firstNonEmptyS(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
