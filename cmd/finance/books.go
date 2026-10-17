package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/p-ahl/personal-finance/internal/books"
	"github.com/p-ahl/personal-finance/internal/connector"
	"github.com/p-ahl/personal-finance/internal/download/common"
	"github.com/p-ahl/personal-finance/internal/ledger"
	"github.com/p-ahl/personal-finance/internal/money"
	"github.com/p-ahl/personal-finance/internal/report"
)

// paths shared by the books subcommands.
type paths struct {
	db, repo, local string
}

func dataDir() string {
	if d := os.Getenv("FINANCE_DATA"); d != "" {
		return d
	}
	return filepath.Join("..", "finance-data") // sibling of the repo checkout
}

func addPathFlags(fs *flag.FlagSet) *paths {
	p := &paths{}
	fs.StringVar(&p.db, "db", envOr("FINANCE_DB", filepath.Join(dataDir(), "finance.db")), "SQLite database (data side, never committed)")
	fs.StringVar(&p.repo, "repo", envOr("FINANCE_REPO", "."), "repo root (config/coa.yaml, rules/*.example.yaml)")
	fs.StringVar(&p.local, "local", envOr("FINANCE_LOCAL_CONFIG", filepath.Join(dataDir(), "config")), "local config dir (accounts.yaml, rules.local.yaml, rules.personal.yaml, transfers.personal.yaml, coa.local.yaml, accruals.yaml, splits.local.yaml)")
	return p
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// localOr returns the personal file in the local config dir when it exists, else the generic
// example shipped in the repo. Personal rule sets REPLACE the example (they are complete files).
func localOr(local, name, example string) string {
	if f := filepath.Join(local, name); fileExists(f) {
		return f
	}
	return example
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func (p *paths) config() (*books.Config, error) {
	cfg, err := books.LoadConfig(
		filepath.Join(p.repo, "config", "coa.yaml"),
		filepath.Join(p.local, "accounts.yaml"),
		// categorization / transfer rules: $FINANCE_DATA/config/rules.personal.yaml and
		// transfers.personal.yaml (real merchants, masks) replace the generic rules/*.example.yaml
		localOr(p.local, "rules.personal.yaml", filepath.Join(p.repo, "rules", "categories.example.yaml")),
		filepath.Join(p.local, "rules.local.yaml"),
		localOr(p.local, "transfers.personal.yaml", filepath.Join(p.repo, "rules", "transfers.example.yaml")),
	)
	if err != nil {
		return nil, err
	}
	// optional personal chart-of-accounts overlay (data-side): extra accounts / defaults
	if err := cfg.LoadCOAOverlay(filepath.Join(p.local, "coa.local.yaml")); err != nil {
		return nil, err
	}
	// optional accrual layer (data-side): payables, prepaids, tax-year accruals, loans, documents
	if err := cfg.LoadAccruals(filepath.Join(p.local, "accruals.yaml")); err != nil {
		return nil, err
	}
	// optional item-level splits (data-side): one charge -> several itemized postings
	if err := cfg.LoadSplits(filepath.Join(p.local, "splits.local.yaml")); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (p *paths) open() (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(p.db), 0o700); err != nil {
		return nil, err
	}
	return ledger.Open(p.db)
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}

func runDB(args []string) int {
	if len(args) > 0 && args[0] == "pin-opening" {
		return runPinOpening(args[1:])
	}
	if len(args) == 0 || args[0] != "migrate" {
		fmt.Fprintln(os.Stderr, "usage: finance db migrate [--db path] | finance db pin-opening --account NAME --amount 0.00 --note EVIDENCE")
		return common.ExitConfig
	}
	fs := flag.NewFlagSet("db migrate", flag.ContinueOnError)
	p := addPathFlags(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return common.ExitConfig
	}
	db, err := p.open()
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	applied, err := ledger.Migrate(db)
	if err != nil {
		return fail(err)
	}
	if len(applied) == 0 {
		fmt.Println("schema up to date:", p.db)
	}
	for _, m := range applied {
		fmt.Println("applied", m)
	}
	return 0
}

// runPinOpening corrects the pinned opening of a transaction-tracked connector account (the
// note records the evidence) and rebuilds the journal.
func runPinOpening(args []string) int {
	fs := flag.NewFlagSet("db pin-opening", flag.ContinueOnError)
	p := addPathFlags(fs)
	account := fs.String("account", "", "ledger account name (a transaction-tracked connector account)")
	amount := fs.String("amount", "", "opening balance, debit-positive (e.g. 0.00)")
	note := fs.String("note", "", "evidence for the amount (required)")
	if err := fs.Parse(args); err != nil {
		return common.ExitConfig
	}
	if *account == "" || *amount == "" || *note == "" {
		fmt.Fprintln(os.Stderr, "usage: finance db pin-opening --account NAME --amount 0.00 --note EVIDENCE")
		return common.ExitConfig
	}
	amt, err := money.ParseMinor(*amount, 2)
	if err != nil {
		return fail(err)
	}
	cfg, err := p.config()
	if err != nil {
		return fail(err)
	}
	db, err := p.open()
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	s, err := ledger.PinOpening(db, cfg, *account, amt, *note)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("pinned opening %s = %s; rebuilt (as of %s)\n", *account, report.FormatUSD(amt), s.AsOf)
	printBuild(s)
	return 0
}

func runImport(args []string) int {
	if len(args) > 0 && args[0] == "kraken" {
		return runImportKraken(args[1:])
	}
	if len(args) > 0 && args[0] == "coinbase" {
		return runImportCoinbase(args[1:])
	}
	if len(args) > 0 && args[0] == "wallets" {
		return runImportWallets(args[1:])
	}
	if len(args) == 0 || args[0] != "connector" {
		fmt.Fprintln(os.Stderr, "usage: finance import connector --raw <extract dir> [--as-of YYYY-MM-DD] | finance import kraken --raw <pull dir> | finance import coinbase --raw <pull dir> | finance import wallets --raw <pull dir>")
		return common.ExitConfig
	}
	fs := flag.NewFlagSet("import connector", flag.ContinueOnError)
	p := addPathFlags(fs)
	raw := fs.String("raw", "", "extract directory (connector CSV/JSON pages)")
	asOf := fs.String("as-of", "", "balance date of the extract (default: directory name)")
	if err := fs.Parse(args[1:]); err != nil {
		return common.ExitConfig
	}
	if *raw == "" {
		fmt.Fprintln(os.Stderr, "--raw is required")
		return common.ExitConfig
	}
	cfg, err := p.config()
	if err != nil {
		return fail(err)
	}
	ex, err := connector.Read(*raw)
	if err != nil {
		return fail(err)
	}
	if *asOf != "" {
		ex.AsOf = *asOf
	}
	db, err := p.open()
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	s, err := ledger.Import(db, cfg, ex, *raw)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("import run %d (as of %s)\n", s.RunID, s.AsOf)
	fmt.Printf("  staged:  %d new, %d changed, %d unchanged raw rows (%d pending kept in staging only); %d balance snapshots, %d holdings\n",
		s.Stage.RawNew, s.Stage.RawChanged, s.Stage.RawUnchanged, s.Stage.Pending, s.Stage.Snapshots, s.Stage.Holdings)
	if s.Stage.UnseenPosted > 0 || s.Stage.UnseenPending > 0 {
		fmt.Printf("  unseen:  %d posted (REVIEW: removed or re-issued upstream?), %d pending (superseded) staged rows inside this extract's date span are not in it; kept in staging\n",
			s.Stage.UnseenPosted, s.Stage.UnseenPending)
	}
	if s.Stage.Superseded > 0 {
		fmt.Printf("  re-keyed: %d staged row(s) re-issued upstream under a new id; the old copy is superseded (kept in staging, not booked)\n", s.Stage.Superseded)
	}
	printBuild(s)
	return 0
}

func printBuild(s *ledger.Summary) {
	b := s.Build
	fmt.Printf("  booked:  %d posted rows, %d portfolio rows -> %d entries\n", b.PostedRows, b.PortfolioRows, len(s.Result.Entries))
	fmt.Printf("  journal: %d inserted, %d updated, %d unchanged, %d deleted; %d accounts\n", s.Sync.Inserted, s.Sync.Updated, s.Sync.Unchanged, s.Sync.Deleted, s.Sync.Accounts)
	if len(s.OpeningDrift) > 0 {
		var parts []string
		for _, d := range s.OpeningDrift {
			parts = append(parts, fmt.Sprintf("%s (%s)", d.Account, report.FormatUSD(d.New-d.Old)))
		}
		fmt.Printf("  openings: %d connector account(s) changed opening balance (REVIEW: missing/duplicate history or pending in the balance?): %s\n",
			len(s.OpeningDrift), strings.Join(parts, "; "))
	}
	// reported balance vs pinned opening + posted activity, booked to a pending account
	var expected, unexplained []string
	for _, g := range s.Result.Pending {
		part := fmt.Sprintf("%s %s -> %s as of %s", g.Account, report.FormatUSD(g.Amount), g.Pending, g.AsOf)
		if g.Configured {
			expected = append(expected, part)
		} else {
			unexplained = append(unexplained, part)
		}
	}
	if len(expected) > 0 {
		fmt.Printf("  pending: %d account(s) with activity not posted yet (clears when it posts): %s\n", len(expected), strings.Join(expected, "; "))
	}
	if len(unexplained) > 0 {
		fmt.Printf("  unreconciled: %d account(s) whose balance does not follow from the pinned opening + history (REVIEW: missing/duplicate history?): %s\n",
			len(unexplained), strings.Join(unexplained, "; "))
	}
}

func rebuild(args []string, name string) (*ledger.Summary, *paths, int) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	p := addPathFlags(fs)
	if err := fs.Parse(args); err != nil {
		return nil, nil, common.ExitConfig
	}
	cfg, err := p.config()
	if err != nil {
		return nil, nil, fail(err)
	}
	db, err := p.open()
	if err != nil {
		return nil, nil, fail(err)
	}
	defer db.Close()
	s, err := ledger.Rebuild(db, cfg, name)
	if err != nil {
		return nil, nil, fail(err)
	}
	return s, p, 0
}

func runCategorize(args []string) int {
	s, _, code := rebuild(args, "categorize")
	if s == nil {
		return code
	}
	b := s.Build
	fmt.Printf("re-categorized from staging (as of %s)\n", s.AsOf)
	fmt.Printf("  rows: %d by rule, %d by Plaid fallback, %d uncategorized, %d to unmatched transfers\n", b.ByRule, b.ByPlaid, b.Uncategorized, b.Unmatched)
	if b.Split > 0 {
		fmt.Printf("  splits:  %d row(s) itemized from splits.local.yaml\n", b.Split)
	}
	printBuild(s)
	return 0
}

func runTransfers(args []string) int {
	if len(args) == 0 || args[0] != "match" {
		fmt.Fprintln(os.Stderr, "usage: finance transfers match [--verbose]")
		return common.ExitConfig
	}
	verbose := false
	var rest []string
	for _, a := range args[1:] {
		if a == "--verbose" || a == "-v" {
			verbose = true
		} else {
			rest = append(rest, a)
		}
	}
	s, _, code := rebuild(rest, "transfers")
	if s == nil {
		return code
	}
	b := s.Build
	kinds := map[string]int{}
	hints := map[string]int{}
	for _, p := range s.Result.Pairs {
		kinds[p.Kind]++
		hints[p.Hint]++
	}
	fmt.Printf("transfer matching (as of %s)\n", s.AsOf)
	fmt.Printf("  pairs: %d (same-day %d, in-transit %d), funding pairs %d; one-sided to internal accounts %d; unmatched %d\n",
		b.Pairs, kinds["same-day"], kinds["in-transit"], b.FundingPairs, b.OneSided, b.Unmatched)
	var hs []string
	for h, n := range hints {
		hs = append(hs, fmt.Sprintf("%s=%d", h, n))
	}
	sort.Strings(hs)
	fmt.Println("  by hint:", strings.Join(hs, " "))
	if verbose {
		for _, p := range s.Result.Pairs {
			fmt.Printf("    %-10s %-22s gap=%dd %12s  %s + %s\n", p.Kind, p.Hint, p.GapDays, report.FormatUSD(p.AmountAbs), p.A, p.B)
		}
	}
	printBuild(s)
	return 0
}

func runCheck(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	p := addPathFlags(fs)
	format := fs.String("format", "text", "text | md | csv | json (json: classified statements only)")
	if err := fs.Parse(args); err != nil {
		return common.ExitConfig
	}
	cfg, err := p.config()
	if err != nil {
		return fail(err)
	}
	db, err := p.open()
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	// optional local allowlist of cross-source pairs confirmed to be distinct events
	reviewed, err := report.LoadReviewed(filepath.Join(p.local, "dedupe.yaml"))
	if err != nil {
		return fail(err)
	}
	n, err := report.Check(db, os.Stdout, *format, cfg.Transfers.Unmatched, cfg.Transfers.InTransit, reviewed)
	if err != nil {
		return fail(err)
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "check: %d failure(s)\n", n)
		return 1
	}
	return 0
}

