package books

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/money"
)

const (
	USD           = "USD"
	OpeningEquity = "Equity:Opening-Balances"
	PendingSource = "pending" // entry source of the pending-gap entries (builder-managed)
	SecScale      = 6 // minor-unit digits for fund/stock quantities
	DividendsAcct = "Income:Investments:Dividends"
	UnrealizedAcc = "Income:Investments:Unrealized-Gains"
	InvFeesAcct   = "Expenses:Fees:Investment"
)

type Line struct {
	Account        string   `json:"a"`
	Amount         int64    `json:"m"`
	Currency       string   `json:"c"`
	Quantity       *int64   `json:"q,omitempty"`
	Commodity      string   `json:"k,omitempty"`
	UnitPrice      string   `json:"p,omitempty"`
	Counterparty   string   `json:"cp,omitempty"`
	Memo           string   `json:"memo,omitempty"`
	CategorySource string   `json:"cs,omitempty"`
	RawRef         string   `json:"r,omitempty"`
	Dimensions     []string `json:"d,omitempty"`
}

type Entry struct {
	Date         string `json:"date"`
	Description  string `json:"desc"`
	Counterparty string `json:"cp,omitempty"`
	Type         string `json:"type"`
	Source       string `json:"src"`
	ExternalID   string `json:"xid"`
	LinkID       string `json:"link,omitempty"`
	ServiceStart string `json:"ss,omitempty"`
	ServiceEnd   string `json:"se,omitempty"`
	Lines        []Line `json:"lines"`
}

// Hash is a stable content hash used for idempotent upserts.
func (e Entry) Hash() string {
	b, _ := json.Marshal(e)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:16])
}

type AccountInfo struct {
	Name        string
	Attrs       Attrs
	ExternalID  string
	Mask        string
	Institution string
}

type Assertion struct {
	Account string
	AsOf    string
	Amount  int64
	Source  string // "" = connector; otherwise the exchange name (e.g. kraken)
}

type Price struct{ Commodity, Date, Price, Source string }

type Commodity struct {
	Code  string
	Scale int
	Kind  string
	Name  string
}

type Lot struct {
	Account, Commodity, AsOf string
	Quantity, Cost           int64
}

type Pair struct {
	A, B      string // connector txn ids (A = money out side)
	Kind      string // same-day | in-transit | funding
	Hint      string
	GapDays   int
	AmountAbs int64
}

type Opening struct {
	Account string
	Date    string
	Amount  int64
	Basis   string
}

// DefaultPendingAcct receives the gap between a transaction-tracked account's reported balance
// and its pinned opening + posted activity when the account mapping names no pending_account.
const DefaultPendingAcct = "Liabilities:Pending:Unposted"

// OpeningPin is an established opening balance of a transaction-tracked connector account
// (keyed by the opening entry's external id). Once pinned, an opening is never re-derived.
type OpeningPin struct {
	ExternalID, Account, AsOf string
	Amount                    int64
	// Lines: per-position opening lines of a holdings-tracked account (empty otherwise).
	Lines []PinLine
}

// PinLine is one pinned opening line of a holdings-tracked account ("" commodity = cash).
type PinLine struct {
	Commodity string `json:"commodity,omitempty"`
	Quantity  int64  `json:"quantity,omitempty"` // security scale (SecScale)
	Amount    int64  `json:"amount"`             // cents at cost
	Memo      string `json:"memo,omitempty"`
}

// PendingGap is the difference between a pinned account's reported balance and its ledger
// balance (pinned opening + posted activity) at the snapshot date, booked to Pending.
type PendingGap struct {
	Account, Pending, AsOf string
	Amount                 int64 // reported - ledger (debit-positive, on the account)
	Configured             bool  // Pending comes from the mapping's pending_account (expected lag)
}

type Stats struct {
	PostedRows, PendingRows        int
	Pairs, FundingPairs            int
	OneSided, Unmatched            int
	ByRule, ByPlaid, Uncategorized int
	Split                          int // categorized rows whose counter-leg was itemized (splits.local.yaml)
	PortfolioRows                  int
	PortfolioTransferMatched       int // security-less portfolio cash rows matched to a booked transfer (not re-booked)
}

