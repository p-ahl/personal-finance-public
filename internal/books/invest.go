package books

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/money"
)

// buildInvestments books portfolio activity and, for holdings-tracked accounts, an opening built
// from connector positions (cost basis where given) plus a mark-to-market entry so the account's
// ledger value equals the connector balance. No tax lots are invented: one aggregate "lot" per
// position records the connector's position-level cost basis.
func buildInvestments(in Input, cfg *Config, res *Result, byConn map[string]*MappedAccount, connAcct map[string]connector.Account) error {
	asOf, _ := time.Parse("2006-01-02", in.AsOf)
	valDate := asOf.AddDate(0, 0, -1).Format("2006-01-02")

	type pos struct {
		qty, book int64
	}
	positions := map[string]map[string]*pos{} // account -> ticker ("" = USD cash) -> position
	get := func(acct, tk string) *pos {
		if positions[acct] == nil {
			positions[acct] = map[string]*pos{}
		}
		if positions[acct][tk] == nil {
			positions[acct][tk] = &pos{}
		}
		return positions[acct][tk]
	}
	firstDate := map[string]string{}

	// Cash already posted to a holdings-tracked account from the bank side (e.g. contributions a
	// local rule categorizes straight to the account) is later activity too: the opening nets it out.
	holdingsName := map[string]bool{}
	for _, m := range byConn {
		if m != nil && m.Tracking == "holdings" {
			holdingsName[m.Name] = true
		}
	}
	for _, e := range res.Entries {
		for _, l := range e.Lines {
			if !holdingsName[l.Account] || l.Commodity != "" {
				continue
			}
			get(l.Account, "").book += l.Amount
			if f, ok := firstDate[l.Account]; !ok || e.Date < f {
				firstDate[l.Account] = e.Date
			}
		}
	}

	// Bank-side lines already posted to investment accounts (transfer pairs, local rules such as
	// a-brokerage-proceeds-rule). A portfolio "cash" row without a security is the brokerage
	// side of such a money movement (ACH in/out), not income: when the same amount already sits
	// on the account within a few days it is the same transfer and is not booked again.
	type bankLine struct {
		date    string
		amount  int64
		claimed bool
	}
	bankSide := map[string][]*bankLine{}
	for _, e := range res.Entries {
		for _, l := range e.Lines {
			if l.Commodity == "" && !strings.HasPrefix(l.CategorySource, "portfolio:") {
				bankSide[l.Account] = append(bankSide[l.Account], &bankLine{date: e.Date, amount: l.Amount})
			}
		}
	}
	matchBank := func(acct, date string, amt int64) bool {
		d0, _ := time.Parse("2006-01-02", date)
		for _, b := range bankSide[acct] {
			if b.claimed || b.amount != amt {
				continue
			}
			d1, _ := time.Parse("2006-01-02", b.date)
			if diff := d1.Sub(d0).Hours() / 24; diff >= -PortfolioCashMatchDays && diff <= PortfolioCashMatchDays {
				b.claimed = true
				return true
			}
		}
		return false
	}

	for _, pt := range in.PortfolioTxns {
		m := byConn[pt.AccountID]
		if m == nil {
			return fmt.Errorf("portfolio txn %s: unmapped account %s", pt.ID, pt.AccountID)
		}
		res.Stats.PortfolioRows++
		amt, err := money.ParseMinor(pt.Amount, 2)
		if err != nil {
			return fmt.Errorf("portfolio txn %s: %w", pt.ID, err)
		}
		qty, err := money.ParseMinor(orZero(pt.Quantity), SecScale)
		if err != nil {
			return fmt.Errorf("portfolio txn %s quantity: %w", pt.ID, err)
		}
		tk := strings.ToUpper(pt.Ticker)
		res.addCommodity(tk, pt.SecurityName, "")
		if f, ok := firstDate[m.Name]; !ok || pt.Date < f {
			firstDate[m.Name] = pt.Date
		}
		e := Entry{Date: pt.Date, Description: strings.TrimSpace(pt.Type + " " + tk), Counterparty: "Fidelity", Type: "normal",
			Source: "connector", ExternalID: "plaid:" + pt.ID}
		cs := "portfolio:" + pt.Type
		q := qty
		if pt.Type == "cash" && tk == "" {
			// money movement in/out of the brokerage account (no security): a transfer
			if matchBank(m.Name, pt.Date, amt) {
				res.Stats.PortfolioTransferMatched++
				res.Notes = append(res.Notes, fmt.Sprintf("portfolio cash row %s (%s %s %s) matched the transfer already booked on %s; not booked again",
					pt.ID, pt.Date, m.Name, money.Format(amt, 2), m.Name))
				continue
			}
			e.Lines = []Line{
				{Account: m.Name, Amount: amt, Currency: USD, CategorySource: cs, RawRef: pt.ID, Memo: "cash transfer"},
				{Account: cfg.Transfers.Unmatched, Amount: -amt, Currency: USD, CategorySource: "unmatched:portfolio-cash-transfer"},
			}
			get(m.Name, "").book += amt
			res.Entries = append(res.Entries, e)
			continue
		}
		switch pt.Type {
		case "buy", "sell":
			// cash leg (amount sign: buy = money out) and security leg in the same account
			e.Lines = []Line{
				{Account: m.Name, Amount: -amt, Currency: USD, Quantity: &q, Commodity: tk, UnitPrice: trimDec(pt.Price), CategorySource: cs, RawRef: pt.ID},
				{Account: m.Name, Amount: amt, Currency: USD, CategorySource: cs, Memo: "cash leg"},
			}
			p := get(m.Name, tk)
			p.qty += qty
			p.book += -amt
			get(m.Name, "").book += amt
		case "cash", "dividend", "interest":
			e.Lines = []Line{
				{Account: m.Name, Amount: amt, Currency: USD, CategorySource: cs, RawRef: pt.ID, Memo: tk},
				{Account: DividendsAcct, Amount: -amt, Currency: USD, CategorySource: cs, Counterparty: "Fidelity"},
			}
			get(m.Name, "").book += amt
		case "fee":
			e.Lines = []Line{
				{Account: m.Name, Amount: amt, Currency: USD, CategorySource: cs, RawRef: pt.ID},
				{Account: InvFeesAcct, Amount: -amt, Currency: USD, CategorySource: cs},
			}
			get(m.Name, "").book += amt
		default:
			e.Lines = []Line{
				{Account: m.Name, Amount: amt, Currency: USD, CategorySource: cs, RawRef: pt.ID},
				{Account: cfg.Transfers.Unmatched, Amount: -amt, Currency: USD, CategorySource: "unmatched:portfolio-" + pt.Type},
			}
			get(m.Name, "").book += amt
		}
		res.Entries = append(res.Entries, e)
	}

	// holdings-tracked accounts: opening from positions + mark to market
	var accts []string
	holdByAcct := map[string][]connector.Holding{}
	for _, h := range in.Holdings {
		holdByAcct[h.AccountID] = append(holdByAcct[h.AccountID], h)
	}
	for _, a := range in.Accounts {
		if m := byConn[a.ID]; m != nil && m.Tracking == "holdings" {
			accts = append(accts, a.ID)
		}
	}
	sort.Strings(accts)
	for _, id := range accts {
		m := byConn[id]
		target, err := connectorBalance(connAcct[id])
		if err != nil {
			return err
		}
		openDate := cfg.Map.BooksStart
		if f, ok := firstDate[m.Name]; ok {
			t, _ := time.Parse("2006-01-02", f)
			openDate = t.AddDate(0, 0, -1).Format("2006-01-02")
		}
		var openLines, mtmLines []Line
		var holdValue int64
		var openTotal, mtmTotal int64
		hs := holdByAcct[id]
		hs = withSweepFallback(hs, id, target, connAcct[id].AvailableBalance, in.OpeningPins["opening:"+id])
		sort.Slice(hs, func(i, j int) bool { return hs[i].Ticker < hs[j].Ticker })
		for _, h := range hs {
			tk := strings.ToUpper(h.Ticker)
			val, err := money.ParseMinor(orZero(h.Value), 2)
			if err != nil {
				return fmt.Errorf("holding %s value: %w", h.ID, err)
			}
			if strings.HasPrefix(tk, "CUR:") {
				continue // cash shows up as balance - securities
			}
			holdValue += val
			qty, err := money.ParseMinor(orZero(h.Quantity), SecScale)
			if err != nil {
				return fmt.Errorf("holding %s quantity: %w", h.ID, err)
			}
			cost := val
			if strings.TrimSpace(h.CostBasis) != "" {
				if cost, err = money.ParseMinorRound(h.CostBasis, 2); err != nil {
					return fmt.Errorf("holding %s cost: %w", h.ID, err)
				}
			}
			res.addCommodity(tk, h.SecurityName, h.SecurityType)
			res.Prices = append(res.Prices, Price{Commodity: tk, Date: valDate, Price: trimDec(h.Price), Source: "connector-holdings"})
			res.Lots = append(res.Lots, Lot{Account: m.Name, Commodity: tk, AsOf: in.AsOf, Quantity: qty, Cost: cost})
			p := get(m.Name, tk)
			oq, oc := qty-p.qty, cost-p.book
			if oq != 0 || oc != 0 {
				q := oq
				openLines = append(openLines, Line{Account: m.Name, Amount: oc, Currency: USD, Quantity: &q, Commodity: tk, CategorySource: "opening", Memo: "position at cost (connector cost basis)"})
				openTotal += oc
			}
			if d := val - cost; d != 0 {
				zero := int64(0)
				mtmLines = append(mtmLines, Line{Account: m.Name, Amount: d, Currency: USD, Quantity: &zero, Commodity: tk, UnitPrice: trimDec(h.Price), CategorySource: "valuation", Memo: "mark to market"})
				mtmTotal += d
			}
		}
		cashOpen := (target - holdValue) - get(m.Name, "").book
		if cashOpen != 0 {
			openLines = append(openLines, Line{Account: m.Name, Amount: cashOpen, Currency: USD, CategorySource: "opening", Memo: "cash"})
			openTotal += cashOpen
		}
		basis := "holdings at cost minus later activity"
		xid := "opening:" + id
		if pin, ok := in.OpeningPins[xid]; ok {
			// Pinned: the per-position opening never moves again. Whatever the connector holdings
			// and later activity no longer explain (a position or cash difference) is booked to a
			// pending account, recomputed every run, and reported - never absorbed into the opening.
			resid := map[string]*PinLine{}
			var order []string
			add := func(tk string, q, a int64) {
				r := resid[tk]
				if r == nil {
					r = &PinLine{Commodity: tk}
					resid[tk] = r
					order = append(order, tk)
				}
				r.Quantity += q
				r.Amount += a
			}
			for _, l := range openLines {
				var q int64
				if l.Quantity != nil {
					q = *l.Quantity
				}
				add(l.Commodity, q, l.Amount)
			}
			openLines, openTotal = nil, 0
			for _, pl := range pin.Lines {
				l := Line{Account: m.Name, Amount: pl.Amount, Currency: USD, Commodity: pl.Commodity, CategorySource: "opening", Memo: pl.Memo}
				if pl.Commodity != "" {
					q := pl.Quantity
					l.Quantity = &q
				}
				openLines = append(openLines, l)
				openTotal += pl.Amount
				add(pl.Commodity, -pl.Quantity, -pl.Amount)
			}
			if len(pin.Lines) == 0 && pin.Amount != 0 {
				openLines = append(openLines, Line{Account: m.Name, Amount: pin.Amount, Currency: USD, CategorySource: "opening", Memo: "cash"})
				openTotal = pin.Amount
				add("", 0, -pin.Amount)
			}
			sort.Strings(order)
			var pendLines []Line
			var gap int64
			for _, tk := range order {
				r := resid[tk]
				if r.Quantity == 0 && r.Amount == 0 {
					continue
				}
				l := Line{Account: m.Name, Amount: r.Amount, Currency: USD, Commodity: tk, CategorySource: "pending", Memo: "holdings minus pinned opening and posted activity"}
				if tk != "" {
					q := r.Quantity
					l.Quantity = &q
				}
				pendLines = append(pendLines, l)
				gap += r.Amount
			}
			if len(pendLines) > 0 {
				pend, configured := m.PendingAccount, true
				if pend == "" {
					pend, configured = DefaultPendingAcct, false
				}
				snap := in.AsOf
				if d := in.SnapshotAsOf[id]; d != "" {
					snap = d
				}
				res.Pending = append(res.Pending, PendingGap{Account: m.Name, Pending: pend, AsOf: snap, Amount: gap, Configured: configured})
				pendLines = append(pendLines, Line{Account: pend, Amount: -gap, Currency: USD, CategorySource: "pending"})
				res.Entries = append(res.Entries, Entry{Date: snap, Type: "adjustment", Source: PendingSource, ExternalID: "pending:" + id,
					Description: "Pending: " + m.Name + " holdings " + snap + " vs pinned opening and posted activity (clears when the missing activity posts)",
					Lines:       pendLines})
			}
			basis = "holdings at cost minus later activity, pinned " + pin.AsOf
		} else {
			np := OpeningPin{ExternalID: xid, Account: m.Name, AsOf: in.AsOf, Amount: openTotal}
			for _, l := range openLines {
				pl := PinLine{Commodity: l.Commodity, Amount: l.Amount, Memo: l.Memo}
				if l.Quantity != nil {
					pl.Quantity = *l.Quantity
				}
				np.Lines = append(np.Lines, pl)
			}
			res.NewPins = append(res.NewPins, np)
		}
		if len(openLines) == 0 {
			openLines = append(openLines, Line{Account: m.Name, Amount: 0, Currency: USD, CategorySource: "opening"})
		}
		openLines = append(openLines, Line{Account: OpeningEquity, Amount: -openTotal, Currency: USD, CategorySource: "opening"})
		res.Openings = append(res.Openings, Opening{Account: m.Name, Date: openDate, Amount: openTotal, Basis: basis})
		res.Entries = append(res.Entries, Entry{Date: openDate, Description: "Opening balance (holdings at cost)", Type: "opening",
			Source: "opening", ExternalID: "opening:" + id, Lines: openLines})
		if len(mtmLines) > 0 {
			mtmLines = append(mtmLines, Line{Account: UnrealizedAcc, Amount: -mtmTotal, Currency: USD, CategorySource: "valuation"})
			res.Entries = append(res.Entries, Entry{Date: valDate, Description: "Mark to market (connector holdings value)", Type: "valuation",
				Source: "valuation", ExternalID: "valuation:" + id + ":" + valDate, Lines: mtmLines})
		}
	}
	return nil
}

