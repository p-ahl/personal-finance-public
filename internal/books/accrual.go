package books

// Accrual layer: turns cash-basis connector entries into accrual-basis books, driven by a local
// YAML file (accruals.yaml, data-side; never committed). Five building blocks:
//
//   payable   one recognition entry (Dr P&L / Cr balance account) dated at the service date;
//             the later payment rows ("settlements") are re-targeted from their P&L category
//             to the balance account, so the payment clears the payable.
//   allocate  spread an amount evenly per month over [start, end] (prepaids, tax-year accruals,
//             property tax). Months before books_start roll into one opening entry; months after
//             the as-of date are not recognized yet. Settlements are re-targeted as above.
//   interest  simple monthly interest on a fixed principal (private loan): Dr expense /
//             Cr accrued interest at each month end, first month pro-rated actual/365.
//   loan      split each matched loan payment into interest and principal by rolling back from
//             the connector balance at a fixed rate and P&I payment.
//   journal   a fully specified non-cash entry (e.g. land bought with seller-side loan funds).
//
// Connector rows never change amount or account: only the counter-leg is re-targeted or split,
// so every connector account still ties to its connector balance.

import (
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/p-ahl/personal-finance/internal/money"
)

const AccrualSource = "accrual"

type DocumentSpec struct {
	ID           string `yaml:"id"`
	Kind         string `yaml:"kind"`   // bill|eob|statement|receipt|invoice|paystub|k1|tax_form|email|contract|other
	Source       string `yaml:"source"` // gmail|fastmail|drive|manual
	ExternalRef  string `yaml:"external_ref"`
	DocDate      string `yaml:"doc_date"`
	Counterparty string `yaml:"counterparty"`
	Amount       string `yaml:"amount"`
	ServiceStart string `yaml:"service_start"`
	ServiceEnd   string `yaml:"service_end"`
	Title        string `yaml:"title"`
	LocalPath    string `yaml:"local_path"`
	Notes        string `yaml:"notes"`
}

// TxnSelector picks connector rows by raw id or by (account, date, amount, regex).
type TxnSelector struct {
	ID      string `yaml:"id"`
	Account string `yaml:"account"` // ledger account of the connector row (name or key)
	Date    string `yaml:"date"`
	From    string `yaml:"from"`
	To      string `yaml:"to"`
	Amount  string `yaml:"amount"` // signed, as booked on that account (debit-positive)
	Match   string `yaml:"match"`  // regex over "name | merchant | description"
	All     bool   `yaml:"all"`    // allow several matches (default: exactly one)
	// Optional: zero matches is not an error (a standing settlement for payments that have not
	// happened yet, e.g. the remaining installments of a contracted fee).
	Optional bool `yaml:"optional"`
	re       *regexp.Regexp
}

type SplitSpec struct {
	Account string `yaml:"account"`
	Amount  string `yaml:"amount"` // positive = same direction as the re-targeted leg
	Memo    string `yaml:"memo"`
}

type SettlementSpec struct {
	Txn    TxnSelector `yaml:"txn"`
	Splits []SplitSpec `yaml:"splits"` // carved out first; the remainder goes to the balance account
	Memo   string      `yaml:"memo"`
}

type JournalLineSpec struct {
	Account string `yaml:"account"`
	Amount  string `yaml:"amount"`
	Memo    string `yaml:"memo"`
}

