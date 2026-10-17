// Package connector reads a dated raw extract folder produced from the Finance (Plaid-backed)
// connector: connections.json, accounts.json, *_txns_pageNNN.csv, holdings*.csv.
package connector

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Connection struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	InstitutionID   string `json:"institution_id"`
	InstitutionName string `json:"institution_name"`
	Status          string `json:"status"`
}

type Account struct {
	ID               string          `json:"id"`
	ConnectionID     string          `json:"connection_id"`
	Class            string          `json:"class"` // cash | liability | investment
	Mask             *string         `json:"mask"`
	Name             string          `json:"name"`
	OfficialName     *string         `json:"official_name"`
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	CurrentBalance   *string         `json:"current_balance"`
	AvailableBalance *string         `json:"available_balance"`
	Currency         string          `json:"currency"`
	Raw              json.RawMessage `json:"-"`
}

// Txn is a cash or liability transaction row. Amount keeps the connector's user-facing sign:
// positive = money in (deposit, refund, card payment), negative = money out (spend, card charge).
type Txn struct {
	ID, AccountID, Kind, Date, Amount, Direction, Currency string
	Name, Merchant, Description                            string
	Pending                                                bool
	CategoryPrimary, CategoryDetailed                      string
	Raw                                                    map[string]string
}

type PortfolioTxn struct {
	ID, AccountID, Date, Type, Ticker, SecurityName string
	Amount, Price, Quantity, Fees, Currency         string
	Raw                                             map[string]string
}

type Holding struct {
	ID, AccountID, Ticker, SecurityName, SecurityType string
	Quantity, CostBasis, Price, Value, Currency       string
	Raw                                               map[string]string
}

type Extract struct {
	Dir           string
	AsOf          string
	Connections   []Connection
	Accounts      []Account
	Txns          []Txn
	PortfolioTxns []PortfolioTxn
	Holdings      []Holding
	PageRows      int // rows read across pages, before dedup
	Conflicts     []string
}

var dateDir = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Read loads and dedups an extract folder. Rows are deduplicated by connector transaction id;
// if the same id appears twice with different content, the posted version wins.
func Read(dir string) (*Extract, error) {
	ex := &Extract{Dir: dir}
	if base := filepath.Base(filepath.Clean(dir)); dateDir.MatchString(base) {
		ex.AsOf = base
	}
	var conns struct {
		Connections []Connection `json:"connections"`
	}
	if err := readJSON(filepath.Join(dir, "connections.json"), &conns); err != nil {
		return nil, err
	}
	ex.Connections = conns.Connections

	var accts struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := readJSON(filepath.Join(dir, "accounts.json"), &accts); err != nil {
		return nil, err
	}
	for _, raw := range accts.Accounts {
		var a Account
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("accounts.json: %w", err)
		}
		a.Raw = raw
		ex.Accounts = append(ex.Accounts, a)
	}

	byID := map[string]Txn{}
	for _, kind := range []string{"cash", "liability"} {
		files, err := readPagesByFile(dir, kind+"_txns_page*.csv")
		if err != nil {
			return nil, err
		}
		var rows []map[string]string
		for _, fr := range files {
			assignSyntheticIDs(kind, fr)
			rows = append(rows, fr...)
		}
		ex.PageRows += len(rows)
		for _, r := range rows {
			t := TxnFromRow(kind, r)
			if t.ID == "" || t.AccountID == "" || t.Date == "" {
				return nil, fmt.Errorf("%s row missing id/account/date: %v", kind, r)
			}
			if prev, ok := byID[t.ID]; ok {
				if !sameRow(prev.Raw, t.Raw) {
					ex.Conflicts = append(ex.Conflicts, t.ID)
					if prev.Pending && !t.Pending {
						byID[t.ID] = t
					}
				}
				continue
			}
			byID[t.ID] = t
		}
	}
	for _, t := range byID {
		ex.Txns = append(ex.Txns, t)
	}
	sort.Slice(ex.Txns, func(i, j int) bool {
		if ex.Txns[i].Date != ex.Txns[j].Date {
			return ex.Txns[i].Date < ex.Txns[j].Date
		}
		return ex.Txns[i].ID < ex.Txns[j].ID
	})

	prow, err := readPages(dir, "portfolio_txns_page*.csv")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, r := range prow {
		if seen[r["id"]] {
			continue
		}
		seen[r["id"]] = true
		ex.PortfolioTxns = append(ex.PortfolioTxns, PortfolioFromRow(r))
	}
	sort.Slice(ex.PortfolioTxns, func(i, j int) bool {
		a, b := ex.PortfolioTxns[i], ex.PortfolioTxns[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		return a.ID < b.ID
	})

	hrow, err := readPages(dir, "holdings*.csv")
	if err != nil {
		return nil, err
	}
	seen = map[string]bool{}
	for _, r := range hrow {
		if seen[r["id"]] {
			continue
		}
		seen[r["id"]] = true
		ex.Holdings = append(ex.Holdings, HoldingFromRow(r))
	}
	sort.Slice(ex.Holdings, func(i, j int) bool { return ex.Holdings[i].ID < ex.Holdings[j].ID })
	return ex, nil
}