type Result struct {
	AsOf        string
	Entries     []Entry
	Accounts    map[string]*AccountInfo
	Assertions  []Assertion
	Prices      []Price
	Lots        []Lot
	Commodities map[string]Commodity
	Pairs       []Pair
	Openings    []Opening
	// NewPins are openings established by this build (accounts with no pin yet); the
	// pipeline stores them so later builds keep them fixed.
	NewPins []OpeningPin
	// Pending lists pinned accounts whose reported balance differs from the ledger.
	Pending []PendingGap
	Stats   Stats
	Notes       []string // non-fatal build notes (e.g. exchange rows without a USD price)
	// WalletResults: per wallet event replay outcome (cost, gains), including events before
	// the books (estimates for tax reporting; only booked ones are in the journal).
	WalletResults []WalletResult
	// CryptoValuations: daily quantity/price/value/cost basis per self-custody holding
	CryptoValuations []CryptoValuation
	// accrual layer
	Documents    []Document
	Accruals     []AccrualRecord
	AccrualLinks []AccrualLink
	EntryDocs    []EntryDoc
}

type Input struct {
	AsOf          string
	Accounts      []connector.Account
	Txns          []connector.Txn
	PortfolioTxns []connector.PortfolioTxn
	Holdings      []connector.Holding
	// SnapshotAsOf is the date of the balance snapshot used for each connector account id
	// (the newest one staged for that account). Missing entries default to AsOf.
	SnapshotAsOf map[string]string
	// Exchanges staged from exchange APIs (Kraken); see exchange.go.
	Exchanges []ExchangeData
	// Wallets staged from `finance import wallets`; see wallets.go.
	Wallets WalletData
	// OpeningPins: established openings of transaction-tracked connector accounts, keyed by
	// the opening's external id (opening:<connector account id>), in cents.
	OpeningPins map[string]OpeningPin
}

type row struct {
	t       connector.Txn
	amt     int64
	day     time.Time
	acct    *MappedAccount
	class   string
	hint    *Hint
	targets map[string]bool // account names
	used    bool
	funded  bool // paired as the payment side of a funding pair; still categorized
}

var transferPrimaries = map[string]bool{"TRANSFER_IN": true, "TRANSFER_OUT": true, "LOAN_PAYMENTS": true}