type AccrualSpec struct {
	ID           string `yaml:"id"`
	Kind         string `yaml:"kind"` // payable|allocate|interest|loan|journal
	Description  string `yaml:"description"`
	Counterparty string `yaml:"counterparty"`
	Account      string `yaml:"account"` // P&L account recognized (expense +, income -)
	Balance      string `yaml:"balance"` // payable / accrued / prepaid / receivable account
	Amount       string `yaml:"amount"`  // total; payable defaults to the settlements' sum
	Date         string `yaml:"date"`    // payable recognition date / journal date
	ServiceStart string `yaml:"service_start"`
	ServiceEnd   string `yaml:"service_end"`
	Start        string `yaml:"start"`         // allocate / interest period start
	End          string `yaml:"end"`           // allocate period end (interest: open-ended)
	PriorSettled string `yaml:"prior_settled"` // paid before books_start (outside the data)
	RatePct      string `yaml:"rate_pct"`      // interest / loan
	Principal    string `yaml:"principal"`     // interest
	PIPayment    string `yaml:"pi_payment"`    // loan
	// loan: lender-held escrow. Payments dated on/after EscrowFrom carry escrow = payment - pi_payment,
	// booked to Escrow (principal = pi_payment - interest). EscrowDisbursements pick rows on the loan
	// account that the lender paid out of escrow (county tax, insurance, escrow interest); their loan
	// leg is offset and moved to Escrow, so the loan line keeps only principal.
	Escrow              string            `yaml:"escrow"`
	EscrowFrom          string            `yaml:"escrow_from"`
	EscrowDisbursements []TxnSelector     `yaml:"escrow_disbursements"`
	Settlements         []SettlementSpec  `yaml:"settlements"`
	Lines               []JournalLineSpec `yaml:"lines"` // journal
	Documents           []string          `yaml:"documents"`
	Dimensions          []string          `yaml:"dimensions"` // kind:code tags on the recognized (P&L / asset) line(s)
	Basis               string            `yaml:"basis"`      // evidence | inferred | estimate (free text tag)
	Notes               string            `yaml:"notes"`
}

type AccrualConfig struct {
	Documents []DocumentSpec `yaml:"documents"`
	Accruals  []AccrualSpec  `yaml:"accruals"`
}

// Result-side records.
type Document struct {
	DocumentSpec
	AmountMinor *int64
}

type AccrualRecord struct {
	ID, Kind, Description, Counterparty, Account, Balance string
	ServiceStart, ServiceEnd, Basis, Notes                string
	AmountMinor                                           int64
}

type AccrualLink struct {
	AccrualID, ExternalID, Role string // role: accrual | settlement | opening | journal
}

type EntryDoc struct{ ExternalID, DocumentID, Role string }

func (c *AccrualConfig) compile() error {
	seen := map[string]bool{}
	docs := map[string]bool{}
	for _, d := range c.Documents {
		if d.ID == "" || docs[d.ID] {
			return fmt.Errorf("accruals: document id %q missing or duplicate", d.ID)
		}
		docs[d.ID] = true
	}
	for i := range c.Accruals {
		a := &c.Accruals[i]
		if a.ID == "" || seen[a.ID] {
			return fmt.Errorf("accruals: id %q missing or duplicate", a.ID)
		}
		seen[a.ID] = true
		for _, d := range a.Documents {
			if !docs[d] {
				return fmt.Errorf("accrual %s: unknown document %q", a.ID, d)
			}
		}
		for _, d := range a.Dimensions {
			if k, c, ok := strings.Cut(d, ":"); !ok || k == "" || c == "" {
				return fmt.Errorf("accrual %s: dimension %q must be kind:code", a.ID, d)
			}
		}
		switch a.Kind {
		case "payable", "allocate":
			if a.Account == "" || a.Balance == "" {
				return fmt.Errorf("accrual %s: account and balance required", a.ID)
			}
		case "interest":
			if a.Account == "" || a.Balance == "" || a.Principal == "" || a.RatePct == "" || a.Start == "" {
				return fmt.Errorf("accrual %s: interest needs account, balance, principal, rate_pct, start", a.ID)
			}
		case "loan":
			if a.Account == "" || a.RatePct == "" || a.PIPayment == "" {
				return fmt.Errorf("accrual %s: loan needs account (interest expense), rate_pct, pi_payment", a.ID)
			}
			if (a.EscrowFrom != "" || len(a.EscrowDisbursements) > 0) && a.Escrow == "" {
				return fmt.Errorf("accrual %s: escrow_from / escrow_disbursements need escrow (account)", a.ID)
			}
			for j := range a.EscrowDisbursements {
				s := &a.EscrowDisbursements[j]
				if s.Match != "" {
					re, err := regexp.Compile(s.Match)
					if err != nil {
						return fmt.Errorf("accrual %s: %w", a.ID, err)
					}
					s.re = re
				}
				if s.ID == "" && s.Account == "" {
					return fmt.Errorf("accrual %s escrow disbursement %d: id or account required", a.ID, j+1)
				}
			}
		case "journal":
			if a.Date == "" || len(a.Lines) < 2 {
				return fmt.Errorf("accrual %s: journal needs date and >= 2 lines", a.ID)
			}
		default:
			return fmt.Errorf("accrual %s: unknown kind %q", a.ID, a.Kind)
		}
		if a.Kind == "allocate" && (a.Start == "" || a.End == "" || a.Amount == "") {
			return fmt.Errorf("accrual %s: allocate needs start, end, amount", a.ID)
		}
		for j := range a.Settlements {
			s := &a.Settlements[j].Txn
			if s.Match != "" {
				re, err := regexp.Compile(s.Match)
				if err != nil {
					return fmt.Errorf("accrual %s: %w", a.ID, err)
				}
				s.re = re
			}
			if s.ID == "" && s.Account == "" {
				return fmt.Errorf("accrual %s settlement %d: id or account required", a.ID, j+1)
			}
		}
	}
	return nil
}

