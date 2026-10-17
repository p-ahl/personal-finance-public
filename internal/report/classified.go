package report

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/p-ahl/personal-finance/internal/money"
)

// Classified multi-period statements (balance sheet, income statement, indirect cash flow).
// Every number is integer cents summed from v_ledger; the three statements tie to each other:
// the balance sheet balances, net income equals the change in accumulated net income, and the
// cash flow's ending cash equals the cash on the balance sheet. Grouping is derived from account
// names and the chart's report_line / cash_flow class, so no personal names live here; plain
// labels for individual accounts can come from an optional local labels file.

// Column is one statement column: a period [From, To] (income statement, cash flow) or an
// as-of date To (balance sheet).
type Column struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	From  string `json:"from,omitempty"`
	To    string `json:"to"`
}

// Row kinds: section (heading, no values), group (subtotal of its items), item, subtotal,
// total, check (must be zero), ratio (values in basis points), memo (not part of the totals).
type Row struct {
	Kind    string
	Section string
	Group   string
	Label   string
	Key     string // account or account prefix the item aggregates (items only)
	Values  []int64
}

func (r Row) MarshalJSON() ([]byte, error) {
	vals := make([]json.RawMessage, len(r.Values))
	for i, v := range r.Values {
		if r.Kind == "ratio" {
			vals[i] = json.RawMessage(money.Format(v, 4)) // fraction, e.g. 0.2531
		} else {
			vals[i] = json.RawMessage(money.Format(v, 2))
		}
	}
	if r.Values == nil {
		vals = nil
	}
	return json.Marshal(struct {
		Kind    string            `json:"kind"`
		Section string            `json:"section,omitempty"`
		Group   string            `json:"group,omitempty"`
		Label   string            `json:"label"`
		Key     string            `json:"key,omitempty"`
		Values  []json.RawMessage `json:"values,omitempty"`
	}{r.Kind, r.Section, r.Group, r.Label, r.Key, vals})
}

// Statement is a classified, multi-column statement.
type Statement struct {
	Kind    string   `json:"kind"`
	Title   string   `json:"title"`
	Basis   string   `json:"basis"`
	Columns []Column `json:"columns"`
	Rows    []Row    `json:"rows"`
	Notes   []string `json:"notes,omitempty"`
}

// Labels maps an item key (account name, or the institution prefix for cash) to a plain label.
type Labels map[string]string