// Build derives all journal entries from staged connector data.
func Build(in Input, cfg *Config) (*Result, error) {
	if in.AsOf == "" {
		return nil, fmt.Errorf("build: as-of date required")
	}
	res := &Result{AsOf: in.AsOf, Accounts: map[string]*AccountInfo{}, Commodities: map[string]Commodity{
		USD: {Code: USD, Scale: 2, Kind: "currency", Name: "US Dollar"}}}

	byConn := map[string]*MappedAccount{}
	byKey := map[string]string{}
	for i := range cfg.Map.Accounts {
		m := &cfg.Map.Accounts[i]
		byConn[m.ConnectorID] = m
		if m.Key != "" {
			byKey[m.Key] = m.Name
		}
	}
	internal := map[string]bool{}
	for _, s := range cfg.COA.Accounts {
		if s.Key != "" {
			byKey[s.Key] = s.Name
			internal[s.Name] = true
		}
	}
	// valuation-only connector accounts have no transaction feed: flows to them are one-sided
	for _, m := range cfg.Map.Accounts {
		if m.Tracking == "valuation" {
			internal[m.Name] = true
		}
	}
	connAcct := map[string]connector.Account{}
	var unmapped []string
	for _, a := range in.Accounts {
		connAcct[a.ID] = a
		m, ok := byConn[a.ID]
		if !ok {
			unmapped = append(unmapped, fmt.Sprintf("%s (%s %s)", a.ID, a.Class, a.Name))
			continue
		}
		mask := ""
		if a.Mask != nil {
			mask = *a.Mask
		}
		if err := res.addAccount(cfg, m.Name, a.ID, mask, m.Institution); err != nil {
			return nil, err
		}
	}
	if len(unmapped) > 0 {
		return nil, fmt.Errorf("connector accounts missing from account map: %s", strings.Join(unmapped, "; "))
	}
	maskOf := func(name string) string { return res.Accounts[name].Mask }

	// ---- rows ----
	var rows []*row
	for _, t := range in.Txns {
		if t.Pending {
			res.Stats.PendingRows++
			continue
		}
		m := byConn[t.AccountID]
		if m == nil {
			return nil, fmt.Errorf("txn %s: account %s not in connector account list", t.ID, t.AccountID)
		}
		amt, err := money.ParseMinor(t.Amount, 2)
		if err != nil {
			return nil, fmt.Errorf("txn %s: %w", t.ID, err)
		}
		d, err := time.Parse("2006-01-02", t.Date)
		if err != nil {
			return nil, fmt.Errorf("txn %s: %w", t.ID, err)
		}
		rows = append(rows, &row{t: t, amt: amt, day: d, acct: m, class: connAcct[t.AccountID].Class})
		res.Stats.PostedRows++
	}

	// ---- transfer hints ----
	for _, r := range rows {
		if _, _, ru := categorize(r, cfg); ru != nil && ru.OverrideTransfer {
			continue
		}
		text := r.t.Name + " | " + r.t.Merchant + " | " + r.t.Description
		for i := range cfg.Transfers.Hints {
			h := &cfg.Transfers.Hints[i]
			if h.OnKind != "" && h.OnKind != r.t.Kind {
				continue
			}
			if h.onInstRe != nil && !h.onInstRe.MatchString(r.acct.Institution) {
				continue
			}
			sm := h.re.FindStringSubmatch(text)
			if sm == nil {
				continue
			}
			targets := map[string]bool{}
			switch {
			case h.MaskGroup > 0 && h.MaskGroup < len(sm):
				for _, m := range cfg.Map.Accounts {
					if maskOf(m.Name) == sm[h.MaskGroup] {
						targets[m.Name] = true
					}
				}
			case h.ToKey != "":
				if n, ok := byKey[h.ToKey]; ok {
					targets[n] = true
				} else {
					return nil, fmt.Errorf("hint %q: unknown key %q", h.Name, h.ToKey)
				}
			case h.toInstRe != nil:
				for _, m := range cfg.Map.Accounts {
					if h.toInstRe.MatchString(m.Institution) {
						targets[m.Name] = true
					}
				}
			case h.ToClass != "":
				for _, m := range cfg.Map.Accounts {
					if h.ToClass == "any" || connAcct[m.ConnectorID].Class == h.ToClass {
						targets[m.Name] = true
					}
				}
			}
			delete(targets, r.acct.Name)
			if len(targets) == 0 {
				continue
			}
			r.hint, r.targets = h, targets
			break
		}
	}

	// ---- pairing ----
	type cand struct {
		r, s     *row
		tier     int
		gap      int
		funding  bool
		hintName string
	}
	var cands []cand
	isCand := func(r *row) bool {
		if r.hint != nil {
			for n := range r.targets {
				if !internal[n] {
					return true
				}
			}
			return false
		}
		if !transferPrimaries[r.t.CategoryPrimary] {
			return false
		}
		// an explicit categorization rule beats the generic amount/date heuristic
		_, _, rule := categorize(r, cfg)
		return rule == nil
	}
	var pool []*row
	for _, r := range rows {
		if isCand(r) {
			pool = append(pool, r)
		}
	}
	for i, r := range pool {
		for j, s := range pool {
			if i == j || r.acct.Name == s.acct.Name || r.amt == 0 {
				continue
			}
			gap := int(s.day.Sub(r.day).Hours() / 24)
			if gap < 0 {
				gap = -gap
			}
			hinted := r.hint != nil || s.hint != nil
			window := cfg.Transfers.WindowGeneric
			if hinted {
				window = cfg.Transfers.WindowHinted
			}
			if gap > window {
				continue
			}
			name := "generic"
			if r.hint != nil {
				name = r.hint.Name
			} else if s.hint != nil {
				name = s.hint.Name
			}
			rOK := r.hint == nil || r.targets[s.acct.Name]
			sOK := s.hint == nil || s.targets[r.acct.Name]
			if r.amt == -s.amt && r.amt < 0 && rOK && sOK { // r = money-out side; each pair considered once
				tier := 2
				if r.hint != nil && s.hint != nil {
					tier = 0
				} else if hinted {
					tier = 1
				}
				cands = append(cands, cand{r: r, s: s, tier: tier, gap: gap, hintName: name})
			}
		}
	}
	// funding pairs: a bank row hinted `funding` may match a same-amount payment in the target
	// wallet regardless of that payment's category (it is a purchase, not a transfer)
	for _, r := range pool {
		if r.hint == nil || !r.hint.Funding || r.amt == 0 {
			continue
		}
		for _, s := range rows {
			if s.hint != nil || !r.targets[s.acct.Name] || s.amt != r.amt {
				continue
			}
			gap := int(s.day.Sub(r.day).Hours() / 24)
			if gap < 0 {
				gap = -gap
			}
			if gap > cfg.Transfers.WindowHinted {
				continue
			}
			cands = append(cands, cand{r: r, s: s, tier: 1, gap: gap, funding: true, hintName: r.hint.Name})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.gap != b.gap {
			return a.gap < b.gap
		}
		if a.r.t.Date != b.r.t.Date {
			return a.r.t.Date < b.r.t.Date
		}
		if a.r.t.ID != b.r.t.ID {
			return a.r.t.ID < b.r.t.ID
		}
		return a.s.t.ID < b.s.t.ID
	})
	for _, c := range cands {
		if c.r.used || c.s.used {
			continue
		}
		c.r.used, c.s.used = true, true
		abs := c.r.amt
		if abs < 0 {
			abs = -abs
		}
		if c.funding {
			c.s.funded = true
			res.Stats.FundingPairs++
			res.Pairs = append(res.Pairs, Pair{A: c.r.t.ID, B: c.s.t.ID, Kind: "funding", Hint: c.hintName, GapDays: c.gap, AmountAbs: abs})
			res.Entries = append(res.Entries, Entry{Date: c.r.t.Date, Description: c.r.t.Name, Counterparty: "self", Type: "transfer",
				Source: "connector", ExternalID: "plaid:" + c.r.t.ID, LinkID: "fund:" + c.r.t.ID + "+" + c.s.t.ID,
				Lines: []Line{
					{Account: c.r.acct.Name, Amount: c.r.amt, Currency: USD, CategorySource: "transfer:" + c.hintName, RawRef: c.r.t.ID},
					{Account: c.s.acct.Name, Amount: -c.r.amt, Currency: USD, CategorySource: "transfer:" + c.hintName, Memo: "funds payment " + c.s.t.ID},
				}})
			continue
		}
		res.Stats.Pairs++
		a, b := c.r, c.s // a = out
		ids := []string{a.t.ID, b.t.ID}
		sort.Strings(ids)
		kind := "same-day"
		if a.t.Date != b.t.Date {
			kind = "in-transit"
		}
		res.Pairs = append(res.Pairs, Pair{A: a.t.ID, B: b.t.ID, Kind: kind, Hint: c.hintName, GapDays: c.gap, AmountAbs: abs})
		cs := "transfer:" + c.hintName
		desc := a.t.Name
		if kind == "same-day" {
			res.Entries = append(res.Entries, Entry{Date: a.t.Date, Description: desc, Counterparty: "self", Type: "transfer",
				Source: "connector", ExternalID: "plaid:" + ids[0] + "+" + ids[1],
				Lines: []Line{
					{Account: a.acct.Name, Amount: a.amt, Currency: USD, CategorySource: cs, RawRef: a.t.ID},
					{Account: b.acct.Name, Amount: b.amt, Currency: USD, CategorySource: cs, RawRef: b.t.ID},
				}})
		} else {
			link := "xfer:" + ids[0] + "+" + ids[1]
			for _, x := range []*row{a, b} {
				res.Entries = append(res.Entries, Entry{Date: x.t.Date, Description: x.t.Name, Counterparty: "self", Type: "transfer",
					Source: "connector", ExternalID: "plaid:" + x.t.ID, LinkID: link,
					Lines: []Line{
						{Account: x.acct.Name, Amount: x.amt, Currency: USD, CategorySource: cs, RawRef: x.t.ID},
						{Account: cfg.Transfers.InTransit, Amount: -x.amt, Currency: USD, CategorySource: cs},
					}})
			}
		}
	}

	// ---- one-sided transfers & categorization ----
	splitUsed := map[string]bool{}
	for _, r := range rows {
		if r.used && !r.funded {
			continue
		}
		if r.hint != nil && !r.funded {
			target := ""
			if len(r.targets) == 1 {
				for n := range r.targets {
					if internal[n] {
						target = n
					}
				}
			}
			cs := "transfer:" + r.hint.Name
			if target == "" {
				target = cfg.Transfers.Unmatched
				cs = "unmatched:" + r.hint.Name
				res.Stats.Unmatched++
			} else {
				res.Stats.OneSided++
			}
			res.Entries = append(res.Entries, Entry{Date: r.t.Date, Description: r.t.Name, Counterparty: counterpartyOf(r.t), Type: "transfer",
				Source: "connector", ExternalID: "plaid:" + r.t.ID,
				Lines: []Line{
					{Account: r.acct.Name, Amount: r.amt, Currency: USD, CategorySource: cs, RawRef: r.t.ID},
					{Account: target, Amount: -r.amt, Currency: USD, CategorySource: cs},
				}})
			continue
		}
		acct, cs, rule := categorize(r, cfg)
		switch {
		case strings.HasPrefix(cs, "rule:"):
			res.Stats.ByRule++
		case strings.HasPrefix(cs, "plaid:"):
			res.Stats.ByPlaid++
		case strings.HasPrefix(cs, "unmatched"):
			res.Stats.Unmatched++
		default:
			res.Stats.Uncategorized++
		}
		cp := counterpartyOf(r.t)
		var dims []string
		if rule != nil {
			if rule.Counterparty != "" {
				cp = rule.Counterparty
			}
			dims = rule.Dimensions
		}
		dims = applyTags(cfg, r, acct, dims)
		typ := "normal"
		if acct == cfg.Transfers.Unmatched {
			typ = "transfer"
		}
		counter := Line{Account: acct, Amount: -r.amt, Currency: USD, CategorySource: cs, Counterparty: cp, Dimensions: dims}
		lines := []Line{{Account: r.acct.Name, Amount: r.amt, Currency: USD, CategorySource: cs, RawRef: r.t.ID}}
		if typ == "normal" {
			sl, err := splitLines(cfg, r, counter, splitUsed)
			if err != nil {
				return nil, err
			}
			if sl != nil {
				res.Stats.Split++
				lines = append(lines, sl...)
			}
		}
		if len(lines) == 1 {
			lines = append(lines, counter)
		}
		res.Entries = append(res.Entries, Entry{Date: r.t.Date, Description: r.t.Name, Counterparty: cp, Type: typ,
			Source: "connector", ExternalID: "plaid:" + r.t.ID, Lines: lines})
	}
	if err := unusedSplits(cfg, splitUsed); err != nil {
		return nil, err
	}

	// ---- investments ----
	if err := buildInvestments(in, cfg, res, byConn, connAcct); err != nil {
		return nil, err
	}

	// ---- exchanges (Kraken): own ledger, USD transfers matched to the bank rows above ----
	if err := buildCrypto(in, cfg, res, byKey); err != nil {
		return nil, err
	}

	// ---- accrual layer (local accruals.yaml) ----
	connBal := map[string]int64{}
	for _, a := range in.Accounts {
		v, err := connectorBalance(a)
		if err != nil {
			return nil, err
		}
		connBal[byConn[a.ID].Name] = v
	}
	if err := applyAccruals(res, cfg, rows, byKey, connBal); err != nil {
		return nil, err
	}

	// ---- opening balances & assertions for every connector account ----
	sums := map[string]int64{}
	first := map[string]string{}
	for _, e := range res.Entries {
		for _, l := range e.Lines {
			sums[l.Account] += l.Amount
			if f, ok := first[l.Account]; !ok || e.Date < f {
				first[l.Account] = e.Date
			}
		}
	}
	dayBefore := func(d string) string {
		t, _ := time.Parse("2006-01-02", d)
		return t.AddDate(0, 0, -1).Format("2006-01-02")
	}
	for _, a := range in.Accounts {
		m := byConn[a.ID]
		target, err := connectorBalance(a)
		if err != nil {
			return nil, err
		}
		snap := in.AsOf
		if d := in.SnapshotAsOf[a.ID]; d != "" {
			snap = d
		}
		res.Assertions = append(res.Assertions, Assertion{Account: m.Name, AsOf: snap, Amount: target})
		if m.Tracking == "holdings" {
			continue // opening built from holdings in buildInvestments
		}
		date := cfg.Map.BooksStart
		if f, ok := first[m.Name]; ok {
			date = dayBefore(f)
		}
		// the snapshot balance covers activity dated on or before the snapshot date only
		// (the same cut-off v_balance_assertion_check uses)
		var upTo int64
		for _, e := range res.Entries {
			if e.Date > snap {
				continue
			}
			for _, l := range e.Lines {
				if l.Account == m.Name {
					upTo += l.Amount
				}
			}
		}
		open := target - upTo
		basis := "connector balance " + snap + " minus posted activity"
		xid := "opening:" + a.ID
		if m.ValuationOnly || m.Tracking == "valuation" {
			basis = "valuation-only: connector balance " + snap + " minus known flows"
			if pin, ok := in.OpeningPins[xid]; ok {
				// Pinned like every other opening: a later full-history pull with older rows adds
				// activity, and a change in reported value that known flows do not explain is a
				// valuation change (unrealized gain/loss), recomputed every run - never opening equity.
				open = pin.Amount
				basis = "valuation-only: connector balance " + pin.AsOf + " minus known flows, pinned"
				if gap := target - upTo - pin.Amount; gap != 0 {
					res.Entries = append(res.Entries, Entry{Date: snap, Type: "valuation", Source: "valuation", ExternalID: "valuation:" + a.ID,
						Description: "Valuation change: " + m.Name + " reported value " + snap + " minus pinned opening and known flows",
						Lines: []Line{
							{Account: m.Name, Amount: gap, Currency: USD, CategorySource: "valuation", Memo: "reported value minus pinned opening and known flows"},
							{Account: UnrealizedAcc, Amount: -gap, Currency: USD, CategorySource: "valuation"},
						}})
				}
			} else {
				res.NewPins = append(res.NewPins, OpeningPin{ExternalID: xid, Account: m.Name, AsOf: snap, Amount: open})
				basis += ", pinned"
			}
		} else if pin, ok := in.OpeningPins[xid]; ok {
			// Pinned: the opening never moves again. A difference between the reported balance
			// and pinned opening + posted activity is activity the institution already counts
			// but that has not posted here yet (e.g. a Venmo payment whose bank funding debit
			// lags). It goes to a pending account, recomputed every run, so it clears by itself
			// once the missing row posts (the gap becomes zero and the entry disappears) and is
			// never absorbed into opening equity.
			open = pin.Amount
			basis = "connector balance " + pin.AsOf + " minus posted activity, pinned"
			if gap := target - upTo - pin.Amount; gap != 0 {
				pend, configured := m.PendingAccount, true
				if pend == "" {
					pend, configured = DefaultPendingAcct, false
				}
				res.Pending = append(res.Pending, PendingGap{Account: m.Name, Pending: pend, AsOf: snap, Amount: gap, Configured: configured})
				res.Entries = append(res.Entries, Entry{Date: snap, Type: "adjustment", Source: PendingSource, ExternalID: "pending:" + a.ID,
					Description: "Pending: " + m.Name + " reported balance " + snap + " vs posted activity (unposted funding/activity; clears when it posts)",
					Lines: []Line{
						{Account: m.Name, Amount: gap, Currency: USD, CategorySource: "pending", Memo: "reported balance minus pinned opening and posted activity"},
						{Account: pend, Amount: -gap, Currency: USD, CategorySource: "pending"},
					}})
			}
		} else {
			res.NewPins = append(res.NewPins, OpeningPin{ExternalID: xid, Account: m.Name, AsOf: snap, Amount: open})
			basis += ", pinned"
		}
		res.addOpening(m.Name, date, open, basis, xid)
	}
	for _, s := range cfg.COA.Accounts {
		if s.AssumeEndBalance == nil || sums[s.Name] == 0 && first[s.Name] == "" {
			continue
		}
		end, err := money.ParseMinor(*s.AssumeEndBalance, 2)
		if err != nil {
			return nil, fmt.Errorf("coa %s assume_end_balance: %w", s.Name, err)
		}
		res.addOpening(s.Name, dayBefore(first[s.Name]), end-sums[s.Name], "assumed ending balance "+*s.AssumeEndBalance, "opening:internal:"+s.Name)
	}

	// ---- register the chart of accounts and every referenced account ----
	for _, s := range cfg.COA.Accounts {
		if _, ok := res.Accounts[s.Name]; !ok {
			if err := res.addAccount(cfg, s.Name, "", "", ""); err != nil {
				return nil, err
			}
		}
	}
	// ---- register every referenced account ----
	for _, e := range res.Entries {
		for _, l := range e.Lines {
			if _, ok := res.Accounts[l.Account]; !ok {
				if err := res.addAccount(cfg, l.Account, "", "", ""); err != nil {
					return nil, err
				}
			}
		}
	}
	// accrual P&L / balance accounts are registered even when no line hits them yet
	for _, a := range res.Accruals {
		for _, n := range []string{a.Account, a.Balance} {
			if _, ok := res.Accounts[n]; n != "" && !ok {
				if err := res.addAccount(cfg, n, "", "", ""); err != nil {
					return nil, err
				}
			}
		}
	}
	sort.SliceStable(res.Entries, func(i, j int) bool {
		if res.Entries[i].Date != res.Entries[j].Date {
			return res.Entries[i].Date < res.Entries[j].Date
		}
		return res.Entries[i].ExternalID < res.Entries[j].ExternalID
	})
	for _, e := range res.Entries {
		var s int64
		for _, l := range e.Lines {
			s += l.Amount
		}
		if s != 0 {
			return nil, fmt.Errorf("internal error: entry %s unbalanced by %d", e.ExternalID, s)
		}
	}
	return res, nil
}

