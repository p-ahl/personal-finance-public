package books

import (
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strings"

	"github.com/p-ahl/personal-finance/internal/money"
)

// Self-custody wallets (staged by `finance import wallets`; see docs/schema.md §3d).
//
// The wallet layer replays one global timeline per wallet account (FIFO lots per coin):
//
//   - exchange_out: an exchange withdrawal received by a wallet. The lots carry the exchange's
//     FIFO cost; in the books the exchange entry's self-custody line is re-pointed to the
//     wallet account. Sends before the staged exchange history (coinbase-prebooks) come in at
//     market value on receipt (estimate);
//   - exchange_in: a wallet send credited by an exchange. The wallet's FIFO lots (cost and
//     acquisition dates) become the exchange's lots for that deposit (cryptoCtx.depositCost),
//     so cost basis is carried through the wallet instead of the exchange guessing it;
//   - transfer: between own wallets at cost; the network fee is expensed at cost;
//   - swap: coin X out, coin Y in (swap service / in-wallet exchange) = a crypto-to-crypto
//     disposal valued at the daily price of X (estimate): realized gain short/long term, the
//     new lot's cost is that value;
//   - unmatched_in / unmatched_out: no counterpart found. Inflows come in at market value on
//     receipt, outflows leave at FIFO cost; in the books both go against
//     Assets:Crypto:SelfCustody:Unassigned (no P&L), which the at-cost adjustment clears;
//   - passthrough: someone else's money that crossed a wallet: never booked.
//
// History up to books_start only updates state; the wallet holdings at that date are one
// opening entry at their replayed cost. At every wallet pull date the at-cost adjustment sets
// each wallet account's quantity to the on-chain balance and moves whatever is left in the
// Unassigned account (exchange withdrawals not found in any wallet, unmatched items) into the
// wallet holdings pro rata by market value (total cost unchanged, no P&L). The newest pull
// marks every wallet to market (Income:Investments:Unrealized-Gains) and adds a balance
// assertion (source = wallets) per wallet account; `finance check` also compares quantities.
const (
	WalletSource   = "wallets"
	SelfUnassigned = "Assets:Crypto:SelfCustody:Unassigned"
)

type WalletDef struct {
	Name, Account string
	PerCoin       bool
	Note          string
}

// AccountFor is the ledger account holding commodity c.
func (w WalletDef) AccountFor(c string) string {
	if w.PerCoin {
		return w.Account + ":" + c
	}
	return w.Account
}

type WalletSnapshot struct{ Wallet, AsOf, Commodity, Account, Balance string }
type WalletPrice struct{ Commodity, AsOf, Price, Source string }

type WalletEvent struct {
	ID, Time, Date, Kind               string
	WalletOut, CoinOut, QtyOut, FeeOut string
	WalletIn, CoinIn, QtyIn            string
	Ref, UsdOut, UsdIn                 string
	Confidence, Evidence, Memo         string
}

type WalletData struct {
	Wallets   []WalletDef
	Snapshots []WalletSnapshot
	Prices    []WalletPrice
	Events    []WalletEvent
}

// WalletResult is the replay outcome of one event (also for events before the books).
type WalletResult struct {
	ID, Date, Kind string
	Booked         bool  // in the books (after books_start)
	Cost           int64 // cost basis relieved (cents)
	Proceeds       int64 // swap: value received/given (cents)
	ShortTerm      int64 // realized gain (swap)
	LongTerm       int64
	Fee            int64 // network fee at cost (expensed)
	Note           string
}

// buildCrypto books the exchanges and the self-custody wallets. A deposit's cost depends on the
// wallet replay, which depends on the exchanges' withdrawal costs, so the two are iterated to a
// fixed point (time order makes this converge; each pass fixes at least the earliest deposit).
func buildCrypto(in Input, cfg *Config, res *Result, byKey map[string]string) error {
	if len(in.Wallets.Wallets) == 0 {
		return buildExchanges(in, cfg, res, byKey, nil)
	}
	snap := snapshotResult(res)
	over := map[string][]piece{}
	for iter := 0; iter < 25; iter++ {
		ctx := &cryptoCtx{depositCost: over}
		if err := buildExchanges(in, cfg, res, byKey, ctx); err != nil {
			return err
		}
		next, err := buildWallets(in, cfg, res, byKey, ctx)
		if err != nil {
			return fmt.Errorf("wallets: %w", err)
		}
		if reflect.DeepEqual(next, over) {
			return nil
		}
		restoreResult(res, snap)
		over = next
	}
	return fmt.Errorf("wallets: deposit cost basis did not converge")
}