func sameRow(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// connectorPage is a connector tool result saved verbatim as JSON: the rows are CSV text in
// "csv"; "row_count" is the number of data rows.
type connectorPage struct {
	CSV      *string `json:"csv"`
	RowCount *int    `json:"row_count"`
}

// pageFiles returns the page files matching a *.csv pattern. A verbatim JSON tool result
// (same name, .json) is used when the .csv twin is absent, so an extract can be just the saved
// tool results.
func pageFiles(dir, pattern string) ([]string, error) {
	csvs, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, f := range csvs {
		have[strings.TrimSuffix(f, ".csv")] = true
	}
	jsons, err := filepath.Glob(filepath.Join(dir, strings.TrimSuffix(pattern, ".csv")+".json"))
	if err != nil {
		return nil, err
	}
	files := csvs
	for _, f := range jsons {
		if !have[strings.TrimSuffix(f, ".json")] {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	return files, nil
}

func readPageRecords(f string) ([][]string, error) {
	if strings.HasSuffix(f, ".json") {
		var pg connectorPage
		if err := readJSON(f, &pg); err != nil {
			return nil, err
		}
		if pg.CSV == nil {
			return nil, fmt.Errorf("%s: no \"csv\" field (not a connector page)", f)
		}
		recs, err := csv.NewReader(strings.NewReader(*pg.CSV)).ReadAll()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if pg.RowCount != nil && len(recs) > 0 && len(recs)-1 != *pg.RowCount {
			return nil, fmt.Errorf("%s: row_count %d but csv has %d rows (truncated page?)", f, *pg.RowCount, len(recs)-1)
		}
		return recs, nil
	}
	fh, err := os.Open(f)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	recs, err := csv.NewReader(fh).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f, err)
	}
	return recs, nil
}

func readPages(dir, pattern string) ([]map[string]string, error) {
	files, err := readPagesByFile(dir, pattern)
	if err != nil {
		return nil, err
	}
	var out []map[string]string
	for _, f := range files {
		out = append(out, f...)
	}
	return out, nil
}

// readPagesByFile reads every page file matching pattern, keeping the rows of each file together
// (in file order) so per-file occurrence numbers can be assigned.
func readPagesByFile(dir, pattern string) ([][]map[string]string, error) {
	files, err := pageFiles(dir, pattern)
	if err != nil {
		return nil, err
	}
	var out [][]map[string]string
	for _, f := range files {
		recs, err := readPageRecords(f)
		if err != nil {
			return nil, err
		}
		if len(recs) == 0 {
			continue
		}
		hdr := recs[0]
		var rows []map[string]string
		for _, rec := range recs[1:] {
			m := make(map[string]string, len(hdr))
			for i, h := range hdr {
				if i < len(rec) {
					m[strings.TrimSpace(h)] = rec[i]
				}
			}
			rows = append(rows, m)
		}
		out = append(out, rows)
	}
	return out, nil
}

// SyntheticID is the stable dedupe key of a transaction row that came without a connector id:
// a hash of the normalized raw fields (kind, account, date, amount, name, merchant, description)
// plus the row's occurrence number among identical rows of the same page file (1, 2, ...).
// Re-reading the same file, or an overlapping page that repeats the row, gives the same key, so
// staging (upsert by id) stays a no-op; two genuinely identical charges in one statement (two
// airline tickets) get occurrence 1 and 2 and both survive.
func SyntheticID(kind string, r map[string]string, occurrence int) string {
	return fmt.Sprintf("h-%s-%d", syntheticBase(kind, r), occurrence)
}

func syntheticBase(kind string, r map[string]string) string {
	acct := r["account_id"]
	if acct == "" {
		acct = r["liability_id"]
	}
	parts := []string{kind, strings.TrimSpace(acct), strings.TrimSpace(r["date"]), NormAmount(r["amount"]),
		NormText(r["name"]), NormText(r["merchant_name"]), NormText(r["description"])}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])[:24]
}