func (res *Result) addOpening(account, date string, amount int64, basis, xid string) {
	res.Openings = append(res.Openings, Opening{Account: account, Date: date, Amount: amount, Basis: basis})
	res.Entries = append(res.Entries, Entry{Date: date, Description: "Opening balance (" + basis + ")", Type: "opening",
		Source: "opening", ExternalID: xid,
		Lines: []Line{
			{Account: account, Amount: amount, Currency: USD, CategorySource: "opening"},
			{Account: OpeningEquity, Amount: -amount, Currency: USD, CategorySource: "opening"},
		}})
}

func (res *Result) addAccount(cfg *Config, name, ext, mask, inst string) error {
	attrs, err := cfg.Resolve(name)
	if err != nil {
		return err
	}
	res.Accounts[name] = &AccountInfo{Name: name, Attrs: attrs, ExternalID: ext, Mask: mask, Institution: inst}
	return nil
}

func connectorBalance(a connector.Account) (int64, error) {
	if a.CurrentBalance == nil {
		return 0, fmt.Errorf("account %s has no current_balance", a.ID)
	}
	v, err := money.ParseMinor(*a.CurrentBalance, 2)
	if err != nil {
		return 0, fmt.Errorf("account %s balance: %w", a.ID, err)
	}
	if a.Class == "liability" {
		v = -v // connector reports amount owed as positive; ledger is debit-positive
	}
	return v, nil
}