type resultSnap struct {
	entries                                    []Entry
	prices, assertions, notes, openings, pairs int
	commodities                                map[string]Commodity
	walletResults                              int
}

func cloneEntries(es []Entry) []Entry {
	out := make([]Entry, len(es))
	for i, e := range es {
		e.Lines = append([]Line(nil), e.Lines...)
		out[i] = e
	}
	return out
}

func snapshotResult(res *Result) resultSnap {
	c := map[string]Commodity{}
	for k, v := range res.Commodities {
		c[k] = v
	}
	return resultSnap{entries: cloneEntries(res.Entries), prices: len(res.Prices), assertions: len(res.Assertions), notes: len(res.Notes),
		openings: len(res.Openings), pairs: len(res.Pairs), commodities: c, walletResults: len(res.WalletResults)}
}

func restoreResult(res *Result, s resultSnap) {
	res.Entries = cloneEntries(s.entries)
	res.Prices, res.Assertions, res.Notes = res.Prices[:s.prices], res.Assertions[:s.assertions], res.Notes[:s.notes]
	res.Openings, res.Pairs = res.Openings[:s.openings], res.Pairs[:s.pairs]
	res.WalletResults = res.WalletResults[:s.walletResults]
	res.Commodities = map[string]Commodity{}
	for k, v := range s.commodities {
		res.Commodities[k] = v
	}
}

// parseQty parses a decimal coin amount to CryptoScale minor units (rounded half away from zero).
func parseQty(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("bad quantity %q", s)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(CryptoScale), nil)))
	return ratRound(r), nil
}

func parseCents(s string) (int64, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return 0, fmt.Errorf("bad USD amount %q", s)
	}
	return ratRound(r.Mul(r, big.NewRat(100, 1))), nil
}