func parseAmt(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return money.ParseMinor(s, 2)
}

func monthEnd(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC)
}

const day = "2006-01-02"

// applyAccruals mutates res.Entries (re-targets/splits settlement legs) and appends accrual
// entries. Called by Build after categorization and before connector openings are derived.
func applyAccruals(res *Result, cfg *Config, rows []*row, byKey map[string]string, connBal map[string]int64) error {
	ac := cfg.Accruals
	if ac == nil {
		return nil
	}
	booksStart := cfg.Map.BooksStart
	openDate := booksStart // pre-books recognition lands on the opening date
	for _, d := range ac.Documents {
		doc := Document{DocumentSpec: d}
		if d.Amount != "" {
			v, err := parseAmt(d.Amount)
			if err != nil {
				return fmt.Errorf("document %s: %w", d.ID, err)
			}
			doc.AmountMinor = &v
		}
		res.Documents = append(res.Documents, doc)
	}
	// index: raw txn id -> entry index
	byRaw := map[string]int{}
	for i, e := range res.Entries {
		for _, l := range e.Lines {
			if l.RawRef != "" {
				byRaw[l.RawRef] = i
			}
		}
	}
	claimed := map[string]string{}
	resolveAcct := func(n string) string {
		if v, ok := byKey[n]; ok {
			return v
		}
		return n
	}
	sel := func(a *AccrualSpec, s *TxnSelector) ([]*row, error) {
		var out []*row
		amt, err := parseAmt(s.Amount)
		if err != nil {
			return nil, err
		}
		acct := resolveAcct(s.Account)
		for _, r := range rows {
			if s.ID != "" && r.t.ID != s.ID {
				continue
			}
			if acct != "" && r.acct.Name != acct {
				continue
			}
			if s.Date != "" && r.t.Date != s.Date {
				continue
			}
			if s.From != "" && r.t.Date < s.From || s.To != "" && r.t.Date > s.To {
				continue
			}
			if s.Amount != "" && r.amt != amt {
				continue
			}
			if s.re != nil && !s.re.MatchString(r.t.Name+" | "+r.t.Merchant+" | "+r.t.Description) {
				continue
			}
			out = append(out, r)
		}
		if len(out) == 0 && s.Optional {
			return nil, nil
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("accrual %s: settlement selector matched no connector row (%+v)", a.ID, *s)
		}
		if len(out) > 1 && !s.All {
			return nil, fmt.Errorf("accrual %s: settlement selector matched %d rows; narrow it or set all: true (%+v)", a.ID, len(out), *s)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].t.Date != out[j].t.Date {
				return out[i].t.Date < out[j].t.Date
			}
			return out[i].t.ID < out[j].t.ID
		})
		return out, nil
	}
	link := func(id string, e *Entry, role string) {
		res.AccrualLinks = append(res.AccrualLinks, AccrualLink{AccrualID: id, ExternalID: e.ExternalID, Role: role})
	}
	docLinks := func(a *AccrualSpec, xid, role string) {
		for _, d := range a.Documents {
			res.EntryDocs = append(res.EntryDocs, EntryDoc{ExternalID: xid, DocumentID: d, Role: role})
		}
	}
	// retarget the counter-leg of a connector row's entry to the balance account (with splits)
	retarget := func(a *AccrualSpec, s *SettlementSpec, r *row) (int64, error) {
		if prev, ok := claimed[r.t.ID]; ok {
			return 0, fmt.Errorf("accrual %s: txn %s already settles %s", a.ID, r.t.ID, prev)
		}
		claimed[r.t.ID] = a.ID
		ei, ok := byRaw[r.t.ID]
		if !ok {
			return 0, fmt.Errorf("accrual %s: txn %s has no entry", a.ID, r.t.ID)
		}
		e := &res.Entries[ei]
		var other []int
		for i, l := range e.Lines {
			if l.RawRef == "" {
				other = append(other, i)
			}
		}
		if len(other) != 1 || e.Type == "transfer" {
			return 0, fmt.Errorf("accrual %s: txn %s is a transfer or multi-leg entry (%s); only categorized rows can settle", a.ID, r.t.ID, e.ExternalID)
		}
		base := e.Lines[other[0]]
		sign := int64(1)
		if base.Amount < 0 {
			sign = -1
		}
		rest := base.Amount
		var newLines []Line
		for _, sp := range s.Splits {
			v, err := parseAmt(sp.Amount)
			if err != nil {
				return 0, err
			}
			l := base
			l.Account, l.Amount, l.Memo, l.CategorySource = sp.Account, sign*v, sp.Memo, "accrual:"+a.ID+":split"
			rest -= sign * v
			newLines = append(newLines, l)
		}
		if rest*sign < 0 {
			return 0, fmt.Errorf("accrual %s: splits exceed txn %s amount", a.ID, r.t.ID)
		}
		base.Account, base.Amount, base.CategorySource = a.Balance, rest, "accrual:"+a.ID
		if s.Memo != "" {
			base.Memo = s.Memo
		}
		if a.Counterparty != "" {
			base.Counterparty = a.Counterparty
		}
		lines := append([]Line{}, e.Lines[:other[0]]...)
		lines = append(lines, base)
		lines = append(lines, newLines...)
		lines = append(lines, e.Lines[other[0]+1:]...)
		e.Lines = lines
		if e.LinkID == "" {
			e.LinkID = "accrual:" + a.ID
		}
		link(a.ID, e, "settlement")
		docLinks(a, e.ExternalID, "settlement")
		return rest, nil
	}
	emit := func(a *AccrualSpec, xid, date, desc, typ, role string, lines []Line) {
		if len(a.Dimensions) > 0 {
			for i := range lines {
				if lines[i].Account != OpeningEquity && (a.Kind == "journal" || lines[i].Account == a.Account) {
					lines[i].Dimensions = a.Dimensions
				}
			}
		}
		e := Entry{Date: date, Description: desc, Counterparty: a.Counterparty, Type: typ, Source: AccrualSource,
			ExternalID: xid, LinkID: "accrual:" + a.ID, ServiceStart: a.ServiceStart, ServiceEnd: a.ServiceEnd, Lines: lines}
		res.Entries = append(res.Entries, e)
		link(a.ID, &e, role)
		docLinks(a, xid, role)
	}
	line := func(acct string, amt int64, cs, cp string) Line {
		return Line{Account: acct, Amount: amt, Currency: USD, CategorySource: cs, Counterparty: cp}
	}

	// Loans run last: escrow disbursement rows may also settle a payable/allocate (e.g. county tax
	// settling a property-tax accrual), which re-targets their single counter-leg first.
	order := make([]int, 0, len(ac.Accruals))
	for pass := 0; pass < 2; pass++ {
		for i := range ac.Accruals {
			if (ac.Accruals[i].Kind == "loan") == (pass == 1) {
				order = append(order, i)
			}
		}
	}
	for _, i := range order {
		a := &ac.Accruals[i]
		cs := "accrual:" + a.ID
		rec := AccrualRecord{ID: a.ID, Kind: a.Kind, Description: a.Description, Counterparty: a.Counterparty, Account: a.Account,
			Balance: a.Balance, ServiceStart: a.ServiceStart, ServiceEnd: a.ServiceEnd, Basis: a.Basis, Notes: a.Notes}
		// settlements (payable / allocate)
		var settled int64
		if a.Kind == "payable" || a.Kind == "allocate" {
			for j := range a.Settlements {
				s := &a.Settlements[j]
				rs, err := sel(a, &s.Txn)
				if err != nil {
					return err
				}
				for _, r := range rs {
					v, err := retarget(a, s, r)
					if err != nil {
						return err
					}
					settled += v
				}
			}
		}
		prior, err := parseAmt(a.PriorSettled)
		if err != nil {
			return fmt.Errorf("accrual %s: %w", a.ID, err)
		}
		switch a.Kind {
		case "payable":
			amt, err := parseAmt(a.Amount)
			if err != nil {
				return fmt.Errorf("accrual %s: %w", a.ID, err)
			}
			if a.Amount == "" {
				amt = settled + prior
			}
			date := a.Date
			if date == "" {
				date = a.ServiceEnd
			}
			if date == "" {
				return fmt.Errorf("accrual %s: payable needs date or service_end", a.ID)
			}
			rec.AmountMinor = amt
			p := line(a.Account, amt, cs, a.Counterparty)
			typ, role := "adjustment", "accrual"
			if date < booksStart {
				p.Account, typ, role, date = OpeningEquity, "opening", "opening", openDate
			}
			emit(a, "accrual:"+a.ID, date, a.Description, typ, role, []Line{p, line(a.Balance, -amt+prior, cs, a.Counterparty)})
			if prior != 0 {
				res.Entries[len(res.Entries)-1].Lines = append(res.Entries[len(res.Entries)-1].Lines, line(OpeningEquity, -prior, cs, ""))
			}
		case "allocate":
			total, err := parseAmt(a.Amount)
			if err != nil {
				return fmt.Errorf("accrual %s: %w", a.ID, err)
			}
			rec.AmountMinor = total
			st, err1 := time.Parse(day, a.Start)
			en, err2 := time.Parse(day, a.End)
			if err1 != nil || err2 != nil || en.Before(st) {
				return fmt.Errorf("accrual %s: bad start/end", a.ID)
			}
			var months []time.Time
			for m := time.Date(st.Year(), st.Month(), 1, 0, 0, 0, 0, time.UTC); !m.After(en); m = m.AddDate(0, 1, 0) {
				d := monthEnd(m)
				if d.After(en) {
					d = en
				}
				months = append(months, d)
			}
			n := int64(len(months))
			per := total / n
			var pre int64
			for k, d := range months {
				amt := per
				if k == len(months)-1 {
					amt = total - per*(n-1)
				}
				ds := d.Format(day)
				if ds < booksStart {
					pre += amt
					continue
				}
				if ds > res.AsOf {
					break
				}
				emit(a, fmt.Sprintf("accrual:%s:%s", a.ID, ds[:7]), ds, a.Description+" ("+ds[:7]+")", "adjustment", "accrual",
					[]Line{line(a.Account, amt, cs, a.Counterparty), line(a.Balance, -amt, cs, a.Counterparty)})
			}
			if pre != 0 || prior != 0 {
				emit(a, "accrual:"+a.ID+":opening", openDate, a.Description+" (recognized before books start, net of prior payments)", "opening", "opening",
					[]Line{line(OpeningEquity, pre-prior, cs, ""), line(a.Balance, prior-pre, cs, a.Counterparty)})
			}
		case "interest":
			p, err := parseAmt(a.Principal)
			if err != nil {
				return err
			}
			rate, ok := new(big.Rat).SetString(a.RatePct)
			if !ok {
				return fmt.Errorf("accrual %s: bad rate_pct", a.ID)
			}
			rate.Quo(rate, big.NewRat(100, 1))
			st, err := time.Parse(day, a.Start)
			if err != nil {
				return err
			}
			end := res.AsOf
			if a.End != "" && a.End < end {
				end = a.End
			}
			rec.AmountMinor = p
			first := true
			for m := monthEnd(st); m.Format(day) <= end; m = monthEnd(m.AddDate(0, 0, 1)) {
				var r *big.Rat
				if first && st.Day() != 1 {
					days := int64(m.Sub(st).Hours()/24) + 1
					r = new(big.Rat).Mul(new(big.Rat).SetInt64(p), rate)
					r.Mul(r, big.NewRat(days, 365))
				} else {
					r = new(big.Rat).Mul(new(big.Rat).SetInt64(p), rate)
					r.Quo(r, big.NewRat(12, 1))
				}
				first = false
				amt := roundRat(r)
				ds := m.Format(day)
				emit(a, fmt.Sprintf("accrual:%s:%s", a.ID, ds[:7]), ds, a.Description+" ("+ds[:7]+")", "adjustment", "accrual",
					[]Line{line(a.Account, amt, cs, a.Counterparty), line(a.Balance, -amt, cs, a.Counterparty)})
			}
		case "loan":
			if err := loanSplit(res, a, rows, byRaw, connBal, resolveAcct, sel, claimed); err != nil {
				return err
			}
		case "journal":
			var lines []Line
			var sum int64
			for _, jl := range a.Lines {
				v, err := parseAmt(jl.Amount)
				if err != nil {
					return fmt.Errorf("accrual %s: %w", a.ID, err)
				}
				l := line(jl.Account, v, cs, a.Counterparty)
				l.Memo = jl.Memo
				lines = append(lines, l)
				sum += v
				if v > 0 {
					rec.AmountMinor += v
				}
			}
			if sum != 0 {
				return fmt.Errorf("accrual %s: journal lines do not balance (%d)", a.ID, sum)
			}
			typ, role := "adjustment", "journal"
			if a.Date < booksStart {
				typ = "opening"
			}
			emit(a, "accrual:"+a.ID, a.Date, a.Description, typ, role, lines)
		}
		res.Accruals = append(res.Accruals, rec)
	}
	return nil
}