func categorize(r *row, cfg *Config) (string, string, *Rule) {
	text := r.t.Name + " | " + r.t.Merchant + " | " + r.t.Description
	dir := "in"
	if r.amt < 0 {
		dir = "out"
	}
	for i := range cfg.Rules.Rules {
		ru := &cfg.Rules.Rules[i]
		if ru.re != nil && !ru.re.MatchString(text) {
			continue
		}
		if ru.plaidRe != nil && !ru.plaidRe.MatchString(r.t.CategoryDetailed) {
			continue
		}
		if ru.Direction != "" && ru.Direction != dir {
			continue
		}
		if ru.Kind != "" && ru.Kind != r.t.Kind {
			continue
		}
		if ru.instRe != nil && !ru.instRe.MatchString(r.acct.Institution) {
			continue
		}
		if abs := absMinor(r.amt); ru.minAbs != nil && abs < *ru.minAbs || ru.maxAbs != nil && abs > *ru.maxAbs {
			continue
		}
		return strings.ReplaceAll(ru.Account, "{institution}", r.acct.Institution), "rule:" + ru.Origin + ":" + ru.Name, ru
	}
	if a, ok := cfg.Rules.PlaidFallback[r.t.CategoryDetailed]; ok {
		return a, "plaid:" + r.t.CategoryDetailed, nil
	}
	if a, ok := cfg.Rules.PlaidFallback[r.t.CategoryPrimary]; ok {
		return a, "plaid:" + r.t.CategoryPrimary, nil
	}
	if r.t.CategoryPrimary == "TRANSFER_IN" || r.t.CategoryPrimary == "TRANSFER_OUT" {
		return cfg.Rules.Fallback.Transfer, "unmatched:plaid-transfer", nil
	}
	if dir == "in" {
		return cfg.Rules.Fallback.In, "uncategorized", nil
	}
	return cfg.Rules.Fallback.Out, "uncategorized", nil
}