func buildWallets(in Input, cfg *Config, res *Result, byKey map[string]string, ctx *cryptoCtx) (map[string][]piece, error) {
	wd := in.Wallets
	cut := cfg.Map.BooksStart
	self := SelfCustodyAcct
	if n, ok := byKey["crypto_selfcustody"]; ok {
		self = n
	}
	defs := map[string]WalletDef{}
	for _, w := range wd.Wallets {
		defs[w.Name] = w
	}
	acctOf := func(wallet, coin string) (string, error) {
		w, ok := defs[wallet]
		if !ok {
			return "", fmt.Errorf("unknown wallet %q", wallet)
		}
		return w.AccountFor(coin), nil
	}
	addCom := func(c string) {
		if _, ok := res.Commodities[c]; !ok && c != USD {
			res.Commodities[c] = Commodity{Code: c, Scale: CryptoScale, Kind: "crypto", Name: c}
		}
	}
	qptr := func(q int64) *int64 { return &q }
	moves := map[string]scMove{}
	for _, m := range ctx.moves {
		moves[m.Key] = m
	}
	entryIdx := map[string]int{}
	for i, e := range res.Entries {
		entryIdx[e.ExternalID] = i
	}
	retarget := func(xid, coin, acct string, qty int64) bool {
		i, ok := entryIdx[xid]
		if !ok {
			return false
		}
		for j := range res.Entries[i].Lines {
			l := &res.Entries[i].Lines[j]
			if l.Account == self && l.Commodity == coin {
				l.Account = acct
				l.Quantity = qptr(qty)
				l.Memo = "self-custody wallet"
				return true
			}
		}
		return false
	}

	pools := map[string]pool{} // account -> commodity -> lots
	pl := func(a string) pool {
		if pools[a] == nil {
			pools[a] = pool{}
		}
		return pools[a]
	}
	notes := map[string]bool{}
	deposit := map[string][]piece{}
	pending := map[string][]piece{} // round trips in flight
	var entries []Entry
	opened := false
	open := func() {
		if opened {
			return
		}
		opened = true
		var lines []Line
		var total int64
		var accts []string
		for a := range pools {
			accts = append(accts, a)
		}
		sort.Strings(accts)
		for _, a := range accts {
			var coms []string
			for c := range pools[a] {
				coms = append(coms, c)
			}
			sort.Strings(coms)
			for _, c := range coms {
				for _, l := range pools[a][c] {
					l.inBooks = true
				}
				q, cost := sum(lotsAsPieces(pools[a][c]))
				if q == 0 && cost == 0 {
					continue
				}
				addCom(c)
				lines = append(lines, Line{Account: a, Amount: cost, Currency: USD, Quantity: qptr(q), Commodity: c, CategorySource: "opening",
					Memo: "wallet holdings at books start, cost replayed from exchange withdrawals and on-chain history (estimate)"})
				total += cost
			}
		}
		if len(lines) == 0 {
			return
		}
		lines = append(lines, Line{Account: OpeningEquity, Amount: -total, Currency: USD, CategorySource: "opening"})
		res.Openings = append(res.Openings, Opening{Account: "Assets:Crypto:SelfCustody (wallets)", Date: cut, Amount: total, Basis: "wallet history to " + cut})
		entries = append(entries, Entry{Date: cut, Description: "Opening balance (self-custody wallets: on-chain history to " + cut + ", lots at replayed cost)",
			Type: "opening", Source: "opening", ExternalID: "opening:wallets", Lines: lines})
	}

	evs := append([]WalletEvent(nil), wd.Events...)
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].Time != evs[j].Time {
			return evs[i].Time < evs[j].Time
		}
		return evs[i].ID < evs[j].ID
	})
	// snapshot dates (ascending) and per-date balances / prices
	snapDates := []string{}
	snapBal := map[string]map[string]map[string]int64{} // date -> account -> commodity -> qty
	for _, s := range wd.Snapshots {
		if snapBal[s.AsOf] == nil {
			snapBal[s.AsOf] = map[string]map[string]int64{}
			snapDates = append(snapDates, s.AsOf)
		}
		q, err := parseQty(s.Balance)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s %s: %w", s.Wallet, s.Commodity, err)
		}
		if snapBal[s.AsOf][s.Account] == nil {
			snapBal[s.AsOf][s.Account] = map[string]int64{}
		}
		snapBal[s.AsOf][s.Account][s.Commodity] = q
		addCom(s.Commodity)
		if err := res.addAccount(cfg, s.Account, "", "", ""); err != nil {
			return nil, err
		}
	}
	sort.Strings(snapDates)
	prices := map[string]map[string]WalletPrice{}
	for _, p := range wd.Prices {
		if prices[p.AsOf] == nil {
			prices[p.AsOf] = map[string]WalletPrice{}
		}
		prices[p.AsOf][p.Commodity] = p
	}
	if err := res.addAccount(cfg, self, "", "", ""); err != nil {
		return nil, err
	}

	// adjust trues the books up to the on-chain state at pull date D. It is a pure function of
	// the ledger as built so far (which already contains the adjustments of earlier pulls), so
	// it never tracks "already cleared" amounts on the side: the Unassigned balance it clears is
	// the ledger balance on D, and the residual quantity is on-chain minus ledger on D. A later
	// pull with the same on-chain data therefore produces no entry, and a rerun reproduces the
	// same entries (the books are rebuilt from staging every run; ids are wallets:adjust:<D>).
	adjust := func(D string) error {
		bal := snapBal[D]
		px := prices[D]
		var lines []Line
		// 1. clear Unassigned: its ledger balance on D (earlier adjustments included), per commodity
		uCost := map[string]int64{}
		uQty := map[string]int64{}
		for _, e := range append(append([]Entry(nil), res.Entries...), entries...) {
			if e.Date > D {
				continue
			}
			for _, l := range e.Lines {
				if l.Account == self {
					uCost[l.Commodity] += l.Amount
					if l.Quantity != nil {
						uQty[l.Commodity] += *l.Quantity
					}
				}
			}
		}
		var ucoms []string
		for c := range uCost {
			ucoms = append(ucoms, c)
		}
		sort.Strings(ucoms)
		var freed int64
		for _, c := range ucoms {
			// Earlier adjustments are already in the ledger sum: subtracting a separately tracked
			// "already cleared" amount as well counted them twice and re-booked the cleared
			// balance back into Unassigned at the next pull (net worth inflated with no real event).
			cost, q := uCost[c], uQty[c]
			if cost == 0 && q == 0 {
				continue
			}
			l := Line{Account: self, Amount: -cost, Currency: USD, CategorySource: "wallets:adjust",
				Memo: "exchange withdrawals / unmatched wallet items not traced to a wallet: carried into wallet holdings at cost"}
			if c != "" {
				l.Commodity, l.Quantity = c, qptr(-q)
			}
			lines = append(lines, l)
			freed += cost
		}
		spread := freed
		// 2. residual quantity per wallet account / coin (dust, fees, unmatched rounding)
		type key struct{ a, c string }
		keys := map[key]bool{}
		for a, m := range bal {
			for c := range m {
				keys[key{a, c}] = true
			}
		}
		for a, p := range pools {
			for c := range p {
				keys[key{a, c}] = true
			}
		}
		var ks []key
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Slice(ks, func(i, j int) bool {
			if ks[i].a != ks[j].a {
				return ks[i].a < ks[j].a
			}
			return ks[i].c < ks[j].c
		})
		// ledger quantity per wallet account (entries so far, on or before D)
		ledQ := map[key]int64{}
		for _, e := range append(append([]Entry(nil), res.Entries...), entries...) {
			if e.Date > D {
				continue
			}
			for _, l := range e.Lines {
				if l.Quantity != nil && l.Commodity != "" && keys[key{l.Account, l.Commodity}] {
					ledQ[key{l.Account, l.Commodity}] += *l.Quantity
				}
			}
		}
		resid := map[key]Line{}
		for _, k := range ks {
			want := int64(0)
			if bal[k.a] != nil {
				want = bal[k.a][k.c]
			}
			dqPool := want - pl(k.a).qty(k.c) // lots (an overdrawn pool sits at 0)
			dqLed := want - ledQ[k]           // ledger (an overdraw shows as a negative quantity)
			if dqPool == 0 && dqLed == 0 {
				continue
			}
			l := Line{Account: k.a, Currency: USD, Quantity: qptr(dqLed), Commodity: k.c, CategorySource: "wallets:adjust", Memo: "on-chain balance true-up (dust, fees, untraced items)"}
			if dqPool < 0 {
				_, c := sum(pl(k.a).take(k.c, -dqPool, false))
				l.Amount = -c
				spread += c
			} else if dqPool > 0 {
				pl(k.a).add(k.c, &lot{qty: dqPool, cost: 0, acquired: D, inBooks: true})
			}
			resid[k] = l
		}
		// 3. spread the freed cost over the holdings by market value
		var val []int64
		var tot int64
		for _, k := range ks {
			q := pl(k.a).qty(k.c)
			v := int64(0)
			if q > 0 {
				if p, ok := px[k.c]; ok {
					v, _ = valueCents(q, CryptoScale, p.Price)
				}
			}
			val = append(val, v)
			tot += v
		}
		if spread != 0 && tot == 0 {
			lines = append(lines, Line{Account: cfg.Transfers.Unmatched, Amount: spread, Currency: USD, CategorySource: "wallets:adjust", Memo: "no wallet holdings to carry the cost"})
			spread = 0
		}
		rest := spread
		lastIdx := -1
		for i := range ks {
			if val[i] > 0 {
				lastIdx = i
			}
		}
		for i, k := range ks {
			share := int64(0)
			if val[i] > 0 && spread != 0 {
				share = mulDiv(spread, val[i], tot)
				if i == lastIdx {
					share = rest
				}
				rest -= share
				// add the cost to the lots pro rata by quantity
				ls := pl(k.a)[k.c]
				q := pl(k.a).qty(k.c)
				left := share
				for j, lt := range ls {
					add := mulDiv(share, lt.qty, q)
					if j == len(ls)-1 {
						add = left
					}
					lt.cost += add
					left -= add
				}
			}
			l, has := resid[k]
			if !has && share == 0 {
				continue
			}
			if !has {
				l = Line{Account: k.a, Currency: USD, Quantity: qptr(0), Commodity: k.c, CategorySource: "wallets:adjust"}
			}
			l.Amount += share
			if share != 0 {
				l.Memo = strings.TrimPrefix(l.Memo+"; ", "; ") + "cost carried in from untraced items (pro rata by market value)"
			}
			lines = append(lines, l)
		}
		lines = compact(lines)
		if len(lines) == 0 {
			return nil
		}
		if len(lines) == 1 {
			lines = append(lines, Line{Account: self, Amount: 0, Currency: USD, CategorySource: "wallets:adjust"})
		}
		entries = append(entries, Entry{Date: D, Description: "Self-custody at-cost adjustment to on-chain wallet balances (" + D + "): untraced items carried at cost, per-swap history only where matched",
			Type: "adjustment", Source: "valuation", ExternalID: "wallets:adjust:" + D, Lines: lines})
		return nil
	}

	si := 0
	flushSnaps := func(upTo string) error { // adjustments for snapshot dates before upTo
		for si < len(snapDates) && snapDates[si] < upTo {
			if snapDates[si] > cut {
				open()
				if err := adjust(snapDates[si]); err != nil {
					return err
				}
			}
			si++
		}
		return nil
	}

	for _, e := range evs {
		if err := flushSnaps(e.Date); err != nil {
			return nil, err
		}
		inBooks := e.Date > cut
		if inBooks {
			open()
		}
		r := WalletResult{ID: e.ID, Date: e.Date, Kind: e.Kind, Booked: inBooks}
		qOut, err := parseQty(e.QtyOut)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.ID, err)
		}
		fOut, err := parseQty(e.FeeOut)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.ID, err)
		}
		qIn, err := parseQty(e.QtyIn)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.ID, err)
		}
		usdOut, err := parseCents(e.UsdOut)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.ID, err)
		}
		usdIn, err := parseCents(e.UsdIn)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.ID, err)
		}
		var aOut, aIn string
		if e.WalletOut != "" {
			if aOut, err = acctOf(e.WalletOut, e.CoinOut); err != nil {
				return nil, fmt.Errorf("%s: %w", e.ID, err)
			}
			addCom(e.CoinOut)
		}
		if e.WalletIn != "" {
			if aIn, err = acctOf(e.WalletIn, e.CoinIn); err != nil {
				return nil, fmt.Errorf("%s: %w", e.ID, err)
			}
			addCom(e.CoinIn)
		}
		desc := ""
		var lines []Line
		feeLines := func(fq, fc int64) {
			if fq == 0 && fc == 0 {
				return
			}
			r.Fee += fc
			lines = append(lines, Line{Account: aOut, Amount: -fc, Currency: USD, Quantity: qptr(-fq), Commodity: e.CoinOut, CategorySource: "wallets:fee", Memo: "network fee"},
				Line{Account: InvFeesAcct, Amount: fc, Currency: USD, CategorySource: "wallets:fee", Counterparty: e.WalletOut})
		}
		switch e.Kind {
		case "passthrough":
			r.Note = "excluded (pass-through)"
		case "exchange_out":
			mv, ok := moves[e.Ref]
			if ok && !mv.Deposit {
				// the wallet receives what the exchange sent (any on-chain difference is trued up
				// by the at-cost adjustment)
				for _, p := range mv.Pieces {
					pl(aIn).add(e.CoinIn, &lot{qty: p.qty, cost: p.cost, acquired: firstNonEmptyS(p.acquired, e.Date), inBooks: inBooks})
				}
				_, c := sum(mv.Pieces)
				r.Cost = c
				if mv.InBooks && !retarget(mv.XID, e.CoinIn, aIn, mv.Qty) {
					notes[fmt.Sprintf("%s: exchange entry %s has no self-custody %s line", e.ID, mv.XID, e.CoinIn)] = true
				}
				if mv.InBooks != inBooks {
					notes[fmt.Sprintf("%s: exchange withdrawal %s and wallet receipt fall on different sides of books start", e.ID, e.Ref)] = true
				}
				break
			}
			// not in the staged exchange history: market value on receipt (estimate)
			pl(aIn).add(e.CoinIn, &lot{qty: qIn, cost: usdIn, acquired: e.Date, inBooks: inBooks})
			r.Cost = usdIn
			r.Note = "cost = market value on receipt (estimate)"
			if inBooks {
				desc = "Self-custody receipt from " + e.Ref + " at market value (estimate)"
				lines = append(lines, Line{Account: aIn, Amount: usdIn, Currency: USD, Quantity: qptr(qIn), Commodity: e.CoinIn, CategorySource: "wallets:unmatched", Memo: e.Memo},
					Line{Account: self, Amount: -usdIn, Currency: USD, CategorySource: "wallets:unmatched"})
			}
		case "exchange_in":
			ps := pl(aOut).take(e.CoinOut, qOut, false)
			for _, p := range ps {
				if p.unknown {
					notes[fmt.Sprintf("%s: %s %s sent to an exchange with no wallet lot (basis 0)", e.ID, money.Format(p.qty, CryptoScale), e.CoinOut)] = true
				}
			}
			_, c := sum(ps)
			r.Cost = c
			if mv, ok := moves[e.Ref]; ok && mv.Deposit {
				cp := make([]piece, len(ps))
				copy(cp, ps)
				deposit[e.Ref] = cp
				if mv.InBooks && !retarget(mv.XID, e.CoinOut, aOut, -qOut) {
					notes[fmt.Sprintf("%s: exchange entry %s has no self-custody %s line", e.ID, mv.XID, e.CoinOut)] = true
				}
			} else {
				notes[fmt.Sprintf("%s: deposit %s not in the staged exchange history", e.ID, e.Ref)] = true
			}
			fq, fc := sum(pl(aOut).take(e.CoinOut, fOut, false))
			if inBooks && (fq != 0 || fc != 0) {
				desc = "Network fee: " + e.WalletOut + " " + e.CoinOut + " send to " + strings.SplitN(e.Ref, ":", 2)[0]
				feeLines(fq, fc)
			}
		case "transfer":
			ps := pl(aOut).take(e.CoinOut, qOut, false)
			q, c := sum(ps)
			fq, fc := sum(pl(aOut).take(e.CoinOut, fOut, false))
			for _, p := range fitPieces(ps, qIn) {
				pl(aIn).add(e.CoinIn, &lot{qty: p.qty, cost: p.cost, acquired: p.acquired, inBooks: inBooks})
			}
			r.Cost = c
			if inBooks {
				desc = fmt.Sprintf("Transfer %s %s -> %s", e.CoinOut, e.WalletOut, e.WalletIn)
				lines = append(lines, Line{Account: aOut, Amount: -c, Currency: USD, Quantity: qptr(-q), Commodity: e.CoinOut, CategorySource: "wallets:transfer"},
					Line{Account: aIn, Amount: c, Currency: USD, Quantity: qptr(qIn), Commodity: e.CoinIn, CategorySource: "wallets:transfer"})
				feeLines(fq, fc)
			} else {
				r.Fee = fc
			}
		case "swap":
			ps := pl(aOut).take(e.CoinOut, qOut, false)
			for _, p := range ps {
				if p.unknown {
					notes[fmt.Sprintf("%s: %s %s swapped with no wallet lot (basis 0)", e.ID, money.Format(p.qty, CryptoScale), e.CoinOut)] = true
				}
			}
			q, c := sum(ps)
			fq, fc := sum(pl(aOut).take(e.CoinOut, fOut, false))
			v := usdOut
			if v == 0 {
				v = usdIn
			}
			st, lt := gains(ps, v, e.Date)
			pl(aIn).add(e.CoinIn, &lot{qty: qIn, cost: v, acquired: e.Date, inBooks: inBooks})
			r.Cost, r.Proceeds, r.ShortTerm, r.LongTerm = c, v, st, lt
			if inBooks {
				desc = fmt.Sprintf("Swap %s -> %s (%s; estimate, %s confidence)", e.CoinOut, e.CoinIn, e.WalletOut, e.Confidence)
				lines = append(lines, Line{Account: aOut, Amount: -c, Currency: USD, Quantity: qptr(-q), Commodity: e.CoinOut, CategorySource: "wallets:swap", Memo: e.Evidence},
					Line{Account: aIn, Amount: v, Currency: USD, Quantity: qptr(qIn), Commodity: e.CoinIn, CategorySource: "wallets:swap", Memo: e.Memo})
				lines = append(lines, gainLines(st, lt, "swap")...)
				for i := range lines {
					if lines[i].CategorySource == "exchange:trade" {
						lines[i].CategorySource = "wallets:swap"
						lines[i].Memo = "estimated (daily price; matched by timing and amount)"
					}
				}
				feeLines(fq, fc)
			} else {
				r.Fee = fc
			}
		case "roundtrip_out": // to an outside address that sends it back later (ref pairs them)
			ps := pl(aOut).take(e.CoinOut, qOut+fOut, false)
			q, c := sum(ps)
			pending[e.Ref] = ps
			r.Cost = c
			r.Note = "round trip out: cost carried to the return"
			if inBooks {
				desc = "Self-custody " + e.CoinOut + " sent out (" + e.WalletOut + "), returned later (round trip, at cost)"
				lines = append(lines, Line{Account: aOut, Amount: -c, Currency: USD, Quantity: qptr(-q), Commodity: e.CoinOut, CategorySource: "wallets:roundtrip", Memo: e.Memo},
					Line{Account: self, Amount: c, Currency: USD, CategorySource: "wallets:roundtrip"})
			}
		case "roundtrip_in":
			ps, ok := pending[e.Ref]
			if !ok {
				return nil, fmt.Errorf("%s: round trip %s has no outgoing side", e.ID, e.Ref)
			}
			delete(pending, e.Ref)
			ps = fitPieces(ps, qIn)
			_, c := sum(ps)
			for _, p := range ps {
				pl(aIn).add(e.CoinIn, &lot{qty: p.qty, cost: p.cost, acquired: firstNonEmptyS(p.acquired, e.Date), inBooks: inBooks})
			}
			r.Cost = c
			r.Note = "round trip back: cost carried"
			if inBooks {
				desc = "Self-custody " + e.CoinIn + " back from round trip (" + e.WalletIn + ", at cost)"
				lines = append(lines, Line{Account: aIn, Amount: c, Currency: USD, Quantity: qptr(qIn), Commodity: e.CoinIn, CategorySource: "wallets:roundtrip", Memo: e.Memo},
					Line{Account: self, Amount: -c, Currency: USD, CategorySource: "wallets:roundtrip"})
			}
		case "unmatched_in":
			pl(aIn).add(e.CoinIn, &lot{qty: qIn, cost: usdIn, acquired: e.Date, inBooks: inBooks})
			r.Cost = usdIn
			r.Note = "unmatched inflow: cost = market value on receipt (estimate)"
			if inBooks {
				desc = "Unmatched wallet inflow (" + e.WalletIn + " " + e.CoinIn + "), at market value; carried by the at-cost adjustment"
				lines = append(lines, Line{Account: aIn, Amount: usdIn, Currency: USD, Quantity: qptr(qIn), Commodity: e.CoinIn, CategorySource: "wallets:unmatched", Memo: e.Memo},
					Line{Account: self, Amount: -usdIn, Currency: USD, CategorySource: "wallets:unmatched"})
			}
		case "unmatched_out":
			ps := pl(aOut).take(e.CoinOut, qOut+fOut, false)
			q, c := sum(ps)
			r.Cost = c
			r.Note = "unmatched outflow: leaves at FIFO cost (not treated as a disposal)"
			if inBooks {
				desc = "Unmatched wallet outflow (" + e.WalletOut + " " + e.CoinOut + "), at cost; carried by the at-cost adjustment"
				lines = append(lines, Line{Account: aOut, Amount: -c, Currency: USD, Quantity: qptr(-q), Commodity: e.CoinOut, CategorySource: "wallets:unmatched", Memo: e.Memo},
					Line{Account: self, Amount: c, Currency: USD, CategorySource: "wallets:unmatched"})
			}
		default:
			return nil, fmt.Errorf("%s: unknown kind %q", e.ID, e.Kind)
		}
		res.WalletResults = append(res.WalletResults, r)
		if inBooks && len(lines) > 0 {
			lines = compact(lines)
			if len(lines) == 1 {
				lines = append(lines, Line{Account: self, Amount: 0, Currency: USD, CategorySource: lines[0].CategorySource})
			}
			if len(lines) >= 2 {
				typ := "normal"
				if e.Kind == "transfer" {
					typ = "transfer"
				}
				entries = append(entries, Entry{Date: e.Date, Description: desc, Counterparty: e.WalletOut + e.WalletIn, Type: typ, Source: "connector", ExternalID: "wallet:" + e.ID, Lines: lines})
			}
		}
	}
	if err := flushSnaps("9999-12-31"); err != nil {
		return nil, err
	}
	open()

	// ---- mark to market at the newest pull + assertions ----
	if n := len(snapDates); n > 0 {
		D := snapDates[n-1]
		px := prices[D]
		book := map[string]map[string][2]int64{} // account -> commodity -> {qty, cost}
		all := append(append([]Entry(nil), res.Entries...), entries...)
		for _, e := range all {
			if e.Date > D {
				continue
			}
			for _, l := range e.Lines {
				if l.Commodity == "" {
					continue
				}
				if _, ok := snapBal[D][l.Account]; !ok {
					continue
				}
				if book[l.Account] == nil {
					book[l.Account] = map[string][2]int64{}
				}
				x := book[l.Account][l.Commodity]
				if l.Quantity != nil {
					x[0] += *l.Quantity
				}
				x[1] += l.Amount
				book[l.Account][l.Commodity] = x
			}
		}
		var accts []string
		for a := range snapBal[D] {
			accts = append(accts, a)
		}
		sort.Strings(accts)
		var mtm []Line
		var mtmTotal int64
		usedPx := map[string]bool{}
		for _, a := range accts {
			var expected int64
			var coms []string
			for c := range snapBal[D][a] {
				coms = append(coms, c)
			}
			for c := range book[a] {
				if _, ok := snapBal[D][a][c]; !ok {
					coms = append(coms, c)
				}
			}
			sort.Strings(coms)
			for _, c := range coms {
				p, ok := px[c]
				q := book[a][c][0]
				mv := int64(0)
				if q != 0 {
					if !ok {
						notes[fmt.Sprintf("no USD price for %s at %s; left at cost", c, D)] = true
						continue
					}
					mv, _ = valueCents(q, CryptoScale, p.Price)
					usedPx[c] = true
				}
				if d := mv - book[a][c][1]; d != 0 {
					up := ""
					if ok {
						up = p.Price
					}
					mtm = append(mtm, Line{Account: a, Amount: d, Currency: USD, Quantity: qptr(0), Commodity: c, UnitPrice: up, CategorySource: "valuation", Memo: "mark to market"})
					mtmTotal += d
				}
				if sq := snapBal[D][a][c]; sq != 0 && ok {
					v, _ := valueCents(sq, CryptoScale, p.Price)
					expected += v
				}
			}
			res.Assertions = append(res.Assertions, Assertion{Account: a, AsOf: D, Amount: expected, Source: WalletSource})
		}
		var pcs []string
		for c := range usedPx {
			pcs = append(pcs, c)
		}
		sort.Strings(pcs)
		for _, c := range pcs {
			res.Prices = append(res.Prices, Price{Commodity: c, Date: D, Price: px[c].Price, Source: "wallets-" + px[c].Source})
		}
		if len(mtm) > 0 {
			mtm = append(mtm, Line{Account: UnrealizedAcc, Amount: -mtmTotal, Currency: USD, CategorySource: "valuation"})
			entries = append(entries, Entry{Date: D, Description: "Mark to market (self-custody wallets, " + D + " prices)", Type: "valuation",
				Source: "valuation", ExternalID: "valuation:wallets:" + D, Lines: mtm})
		}
	}
	res.Entries = append(res.Entries, entries...)
	for n := range notes {
		res.Notes = append(res.Notes, "wallets: "+n)
	}
	sort.Strings(res.Notes)
	return deposit, nil
}