func roundRat(r *big.Rat) int64 {
	// half away from zero to integer cents
	num, den := new(big.Int).Set(r.Num()), new(big.Int).Set(r.Denom())
	neg := num.Sign() < 0
	num.Abs(num)
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Mul(m, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	v := q.Int64()
	if neg {
		v = -v
	}
	return v
}

// loanSplit: for each matched payment on a loan account, carve the interest portion out of the
// loan leg (the connector row itself stays intact in amount; its loan line is reduced and an
// interest-expense line added to the same entry). Balances are rolled back from the connector
// balance: B_before = (B_after + PI) / (1 + r/12); interest = B_before * r/12.
func loanSplit(res *Result, a *AccrualSpec, rows []*row, byRaw map[string]int, connBal map[string]int64,
	resolveAcct func(string) string, sel func(*AccrualSpec, *TxnSelector) ([]*row, error), claimed map[string]string) error {
	if len(a.Settlements) != 1 {
		return fmt.Errorf("accrual %s: loan needs exactly one settlement selector (the payments)", a.ID)
	}
	rs, err := sel(a, &a.Settlements[0].Txn)
	if err != nil {
		return err
	}
	loanAcct := resolveAcct(a.Settlements[0].Txn.Account)
	bal, ok := connBal[loanAcct]
	if !ok {
		return fmt.Errorf("accrual %s: %s has no connector balance", a.ID, loanAcct)
	}
	pi, err := parseAmt(a.PIPayment)
	if err != nil {
		return err
	}
	rate, ok := new(big.Rat).SetString(a.RatePct)
	if !ok {
		return fmt.Errorf("accrual %s: bad rate_pct", a.ID)
	}
	mr := new(big.Rat).Quo(rate, big.NewRat(1200, 1)) // monthly rate
	b := new(big.Rat).SetInt64(-bal)                  // amount owed (positive)
	onePlus := new(big.Rat).Add(big.NewRat(1, 1), mr)
	interest := make([]int64, len(rs))
	for k := len(rs) - 1; k >= 0; k-- {
		before := new(big.Rat).Add(b, new(big.Rat).SetInt64(pi))
		before.Quo(before, onePlus)
		interest[k] = roundRat(new(big.Rat).Mul(before, mr))
		b = before
	}
	cs := "accrual:" + a.ID
	if a.EscrowFrom != "" {
		if _, err := time.Parse(day, a.EscrowFrom); err != nil {
			return fmt.Errorf("accrual %s: bad escrow_from", a.ID)
		}
	}
	for k, r := range rs {
		if prev, ok := claimed[r.t.ID]; ok {
			return fmt.Errorf("accrual %s: txn %s already used by %s", a.ID, r.t.ID, prev)
		}
		claimed[r.t.ID] = a.ID
		ei, ok := byRaw[r.t.ID]
		if !ok {
			return fmt.Errorf("accrual %s: txn %s has no entry", a.ID, r.t.ID)
		}
		e := &res.Entries[ei]
		done := false
		for i, l := range e.Lines {
			if l.RawRef == r.t.ID && l.Account == loanAcct {
				if l.Amount < interest[k] {
					return fmt.Errorf("accrual %s: interest %d exceeds payment on %s", a.ID, interest[k], r.t.ID)
				}
				var escrow int64
				if a.Escrow != "" && a.EscrowFrom != "" && r.t.Date >= a.EscrowFrom {
					escrow = l.Amount - pi
					if escrow < 0 {
						return fmt.Errorf("accrual %s: payment %d on %s is below pi_payment %d", a.ID, l.Amount, r.t.ID, pi)
					}
				}
				e.Lines[i].Amount -= interest[k] + escrow
				e.Lines = append(e.Lines, Line{Account: a.Account, Amount: interest[k], Currency: USD, CategorySource: cs,
					Counterparty: a.Counterparty, Memo: fmt.Sprintf("interest at %s%% (derived)", a.RatePct)})
				if escrow != 0 {
					e.Lines = append(e.Lines, Line{Account: a.Escrow, Amount: escrow, Currency: USD, CategorySource: cs,
						Counterparty: a.Counterparty, Memo: fmt.Sprintf("escrow deposit = payment - P&I %s", a.PIPayment)})
				}
				done = true
				break
			}
		}
		if !done {
			return fmt.Errorf("accrual %s: entry for txn %s has no %s line", a.ID, r.t.ID, loanAcct)
		}
		res.AccrualLinks = append(res.AccrualLinks, AccrualLink{AccrualID: a.ID, ExternalID: e.ExternalID, Role: "settlement"})
		for _, d := range a.Documents {
			res.EntryDocs = append(res.EntryDocs, EntryDoc{ExternalID: e.ExternalID, DocumentID: d, Role: "settlement"})
		}
	}
	// escrow disbursements: offset the loan leg and move it to escrow (connector line untouched)
	seen := map[string]bool{}
	for j := range a.EscrowDisbursements {
		ds, err := sel(a, &a.EscrowDisbursements[j])
		if err != nil {
			return err
		}
		for _, r := range ds {
			if seen[r.t.ID] {
				return fmt.Errorf("accrual %s: escrow disbursement %s selected twice", a.ID, r.t.ID)
			}
			seen[r.t.ID] = true
			ei, ok := byRaw[r.t.ID]
			if !ok {
				return fmt.Errorf("accrual %s: txn %s has no entry", a.ID, r.t.ID)
			}
			e := &res.Entries[ei]
			var amt int64
			found := false
			for _, l := range e.Lines {
				if l.RawRef == r.t.ID && l.Account == loanAcct {
					amt, found = l.Amount, true
					break
				}
			}
			if !found {
				return fmt.Errorf("accrual %s: escrow disbursement %s has no %s line", a.ID, r.t.ID, loanAcct)
			}
			e.Lines = append(e.Lines,
				Line{Account: loanAcct, Amount: -amt, Currency: USD, CategorySource: cs, Counterparty: a.Counterparty, Memo: "paid from escrow, not the loan"},
				Line{Account: a.Escrow, Amount: amt, Currency: USD, CategorySource: cs, Counterparty: a.Counterparty, Memo: "escrow disbursement"})
			res.AccrualLinks = append(res.AccrualLinks, AccrualLink{AccrualID: a.ID, ExternalID: e.ExternalID, Role: "settlement"})
		}
	}
	return nil
}

// CompileAccruals validates specs built in code (LoadAccruals does this for YAML).
func CompileAccruals(c *AccrualConfig) error { return c.compile() }