// PortfolioCashMatchDays is the window (days, either side) in which a security-less portfolio cash
// row matches a bank-side transfer line of the same amount on the same account.
const PortfolioCashMatchDays = 5

func (res *Result) addCommodity(tk, name, secType string) {
	if tk == "" || tk == USD {
		return
	}
	c, ok := res.Commodities[tk]
	if !ok {
		c = Commodity{Code: tk, Scale: SecScale, Kind: "fund", Name: name}
	}
	switch strings.ToLower(secType) {
	case "equity", "stock":
		c.Kind = "stock"
	case "cryptocurrency", "crypto":
		c.Kind = "crypto"
	}
	if c.Name == "" {
		c.Name = name
	}
	res.Commodities[tk] = c
}

func orZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

func trimDec(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// sweepTickers are the money-market sweep funds a brokerage reports as a $1 holding row.
var sweepTickers = map[string]bool{"SPAXX": true, "FDRXX": true, "FZFXX": true, "SPRXX": true, "FCASH": true}

// withSweepFallback keeps a pinned account reconciling when the feed stops reporting its sweep
// holding row (the Fidelity feed dropped SPAXX rows and reports that cash as available_balance
// instead; current_balance still includes it). When the opening pin carries a sweep position but
// no row for it arrives and available_balance is reported, the available balance is the sweep
// position again: a synthetic $1 holding (quantity = value = cost) instead of a pending entry that
// moves the pinned position out. It is only used when it fits inside the reported balance beyond
// the other holdings; otherwise the old behaviour (a reported pending gap) stands. Without
// available_balance nothing is synthesized. Idempotent: depends only on the extract and the pin.
func withSweepFallback(hs []connector.Holding, accountID string, target int64, available *string, pin OpeningPin) []connector.Holding {
	have := map[string]bool{}
	var held int64
	for _, h := range hs {
		tk := strings.ToUpper(h.Ticker)
		have[tk] = true
		if strings.HasPrefix(tk, "CUR:") {
			continue
		}
		if v, err := money.ParseMinor(orZero(h.Value), 2); err == nil {
			held += v
		}
	}
	for _, pl := range pin.Lines {
		tk := pl.Commodity
		if !sweepTickers[tk] || have[tk] {
			continue
		}
		if available == nil {
			return hs
		}
		v, err := money.ParseMinor(orZero(*available), 2)
		if err != nil || v <= 0 || v > target-held {
			return hs
		}
		amt := money.Format(v, 2)
		out := append([]connector.Holding(nil), hs...)
		return append(out, connector.Holding{ID: "sweep-fallback:" + accountID + ":" + tk, AccountID: accountID, Ticker: tk,
			SecurityName: tk + " (sweep, derived from reported balance)", SecurityType: "cash",
			Quantity: amt, CostBasis: amt, Price: "1", Value: amt, Currency: "USD"})
	}
	return hs
}