// assignSyntheticIDs gives every id-less row of one page file its SyntheticID (in place; the id
// is also kept in the raw row, flagged id_synthetic, so the staged JSON reproduces it).
func assignSyntheticIDs(kind string, rows []map[string]string) {
	n := map[string]int{}
	for _, r := range rows {
		if strings.TrimSpace(r["id"]) != "" {
			continue
		}
		b := syntheticBase(kind, r)
		n[b]++
		r["id"] = fmt.Sprintf("h-%s-%d", b, n[b])
		r["id_synthetic"] = "true"
	}
}

// NormAmount normalizes a decimal string ("-1095.1300" -> "-1095.13", "+5.0" -> "5").
func NormAmount(s string) string {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "+"))
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" || s == "" {
		s = "0"
	}
	return s
}

// NormText lowercases and collapses whitespace (for matching descriptions across pulls).
func NormText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// RawJSON returns a stable JSON encoding of a CSV row (keys sorted by encoding/json).
func RawJSON(m map[string]string) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// TxnFromRow converts a CSV row (as read, or as stored verbatim in raw_transactions) to a Txn.
func TxnFromRow(kind string, r map[string]string) Txn {
	acct := r["account_id"]
	if acct == "" {
		acct = r["liability_id"]
	}
	t := Txn{ID: r["id"], AccountID: acct, Kind: kind, Date: r["date"], Amount: r["amount"],
		Direction: r["direction"], Currency: r["currency"], Name: r["name"], Merchant: r["merchant_name"],
		Description: r["description"], Pending: r["pending"] == "true", Raw: r}
	if c := r["category"]; c != "" {
		var cat struct{ Primary, Detailed string }
		if json.Unmarshal([]byte(c), &cat) == nil {
			t.CategoryPrimary, t.CategoryDetailed = cat.Primary, cat.Detailed
		}
	}
	return t
}

func PortfolioFromRow(r map[string]string) PortfolioTxn {
	return PortfolioTxn{ID: r["id"], AccountID: r["portfolio_id"], Date: r["date"],
		Type: r["type"], Ticker: r["ticker_symbol"], SecurityName: r["security_name"], Amount: r["amount"],
		Price: r["price"], Quantity: r["quantity"], Fees: r["fees"], Currency: r["currency"], Raw: r}
}

func HoldingFromRow(r map[string]string) Holding {
	return Holding{ID: r["id"], AccountID: r["portfolio_id"], Ticker: r["ticker_symbol"],
		SecurityName: r["security_name"], SecurityType: r["security_type"], Quantity: r["quantity"],
		CostBasis: r["cost_basis"], Price: r["institution_price"], Value: r["institution_value"], Currency: r["currency"], Raw: r}
}