// LoadLabels reads an optional YAML file `labels: {<account or prefix>: <label>}`. A missing
// file is not an error.
func LoadLabels(path string) (Labels, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) || path == "" {
		return Labels{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Labels map[string]string `yaml:"labels"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Labels == nil {
		f.Labels = map[string]string{}
	}
	return Labels(f.Labels), nil
}

// ---------- periods ----------

const day = "2006-01-02"

func parseDay(s string) time.Time { t, _ := time.Parse(day, s); return t }

// Periods builds statement columns between from and to: one per month/quarter/year (clipped to
// the range; a partial last period is labelled "to MM-DD"), or one column for "none"/"total".
func Periods(from, to, period string) ([]Column, error) {
	f, t := parseDay(from), parseDay(to)
	if f.IsZero() || t.IsZero() || t.Before(f) {
		return nil, fmt.Errorf("bad period %s..%s", from, to)
	}
	step := 0
	switch period {
	case "month":
		step = 1
	case "quarter":
		step = 3
	case "year":
		step = 12
	case "", "none", "total":
		return []Column{{Key: from + ".." + to, Label: from + " to " + to, From: from, To: to}}, nil
	default:
		return nil, fmt.Errorf("period must be month, quarter, year or none")
	}
	var out []Column
	start := time.Date(f.Year(), f.Month(), 1, 0, 0, 0, 0, time.UTC)
	if step == 3 {
		start = time.Date(f.Year(), time.Month((int(f.Month())-1)/3*3+1), 1, 0, 0, 0, 0, time.UTC)
	} else if step == 12 {
		start = time.Date(f.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	}
	for p := start; !p.After(t); p = p.AddDate(0, step, 0) {
		pe := p.AddDate(0, step, -1)
		cf, ct := p, pe
		if cf.Before(f) {
			cf = f
		}
		partial := false
		if ct.After(t) {
			ct, partial = t, true
		}
		var label string
		switch step {
		case 1:
			label = p.Format("Jan 2006")
		case 3:
			label = fmt.Sprintf("Q%d %d", (int(p.Month())-1)/3+1, p.Year())
		default:
			label = p.Format("2006")
		}
		if partial {
			label += " (to " + ct.Format("01-02") + ")"
		}
		out = append(out, Column{Key: p.Format("2006-01"), Label: label, From: cf.Format(day), To: ct.Format(day)})
	}
	return out, nil
}

// ExtraPeriod returns a named summary column ending at to: "ytd" (calendar year to date),
// "ttm" (trailing 12 months ending at to) or "total" (from..to).
func ExtraPeriod(name, from, to string) (Column, error) {
	t := parseDay(to)
	switch name {
	case "ytd":
		return Column{Key: "ytd", Label: fmt.Sprintf("%d YTD", t.Year()), From: fmt.Sprintf("%d-01-01", t.Year()), To: to}, nil
	case "ttm":
		return Column{Key: "ttm", Label: "Trailing 12 months", From: t.AddDate(-1, 0, 1).Format(day), To: to}, nil
	case "total":
		return Column{Key: "total", Label: "Total", From: from, To: to}, nil
	}
	return Column{}, fmt.Errorf("unknown extra column %q (ytd, ttm, total)", name)
}

// AsOfDates returns the month ends between from and to plus to itself (balance sheet columns).
func AsOfDates(from, to string) []Column {
	f, t := parseDay(from), parseDay(to)
	var out []Column
	for m := time.Date(f.Year(), f.Month(), 1, 0, 0, 0, 0, time.UTC); !m.After(t); m = m.AddDate(0, 1, 0) {
		me := m.AddDate(0, 1, -1)
		if me.Before(f) {
			continue
		}
		if me.After(t) {
			break
		}
		out = append(out, Column{Key: me.Format(day), Label: me.Format("Jan 2, 2006"), To: me.Format(day)})
	}
	if len(out) == 0 || out[len(out)-1].To != to {
		out = append(out, Column{Key: to, Label: t.Format("Jan 2, 2006"), To: to})
	}
	return out
}

// ---------- ledger lines ----------

type sline struct {
	entry                                int64
	date, etype, desc                    string
	account, atype, cfclass, stmt, rline string
	amt                                  int64 // debit-positive cents
}

func loadSLines(db *sql.DB, to string) ([]sline, error) {
	rows, err := db.Query(`SELECT entry_id, date, entry_type, COALESCE(description,''), account, account_type,
		COALESCE(cash_flow_class,''), statement, COALESCE(report_line,''), amount_minor
		FROM v_ledger WHERE date <= ? ORDER BY entry_id, line_id`, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sline
	for rows.Next() {
		var l sline
		if err := rows.Scan(&l.entry, &l.date, &l.etype, &l.desc, &l.account, &l.atype, &l.cfclass, &l.stmt, &l.rline, &l.amt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---------- labels ----------

// Humanize turns an account segment into words: "XMoney" -> "X Money", "RewardsCard-1000" ->
// "Rewards Card 1000", "Realized-Gains" -> "Realized Gains". Short chunks stay joined ("SoFi",
// "AcmeCo"), so brand-style names survive; plain labels can be set in the labels file.
func Humanize(seg string) string {
	var words []string
	for _, part := range strings.FieldsFunc(seg, func(r rune) bool { return r == '-' || r == '_' || r == ' ' }) {
		words = append(words, splitCamel(part)...)
	}
	return strings.Join(words, " ")
}

func splitCamel(s string) []string {
	r := []rune(s)
	var cuts []int
	for i := 1; i < len(r); i++ {
		lowerToUpper := (unicode.IsLower(r[i-1]) || unicode.IsDigit(r[i-1])) && unicode.IsUpper(r[i])
		acronymEnd := unicode.IsUpper(r[i-1]) && unicode.IsUpper(r[i]) && i+1 < len(r) && unicode.IsLower(r[i+1])
		if lowerToUpper || acronymEnd {
			cuts = append(cuts, i)
		}
	}
	var out []string
	start := 0
	for k, c := range cuts {
		next := len(r)
		if k+1 < len(cuts) {
			next = cuts[k+1]
		}
		left, right := r[start:c], r[c:next]
		allCaps := strings.ToUpper(string(left)) == string(left)
		if (len(left) >= 3 || allCaps) && len(right) >= 3 {
			out = append(out, string(left))
			start = c
		}
	}
	return append(out, string(r[start:]))
}

func humanJoin(segs []string) string {
	var p []string
	for _, s := range segs {
		p = append(p, Humanize(s))
	}
	return strings.Join(p, " ")
}

func (lb Labels) label(key, def string) string {
	if v, ok := lb[key]; ok && v != "" {
		return v
	}
	return def
}

// ---------- classification ----------

var retirementRe = regexp.MustCompile(`(?i)\b(ira|roth|401k|403b|retire|hsa)|IRA|Roth|401k|403b`)

type bsClass struct{ section, group, key, item string }

var bsSections = []string{"Current assets", "Investments", "Property and equipment", "Other assets",
	"Current liabilities", "Long-term liabilities", "Equity"}
var bsGroups = []string{"Cash", "Money owed to us", "Prepaid expenses", "Escrow", "Transfers in transit",
	"Brokerage and private shares", "Retirement accounts", "Crypto",
	"Home (at cost)", "Land", "Construction in progress", "Vehicles", "Other assets",
	"Credit cards", "Bills and payables", "Pending funding", "Accrued taxes", "Accrued interest and other", "Owed to others (pass-through)", "Other liabilities",
	"Mortgages", "Loans", "Equity"}

// investingOrder is the cash-flow investing section: gross outflows then inflows (not netted
// by balance-sheet group), so a large land payment is not obscured by share-sale proceeds.
var investingOrder = []string{
	"Land purchase payments", "Construction in progress", "Home improvements",
	"Purchases of investments", "Vehicles bought",
	"Proceeds from sales of investments", "Proceeds from land sales", "Vehicles sold",
}

// investingLine maps a balance-sheet investing group and the cash-flow amount (cash in +, cash
// out −) onto a gross statement line. Brokerage, retirement and crypto buys and sells are not
// netted; land and CIP stay as their own lines.
func investingLine(grp string, amt int64) (key, label string) {
	if amt == 0 {
		return "", ""
	}
	switch grp {
	case "Brokerage and private shares", "Retirement accounts", "Crypto":
		if amt < 0 {
			return "Purchases of investments", "Purchases of investments"
		}
		return "Proceeds from sales of investments", "Proceeds from sales of investments"
	case "Land":
		if amt < 0 {
			return "Land purchase payments", "Land purchase payments"
		}
		return "Proceeds from land sales", "Proceeds from land sales"
	case "Construction in progress":
		return "Construction in progress", "Construction in progress"
	case "Home (at cost)":
		return "Home improvements", "Home improvements"
	case "Vehicles":
		if amt < 0 {
			return "Vehicles bought", "Vehicles bought"
		}
		return "Vehicles sold", "Vehicles sold"
	default:
		return grp, grp
	}
}

func classifyBS(account, atype, rline, cfclass string) bsClass {
	s := strings.Split(account, ":")
	rest := func(i int) string {
		if len(s) > i {
			return humanJoin(s[i:])
		}
		return Humanize(s[len(s)-1])
	}
	has := func(p string) bool { return strings.HasPrefix(account, p) }
	switch atype {
	case "asset":
		switch {
		case has("Assets:Bank:") || has("Assets:Cash") || cfclass == "cash":
			if len(s) > 2 {
				return bsClass{"Current assets", "Cash", strings.Join(s[:3], ":"), Humanize(s[2])}
			}
			return bsClass{"Current assets", "Cash", account, Humanize(s[len(s)-1])}
		case has("Assets:Receivable") || rline == "Receivables":
			return bsClass{"Current assets", "Money owed to us", account, rest(2)}
		case has("Assets:Prepaid") || rline == "Prepaid expenses":
			return bsClass{"Current assets", "Prepaid expenses", account, rest(2)}
		case has("Assets:Escrow"):
			return bsClass{"Current assets", "Escrow", account, rest(2)}
		case has("Assets:Transfers"):
			return bsClass{"Current assets", "Transfers in transit", account, rest(2)}
		case has("Assets:Investments"):
			if retirementRe.MatchString(account) {
				return bsClass{"Investments", "Retirement accounts", account, rest(2)}
			}
			return bsClass{"Investments", "Brokerage and private shares", account, rest(2)}
		case has("Assets:Crypto"):
			return bsClass{"Investments", "Crypto", account, strings.TrimPrefix(rest(2), "Exchange ")}
		case has("Assets:RealEstate"):
			switch {
			case strings.Contains(account, ":CIP") || strings.Contains(account, ":Construction"):
				return bsClass{"Property and equipment", "Construction in progress", account, rest(2)}
			case strings.Contains(account, ":Land") || strings.Contains(account, ":Lot"):
				return bsClass{"Property and equipment", "Land", account, rest(2)}
			}
			return bsClass{"Property and equipment", "Home (at cost)", account, rest(2)}
		case has("Assets:Land"):
			return bsClass{"Property and equipment", "Land", account, rest(1)}
		case has("Assets:Vehicles"):
			return bsClass{"Property and equipment", "Vehicles", account, rest(2)}
		}
		return bsClass{"Other assets", "Other assets", account, rest(1)}
	case "liability":
		switch {
		case has("Liabilities:CreditCard") || rline == "Credit cards":
			return bsClass{"Current liabilities", "Credit cards", account, rest(2)}
		case has("Liabilities:Payable") || has("Liabilities:Vendor") || rline == "Payables":
			return bsClass{"Current liabilities", "Bills and payables", account, rest(2)}
		case has("Liabilities:Accrued:Taxes"):
			return bsClass{"Current liabilities", "Accrued taxes", account, rest(3)}
		case has("Liabilities:Accrued"):
			return bsClass{"Current liabilities", "Accrued interest and other", account, rest(2)}
		case has("Liabilities:Pending"):
			return bsClass{"Current liabilities", "Pending funding", account, rest(2)}
		case has("Liabilities:PassThrough"):
			return bsClass{"Current liabilities", "Owed to others (pass-through)", account, rest(2)}
		case has("Liabilities:Mortgage") || rline == "Mortgages":
			return bsClass{"Long-term liabilities", "Mortgages", account, rest(2)}
		case has("Liabilities:Loan") || rline == "Loans":
			return bsClass{"Long-term liabilities", "Loans", account, rest(2)}
		}
		return bsClass{"Current liabilities", "Other liabilities", account, rest(1)}
	}
	return bsClass{"Equity", "Equity", "opening-equity", "Opening equity (balances at books start)"}
}

// linkable: receivable/payable accounts that can belong to an investing or financing deal.
func linkable(account string) bool {
	return strings.HasPrefix(account, "Assets:Receivable") || strings.HasPrefix(account, "Liabilities:Payable") ||
		strings.HasPrefix(account, "Liabilities:Vendor")
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return len(list)
}

// Income statement categories by report_line.
var isCategory = map[string]string{
	"Salary": "Wages and salaries", "Business income": "Business income", "Other income": "Other income",
	"Uncategorized income": "Other income",
	"Interest":             "Interest and dividends", "Investment income": "Interest and dividends",
	"Realized gains": "Investment gains", "Unrealized gains": "Investment gains", "Retirement distributions": "Retirement distributions",
	"Housing": "Housing", "Utilities": "Utilities", "Food": "Food", "Transport": "Transportation", "Healthcare": "Healthcare",
	"Baby": "Baby and childcare", "Childcare": "Baby and childcare", "Insurance": "Insurance", "Shopping": "Shopping",
	"Subscriptions": "Subscriptions and software", "Software": "Subscriptions and software", "Education": "Education",
	"Entertainment": "Entertainment and travel", "Travel": "Entertainment and travel",
	"Personal care": "Personal care, gifts and other", "Gifts": "Personal care, gifts and other", "Fees": "Personal care, gifts and other",
	"Other expenses": "Personal care, gifts and other", "Uncategorized expenses": "Personal care, gifts and other",
	"Charity": "Giving", "Taxes": "Taxes", "Interest expense": "Interest on loans",
}

var isSectionOf = map[string]string{
	"Wages and salaries": "Income", "Business income": "Income", "Other income": "Income",
	"Interest and dividends": "Non-operating", "Investment gains": "Non-operating", "Retirement distributions": "Non-operating",
	"Interest on loans": "Non-operating", "Taxes": "Taxes",
}

var isGroups = []string{"Wages and salaries", "Business income", "Other income",
	"Housing", "Utilities", "Food", "Transportation", "Healthcare", "Baby and childcare", "Insurance", "Shopping",
	"Subscriptions and software", "Education", "Entertainment and travel", "Personal care, gifts and other", "Giving",
	"Interest and dividends", "Investment gains", "Retirement distributions", "Interest on loans", "Taxes"}

type isClass struct{ section, group, key, item string }

func classifyIS(account, atype, rline string) isClass {
	s := strings.Split(account, ":")
	g, ok := isCategory[rline]
	if !ok {
		if atype == "income" {
			g = "Other income"
		} else {
			g = "Personal care, gifts and other"
		}
	}
	sec, ok := isSectionOf[g]
	if !ok {
		sec = "Living expenses"
	}
	item := Humanize(s[len(s)-1])
	switch {
	case strings.HasPrefix(account, "Income:Salary:") && len(s) >= 4:
		item = Humanize(s[2]) + " - " + Humanize(strings.TrimSuffix(s[3], "-Equity"))
		if strings.HasSuffix(s[3], "-Equity") {
			item += " (paid in shares)"
		}
	case len(s) >= 3:
		item = humanJoin(s[2:])
		if g == "Interest and dividends" && s[1] == "Interest" {
			item = "Interest " + item
		}
	case len(s) == 2:
		item = Humanize(s[1])
	}
	if g == "Taxes" && len(s) >= 3 {
		item = humanJoin(s[2:]) + " tax"
		if s[2] == "Penalties" {
			item = "Tax penalties"
		}
	}
	return isClass{sec, g, account, item}
}

// ---------- builders ----------

type agg struct {
	order      []string
	vals       map[string][]int64
	label, grp map[string]string
	sec        map[string]string
	n          int
}

func newAgg(n int) *agg {
	return &agg{vals: map[string][]int64{}, label: map[string]string{}, grp: map[string]string{}, sec: map[string]string{}, n: n}
}

func (a *agg) add(sec, grp, key, label string, col int, v int64) {
	if _, ok := a.vals[key]; !ok {
		a.vals[key] = make([]int64, a.n)
		a.order = append(a.order, key)
		a.label[key], a.grp[key], a.sec[key] = label, grp, sec
	}
	a.vals[key][col] += v
}

func addVec(dst, src []int64) {
	for i := range src {
		dst[i] += src[i]
	}
}

func nonzero(v []int64) bool {
	for _, x := range v {
		if x != 0 {
			return true
		}
	}
	return false
}

// emit writes section/group/item rows for the given sections in order, returning section totals.
func (a *agg) emit(st *Statement, sections, groups []string, lb Labels, sectionTotalLabel func(string) string) map[string][]int64 {
	tot := map[string][]int64{}
	for _, sec := range sections {
		var gs []string
		seen := map[string]bool{}
		for _, k := range a.order {
			if a.sec[k] == sec && !seen[a.grp[k]] && nonzero(a.vals[k]) {
				seen[a.grp[k]] = true
				gs = append(gs, a.grp[k])
			}
		}
		if len(gs) == 0 {
			continue
		}
		sort.SliceStable(gs, func(i, j int) bool { return indexOf(groups, gs[i]) < indexOf(groups, gs[j]) })
		st.Rows = append(st.Rows, Row{Kind: "section", Section: sec, Label: sec})
		stot := make([]int64, a.n)
		for _, g := range gs {
			var keys []string
			gt := make([]int64, a.n)
			for _, k := range a.order {
				if a.sec[k] == sec && a.grp[k] == g && nonzero(a.vals[k]) {
					keys = append(keys, k)
					addVec(gt, a.vals[k])
				}
			}
			sort.SliceStable(keys, func(i, j int) bool { return lb.label(keys[i], a.label[keys[i]]) < lb.label(keys[j], a.label[keys[j]]) })
			st.Rows = append(st.Rows, Row{Kind: "group", Section: sec, Group: g, Label: g, Values: gt})
			for _, k := range keys {
				st.Rows = append(st.Rows, Row{Kind: "item", Section: sec, Group: g, Label: lb.label(k, a.label[k]), Key: k, Values: a.vals[k]})
			}
			addVec(stot, gt)
		}
		if sectionTotalLabel != nil {
			st.Rows = append(st.Rows, Row{Kind: "subtotal", Section: sec, Label: sectionTotalLabel(sec), Values: stot})
		}
		tot[sec] = stot
	}
	return tot
}

func vec(n int) []int64 { return make([]int64, n) }

func combine(n int, terms ...[]int64) []int64 {
	out := vec(n)
	for _, t := range terms {
		if t != nil {
			addVec(out, t)
		}
	}
	return out
}

func neg(v []int64) []int64 {
	out := make([]int64, len(v))
	for i := range v {
		out[i] = -v[i]
	}
	return out
}

// ClassifiedBalanceSheet as of each column's To date.
func ClassifiedBalanceSheet(db *sql.DB, cols []Column, lb Labels) (*Statement, error) {
	if len(cols) == 0 {
		return nil, fmt.Errorf("no columns")
	}
	maxTo := cols[0].To
	for _, c := range cols {
		if c.To > maxTo {
			maxTo = c.To
		}
	}
	lines, err := loadSLines(db, maxTo)
	if err != nil {
		return nil, err
	}
	n := len(cols)
	a := newAgg(n)
	ni := vec(n)
	for _, l := range lines {
		for i, c := range cols {
			if l.date > c.To {
				continue
			}
			switch l.stmt {
			case "income_statement":
				ni[i] -= l.amt
			default:
				cl := classifyBS(l.account, l.atype, l.rline, l.cfclass)
				v := l.amt
				if l.atype != "asset" {
					v = -v
				}
				a.add(cl.section, cl.group, cl.key, cl.item, i, v)
			}
		}
	}
	st := &Statement{Kind: "balance-sheet", Title: "Balance sheet (statement of financial position)", Basis: "accrual", Columns: cols}
	// equity: opening balances + accumulated net income
	a.add("Equity", "Equity", "net-income", "Net income since books start (retained earnings)", 0, 0)
	addVec(a.vals["net-income"], ni)

	assetSecs := bsSections[:4]
	liabSecs := bsSections[4:6]
	tA := a.emit(st, assetSecs, bsGroups, lb, func(s string) string { return "Total " + strings.ToLower(s) })
	totalA := combine(n, tA["Current assets"], tA["Investments"], tA["Property and equipment"], tA["Other assets"])
	st.Rows = append(st.Rows, Row{Kind: "total", Label: "Total assets", Values: totalA})
	tL := a.emit(st, liabSecs, bsGroups, lb, func(s string) string { return "Total " + strings.ToLower(s) })
	totalL := combine(n, tL["Current liabilities"], tL["Long-term liabilities"])
	st.Rows = append(st.Rows, Row{Kind: "total", Label: "Total liabilities", Values: totalL})
	tE := a.emit(st, []string{"Equity"}, bsGroups, lb, func(string) string { return "Total equity" })
	st.Rows = append(st.Rows, Row{Kind: "total", Label: "Total liabilities and equity", Values: combine(n, totalL, tE["Equity"])})
	st.Rows = append(st.Rows, Row{Kind: "total", Label: "Net worth (assets minus liabilities)", Values: combine(n, totalA, neg(totalL))})
	st.Rows = append(st.Rows, Row{Kind: "check", Label: "Check: assets - liabilities - equity (must be 0)", Values: combine(n, totalA, neg(totalL), neg(tE["Equity"]))})
	st.Notes = append(st.Notes,
		"Accrual basis. Home, land, construction in progress and vehicles at cost or owner value (not marked to market); valuation-only brokerage/IRA accounts at the connector balance; crypto at net cost.",
		"Unrealized gains, where booked (holdings-tracked accounts), run through net income; there is no separate other-comprehensive-income reserve.",
		"Long-term debt is shown in full under long-term liabilities (the current portion is not split out).")
	return st, nil
}

// ClassifiedIncomeStatement with one column per period.
func ClassifiedIncomeStatement(db *sql.DB, cols []Column, lb Labels) (*Statement, error) {
	if len(cols) == 0 {
		return nil, fmt.Errorf("no columns")
	}
	maxTo := cols[0].To
	for _, c := range cols {
		if c.To > maxTo {
			maxTo = c.To
		}
	}
	lines, err := loadSLines(db, maxTo)
	if err != nil {
		return nil, err
	}
	n := len(cols)
	a := newAgg(n)
	for _, l := range lines {
		if l.stmt != "income_statement" {
			continue
		}
		cl := classifyIS(l.account, l.atype, l.rline)
		v := l.amt // expenses natural = debit
		if l.atype == "income" {
			v = -v
		}
		if cl.section == "Non-operating" && l.atype == "expense" {
			v = -v // shown as a negative inside non-operating
		}
		for i, c := range cols {
			if l.date >= c.From && l.date <= c.To {
				a.add(cl.section, cl.group, cl.key, cl.item, i, v)
			}
		}
	}
	st := &Statement{Kind: "income-statement", Title: "Income statement (profit and loss)", Basis: "accrual", Columns: cols}
	tot := a.emit(st, []string{"Income"}, isGroups, lb, func(string) string { return "Total income" })
	inc := combine(n, tot["Income"])
	t2 := a.emit(st, []string{"Living expenses"}, isGroups, lb, func(string) string { return "Total living expenses" })
	liv := combine(n, t2["Living expenses"])
	op := combine(n, inc, neg(liv))
	st.Rows = append(st.Rows, Row{Kind: "total", Label: "Operating surplus (income minus living expenses)", Values: op})
	t3 := a.emit(st, []string{"Non-operating"}, isGroups, lb, func(string) string { return "Net non-operating income" })
	nonop := combine(n, t3["Non-operating"])
	pre := combine(n, op, nonop)
	st.Rows = append(st.Rows, Row{Kind: "total", Label: "Net income before taxes", Values: pre})
	t4 := a.emit(st, []string{"Taxes"}, isGroups, lb, func(string) string { return "Total taxes" })
	tax := combine(n, t4["Taxes"])
	netinc := combine(n, pre, neg(tax))
	st.Rows = append(st.Rows, Row{Kind: "total", Label: "Net income", Values: netinc})
	// savings rate = net income / (income + positive non-operating income)
	rate := vec(n)
	for i := range rate {
		den := inc[i]
		for _, k := range a.order {
			if a.sec[k] == "Non-operating" && a.vals[k][i] > 0 {
				den += a.vals[k][i]
			}
		}
		if den != 0 {
			rate[i] = netinc[i] * 10000 / den
		}
	}
	st.Rows = append(st.Rows, Row{Kind: "ratio", Label: "Savings rate (net income / all income)", Values: rate})
	// check vs ledger
	chk := vec(n)
	for _, l := range lines {
		if l.stmt != "income_statement" {
			continue
		}
		for i, c := range cols {
			if l.date >= c.From && l.date <= c.To {
				chk[i] -= l.amt
			}
		}
	}
	st.Rows = append(st.Rows, Row{Kind: "check", Label: "Check: net income vs ledger (must be 0)", Values: combine(n, netinc, neg(chk))})
	st.Notes = append(st.Notes,
		"Accrual basis: income when earned, expenses when incurred (medical by service date, taxes by tax year, prepaid items spread over their term).",
		"Mortgage interest is in Housing; interest on other loans and investment results are non-operating; taxes include income, payroll (where wages are grossed up) and property tax.")
	return st, nil
}

// ClassifiedCashFlow (indirect method) with one column per period. Opening-balance entries are
// starting balances, not flows; entries with no cash and no working-capital leg that only move
// value between investments, property, loans and income (shares received as pay, gains inside
// an account, a purchase financed directly by a lender) are non-cash and excluded.
func ClassifiedCashFlow(db *sql.DB, cols []Column, lb Labels) (*Statement, error) {
	if len(cols) == 0 {
		return nil, fmt.Errorf("no columns")
	}
	maxTo := cols[0].To
	for _, c := range cols {
		if c.To > maxTo {
			maxTo = c.To
		}
	}
	lines, err := loadSLines(db, maxTo)
	if err != nil {
		return nil, err
	}
	n := len(cols)
	ni, adjGain, adjNonCash := vec(n), vec(n), vec(n)
	begin, openIn, end := vec(n), vec(n), vec(n)
	wc, inv, fin := newAgg(n), newAgg(n), newAgg(n)
	memo := newAgg(n)
	type entry struct {
		lines    []sline
		nonCash  bool
		firstInv string // investing group of the first investing line
		firstFin string
	}
	var entries []entry
	for i := 0; i < len(lines); {
		j := i
		for j < len(lines) && lines[j].entry == lines[i].entry {
			j++
		}
		e := entry{lines: lines[i:j]}
		i = j
		hasCash, other, invfin := false, false, false
		for _, l := range e.lines {
			if l.stmt == "income_statement" {
				continue
			}
			switch {
			case l.cfclass == "cash":
				hasCash = true
			case l.cfclass == "investing":
				invfin = true
				if e.firstInv == "" {
					e.firstInv = classifyBS(l.account, l.atype, l.rline, l.cfclass).group
				}
			case l.cfclass == "financing":
				invfin = true
				if e.firstFin == "" {
					e.firstFin = classifyBS(l.account, l.atype, l.rline, l.cfclass).group
				}
			case linkable(l.account):
			default:
				other = true
			}
		}
		e.nonCash = !hasCash && !other && invfin && e.lines[0].etype != "opening"
		entries = append(entries, e)
	}
	// Receivables/payables created by non-cash investing or financing entries (a sale on
	// account, a building invoice not yet paid, a co-owner's share owed to us) are settled in
	// that same activity, not in operating working capital.
	linked := map[string][2]string{} // account -> {section, group}
	for _, e := range entries {
		if !e.nonCash {
			continue
		}
		for _, l := range e.lines {
			if l.stmt != "income_statement" && linkable(l.account) {
				if _, ok := linked[l.account]; !ok {
					if e.firstInv != "" {
						linked[l.account] = [2]string{"investing", e.firstInv}
					} else {
						linked[l.account] = [2]string{"financing", e.firstFin}
					}
				}
			}
		}
	}
	for _, e := range entries {
		date, etype := e.lines[0].date, e.lines[0].etype
		for c, col := range cols {
			for _, l := range e.lines {
				if l.cfclass == "cash" && l.stmt != "income_statement" {
					if date < col.From {
						begin[c] += l.amt
					} else if etype == "opening" && date <= col.To {
						openIn[c] += l.amt
					}
					if date <= col.To {
						end[c] += l.amt
					}
				}
			}
			if date < col.From || date > col.To || etype == "opening" {
				continue
			}
			for _, l := range e.lines {
				if l.stmt == "income_statement" {
					ni[c] -= l.amt
					if l.cfclass == "investing" {
						adjGain[c] += l.amt
					}
				}
			}
			if e.nonCash {
				for _, l := range e.lines {
					switch {
					case l.stmt == "income_statement":
						if l.cfclass != "investing" {
							adjNonCash[c] += l.amt
						}
					case l.amt < 0 && (l.cfclass == "financing" || linkable(l.account)) && e.firstInv != "":
						cl := classifyBS(l.account, l.atype, l.rline, l.cfclass)
						what := "new " + strings.ToLower(cl.group)
						if linkable(l.account) {
							what = "amounts owed (paid later or still owed)"
						}
						memo.add("memo", e.firstInv, e.firstInv+"|"+what, "Non-cash: "+strings.ToLower(e.firstInv)+" acquired with "+what, c, -l.amt)
					}
				}
				continue
			}
			for _, l := range e.lines {
				if l.cfclass == "cash" && l.stmt != "income_statement" {
					continue
				}
				if l.stmt == "income_statement" {
					if l.cfclass == "investing" {
						g := e.firstInv
						if g == "" {
							g = "Brokerage and private shares"
						}
						if key, label := investingLine(g, -l.amt); key != "" {
							inv.add("Investing", key, key, label, c, -l.amt)
						}
					}
					continue
				}
				cl := classifyBS(l.account, l.atype, l.rline, l.cfclass)
				cls := l.cfclass
				grp := cl.group
				if lk, ok := linked[l.account]; ok {
					cls, grp = lk[0], lk[1]
				}
				switch cls {
				case "investing":
					if key, label := investingLine(grp, -l.amt); key != "" {
						inv.add("Investing", key, key, label, c, -l.amt)
					}
				case "financing":
					fin.add("Financing", grp, grp, grp, c, -l.amt)
				case "operating", "unclassified":
					wc.add("Operating", "Changes in working capital", grp, grp, c, -l.amt)
				default: // equity outside opening entries
					fin.add("Financing", "Other equity", "Other equity", "Other equity", c, -l.amt)
				}
			}
		}
	}
	st := &Statement{Kind: "cash-flow", Title: "Statement of cash flows (indirect method)", Basis: "accrual", Columns: cols}
	sumAgg := func(a *agg) []int64 {
		t := vec(n)
		for _, k := range a.order {
			addVec(t, a.vals[k])
		}
		return t
	}
	st.Rows = append(st.Rows, Row{Kind: "section", Section: "Operating", Label: "Cash from operating activities (earning and spending)"})
	st.Rows = append(st.Rows, Row{Kind: "item", Section: "Operating", Group: "Net income", Label: "Net income", Values: ni})
	if nonzero(adjNonCash) {
		st.Rows = append(st.Rows, Row{Kind: "item", Section: "Operating", Group: "Non-cash items", Label: "Non-cash items (pay received in shares, depreciation)", Values: adjNonCash})
	}
	if nonzero(adjGain) {
		st.Rows = append(st.Rows, Row{Kind: "item", Section: "Operating", Group: "Non-cash items", Label: "Less investment gains (any sale proceeds are under investing)", Values: adjGain})
	}
	wcOrder := []string{"Money owed to us", "Prepaid expenses", "Escrow", "Credit cards", "Bills and payables", "Pending funding", "Accrued taxes",
		"Accrued interest and other", "Owed to others (pass-through)", "Transfers in transit", "Other liabilities", "Other assets"}
	sort.SliceStable(wc.order, func(i, j int) bool { return indexOf(wcOrder, wc.order[i]) < indexOf(wcOrder, wc.order[j]) })
	for _, k := range wc.order {
		if nonzero(wc.vals[k]) {
			st.Rows = append(st.Rows, Row{Kind: "item", Section: "Operating", Group: "Changes in working capital", Label: "Change in " + strings.ToLower(lb.label(k, k)), Key: k, Values: wc.vals[k]})
		}
	}
	op := combine(n, ni, adjNonCash, adjGain, sumAgg(wc))
	st.Rows = append(st.Rows, Row{Kind: "subtotal", Section: "Operating", Label: "Net cash from operating activities", Values: op})
	emitFlows := func(sec, title, total string, a *agg, order []string) []int64 {
		st.Rows = append(st.Rows, Row{Kind: "section", Section: sec, Label: title})
		sort.SliceStable(a.order, func(i, j int) bool { return indexOf(order, a.order[i]) < indexOf(order, a.order[j]) })
		for _, k := range a.order {
			if nonzero(a.vals[k]) {
				st.Rows = append(st.Rows, Row{Kind: "item", Section: sec, Group: k, Label: lb.label(k, k), Key: k, Values: a.vals[k]})
			}
		}
		t := sumAgg(a)
		st.Rows = append(st.Rows, Row{Kind: "subtotal", Section: sec, Label: total, Values: t})
		return t
	}
	it := emitFlows("Investing", "Cash from investing activities (investing and building)", "Net cash from investing activities", inv, investingOrder)
	ft := emitFlows("Financing", "Cash from financing activities (borrowing and repaying)", "Net cash from financing activities", fin, bsGroups)
	net := combine(n, op, it, ft)
	st.Rows = append(st.Rows, Row{Kind: "total", Label: "Net change in cash", Values: net})
	start := combine(n, begin, openIn)
	st.Rows = append(st.Rows, Row{Kind: "total", Label: "Cash at start of period", Values: start})
	st.Rows = append(st.Rows, Row{Kind: "total", Label: "Cash at end of period", Values: combine(n, start, net)})
	st.Rows = append(st.Rows, Row{Kind: "total", Label: "Cash on the balance sheet at end of period", Values: end})
	st.Rows = append(st.Rows, Row{Kind: "check", Label: "Check: start + net change - balance-sheet cash (must be 0)", Values: combine(n, start, net, neg(end))})
	// sorted, not first-seen: the order must not depend on journal entry ids (a fresh build and an
	// incremental one hold the same books and must print the same statements)
	memoKeys := append([]string(nil), memo.order...)
	sort.Strings(memoKeys)
	for _, k := range memoKeys {
		st.Rows = append(st.Rows, Row{Kind: "memo", Label: memo.label[k], Key: k, Values: memo.vals[k]})
	}
	if nonzero(openIn) {
		st.Notes = append(st.Notes, "Cash at start includes opening balances recognized during the first period (accounts whose history starts after the period start).")
	}
	st.Notes = append(st.Notes,
		"Cash = bank, savings and wallet accounts (cash_flow class 'cash'); credit cards are working capital, so card spending is operating cash when the card is paid.",
		"Non-cash transactions (shares received as pay, trades and gains inside investment accounts, purchases financed directly by a lender or not yet paid) are excluded from the flows and listed as memo lines where they add assets; receivables and payables from those deals are settled under investing/financing when the cash moves.",
		"Investing activities are shown gross: land purchase payments, construction in progress, purchases of investments, and proceeds from sales of investments are separate lines (not netted within the section).")
	return st, nil
}

// ---------- rendering ----------

func (s *Statement) cell(r Row, i int) string {
	if r.Values == nil {
		return ""
	}
	if r.Kind == "ratio" {
		return fmt.Sprintf("%.1f%%", float64(r.Values[i])/100)
	}
	return usd(r.Values[i])
}

// Table flattens the statement for text / md output.
func (s *Statement) Table() *Table {
	t := &Table{Title: s.Title + " (" + s.Basis + " basis)", Notes: s.Notes}
	t.Headers = []string{""}
	t.Right = []bool{false}
	for _, c := range s.Columns {
		t.Headers = append(t.Headers, c.Label)
		t.Right = append(t.Right, true)
	}
	for _, r := range s.Rows {
		label := r.Label
		switch r.Kind {
		case "section":
			if len(t.Rows) > 0 {
				t.Sep()
			}
			label = strings.ToUpper(label)
		case "group":
			label = "  " + label
		case "item":
			label = "      " + label
		case "subtotal":
			label = "  " + label
		case "memo":
			label = "  (memo) " + label
		}
		cells := []string{label}
		for i := range s.Columns {
			cells = append(cells, s.cell(r, i))
		}
		t.Add(cells...)
	}
	return t
}

// Render writes text, md, csv (machine-friendly: kind/section/group/label/key + plain numbers)
// or json.
func (s *Statement) Render(w io.Writer, format string) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	case "csv":
		cw := csv.NewWriter(w)
		h := []string{"kind", "section", "group", "label", "key"}
		for _, c := range s.Columns {
			h = append(h, c.Label)
		}
		if err := cw.Write(h); err != nil {
			return err
		}
		for _, r := range s.Rows {
			rec := []string{r.Kind, r.Section, r.Group, r.Label, r.Key}
			for i := range s.Columns {
				v := ""
				if r.Values != nil {
					if r.Kind == "ratio" {
						v = money.Format(r.Values[i], 4)
					} else {
						v = money.Format(r.Values[i], 2)
					}
				}
				rec = append(rec, v)
			}
			if err := cw.Write(rec); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	}
	return s.Table().Render(w, format)
}

// CheckRow returns the largest absolute value on the statement's check rows (0 = ties out).
func (s *Statement) CheckRow() int64 {
	var worst int64
	for _, r := range s.Rows {
		if r.Kind != "check" {
			continue
		}
		for _, v := range r.Values {
			if v < 0 {
				v = -v
			}
			if v > worst {
				worst = v
			}
		}
	}
	return worst
}