// applyTags appends the dimensions of every matching tag rule (deduplicated, order kept).
func applyTags(cfg *Config, r *row, acct string, dims []string) []string {
	if len(cfg.Rules.Tags) == 0 {
		return dims
	}
	text := r.t.Name + " | " + r.t.Merchant + " | " + r.t.Description
	out := append([]string(nil), dims...)
	have := map[string]bool{}
	for _, d := range out {
		have[d] = true
	}
	for i := range cfg.Rules.Tags {
		t := &cfg.Rules.Tags[i]
		if t.re != nil && !t.re.MatchString(text) || t.exRe != nil && t.exRe.MatchString(text) {
			continue
		}
		if t.acctRe != nil && !t.acctRe.MatchString(acct) || t.instRe != nil && !t.instRe.MatchString(r.acct.Institution) {
			continue
		}
		if t.From != "" && r.t.Date < t.From || t.To != "" && r.t.Date > t.To {
			continue
		}
		for _, d := range t.Dimensions {
			if !have[d] {
				have[d] = true
				out = append(out, d)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var (
	cpDigits = regexp.MustCompile(`\s*(ID:\S+|#\S+|\d{4,}\S*)`)
	cpSpace  = regexp.MustCompile(`\s+`)
)

func counterpartyOf(t connector.Txn) string {
	if t.Merchant != "" {
		return t.Merchant
	}
	n := cpDigits.ReplaceAllString(t.Name, "")
	n = strings.Trim(cpSpace.ReplaceAllString(n, " "), " -*")
	if n == "" {
		n = t.Name
	}
	return n
}

func absMinor(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