func runReport(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: finance report balance-sheet|income-statement|cash-flow|statements|bs|is|cf|tb|payables|accrual-vs-cash|uncategorized|networth|reconcile|spending|coverage|openings|unmatched|project [flags]")
		return common.ExitConfig
	}
	kind := args[0]
	fs := flag.NewFlagSet("report "+kind, flag.ContinueOnError)
	p := addPathFlags(fs)
	today := time.Now().Format("2006-01-02")
	asOf := fs.String("as-of", today, "as-of date (balance sheet, trial balance, net worth)")
	from := fs.String("from", "", "period start YYYY-MM-DD (default: 12 months before --to)")
	to := fs.String("to", today, "period end YYYY-MM-DD (inclusive)")
	monthly := fs.Bool("monthly", false, "one column per month (is, cf)")
	detail := fs.Bool("detail", false, "full account names instead of categories (is)")
	limit := fs.Int("limit", 50, "max rows (uncategorized)")
	grouped := fs.Bool("by-counterparty", false, "group uncategorized by counterparty")
	format := fs.String("format", "text", "text | md | csv | json (json: classified statements only)")
	basis := fs.String("basis", "accrual", "accrual | cash (is)")
	project := fs.String("project", "", "project dimension code (project), e.g. the code used in project:<code> tags")
	budget := fs.String("budget", "", "budgets file (project; default <local>/budgets.yaml)")
	period := fs.String("period", "month", "column period for balance-sheet / income-statement / cash-flow / statements: month | quarter | year | none")
	extra := fs.String("extra", "", "extra summary columns (income-statement, cash-flow, statements): comma list of ytd, ttm, total")
	dates := fs.String("dates", "", "explicit as-of dates for balance-sheet columns (comma list; overrides --period)")
	labelsFile := fs.String("labels", "", "plain labels for accounts (YAML labels: {account: label}; default <local>/labels.yaml if present)")
	if err := fs.Parse(args[1:]); err != nil {
		return common.ExitConfig
	}
	if *from == "" {
		t, _ := time.Parse("2006-01-02", *to)
		*from = t.AddDate(-1, 0, 1).Format("2006-01-02")
	}
	db, err := p.open()
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	if kind == "project" {
		if *project == "" && fs.NArg() > 0 {
			*project = fs.Arg(0)
		}
		if *project == "" {
			fmt.Fprintln(os.Stderr, "usage: finance report project --project <code> [--as-of D] [--budget file]")
			return common.ExitConfig
		}
		if *budget == "" {
			*budget = filepath.Join(p.local, "budgets.yaml")
		}
		pb, err := report.LoadProjectBudget(*budget)
		if err != nil {
			return fail(err)
		}
		ts, err := report.Project(db, *project, *asOf, pb)
		if err != nil {
			return fail(err)
		}
		for i, t := range ts {
			if i > 0 {
				fmt.Println()
			}
			if err := t.Render(os.Stdout, *format); err != nil {
				return fail(err)
			}
		}
		return 0
	}
	switch kind {
	case "balance-sheet", "income-statement", "cash-flow", "statements":
		if !setFlag(fs, "from") {
			*from = ""
		}
		if !setFlag(fs, "to") && setFlag(fs, "as-of") {
			*to = *asOf
		}
		if *labelsFile == "" {
			*labelsFile = filepath.Join(p.local, "labels.yaml")
		}
		return runStatements(db, kind, *from, *to, *period, *extra, *dates, *labelsFile, *format)
	}
	var t *report.Table
	switch kind {
	case "bs":
		t, err = report.BalanceSheet(db, *asOf)
	case "is":
		t, err = report.IncomeStatement(db, *from, *to, *monthly, *detail, *basis)
	case "payables":
		t, err = report.Payables(db, *asOf)
	case "accrual-vs-cash":
		t, err = report.AccrualVsCash(db, *from, *to)
	case "cf":
		t, err = report.CashFlow(db, *from, *to, *monthly)
	case "tb", "trial-balance":
		t, err = report.TrialBalance(db, *asOf)
	case "uncategorized":
		t, err = report.Uncategorized(db, *from, *to, *limit, *grouped)
	case "networth", "balances":
		t, err = report.NetWorth(db, *asOf)
	case "reconcile":
		t, _, err = report.Reconcile(db)
	case "spending":
		t, err = report.Spending(db, *from, *to)
	case "coverage":
		t, _, _, err = report.Coverage(db, *from, *to)
	case "openings":
		t, err = report.Openings(db)
	case "unmatched":
		cfg, cerr := p.config()
		if cerr != nil {
			return fail(cerr)
		}
		t, _, _, err = report.Unmatched(db, cfg.Transfers.Unmatched)
	default:
		fmt.Fprintln(os.Stderr, "unknown report:", kind)
		return common.ExitConfig
	}
	if err != nil {
		return fail(err)
	}
	if err := t.Render(os.Stdout, *format); err != nil {
		return fail(err)
	}
	return 0
}
