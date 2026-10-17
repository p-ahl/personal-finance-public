package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/p-ahl/personal-finance/internal/download/common"
	"github.com/p-ahl/personal-finance/internal/download/kaspa"
	"github.com/p-ahl/personal-finance/internal/download/kraken"
	"github.com/p-ahl/personal-finance/internal/download/wallets"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return common.ExitConfig
	}
	switch args[0] {
	case "download":
		return runDownload(args[1:])
	case "db":
		return runDB(args[1:])
	case "import":
		return runImport(args[1:])
	case "categorize":
		return runCategorize(args[1:])
	case "transfers":
		return runTransfers(args[1:])
	case "check":
		return runCheck(args[1:])
	case "report":
		return runReport(args[1:])
	case "sql":
		return runSQL(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return common.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		printUsage()
		return common.ExitConfig
	}
}

func runDownload(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: finance download <fi>")
		return common.ExitConfig
	}
	fi := strings.ToLower(args[0])
	switch fi {
	case "kaspa":
		return kaspa.Run(context.Background(), kaspa.Options{})
	case "kraken":
		return runDownloadKraken(args[1:])
	case "wallets":
		return runDownloadWallets(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown institution: %s\n", fi)
		fmt.Fprintln(os.Stderr, "known: kaspa | kraken | wallets")
		return common.ExitConfig
	}
}

func runDownloadKraken(args []string) int {
	fs := flag.NewFlagSet("download kraken", flag.ContinueOnError)
	full := fs.Bool("full", false, "fetch the whole ledger/trade history (default: incremental from the newest earlier pull, 14-day overlap)")
	date := fs.String("date", "", "folder date YYYY-MM-DD (default: today in America/Denver)")
	data := fs.String("data", dataDir(), "data dir ($FINANCE_DATA)")
	if err := fs.Parse(args); err != nil {
		return common.ExitConfig
	}
	opt := kraken.Options{DataDir: *data, Full: *full}
	if *date != "" {
		d, err := time.Parse("2006-01-02", *date)
		if err != nil {
			fmt.Fprintln(os.Stderr, "--date:", err)
			return common.ExitConfig
		}
		opt.Day = d
	}
	return kraken.Run(context.Background(), opt)
}

func runDownloadWallets(args []string) int {
	fs := flag.NewFlagSet("download wallets", flag.ContinueOnError)
	date := fs.String("date", "", "folder date YYYY-MM-DD (default: today in America/Denver)")
	data := fs.String("data", dataDir(), "data dir ($FINANCE_DATA)")
	cfg := fs.String("config", "", "wallets config (default $FINANCE_DATA/config/wallets.yaml)")
	hist := fs.Bool("history", false, "also save every used address's full transaction history (history/)")
	if err := fs.Parse(args); err != nil {
		return common.ExitConfig
	}
	opt := wallets.Options{DataDir: *data, Config: *cfg, History: *hist}
	if *date != "" {
		d, err := time.Parse("2006-01-02", *date)
		if err != nil {
			fmt.Fprintln(os.Stderr, "--date:", err)
			return common.ExitConfig
		}
		opt.Day = d
	}
	return wallets.Run(context.Background(), opt)
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `finance — personal finance CLI (SQLite double-entry books + downloaders)

Books (SQLite is the source of truth; see docs/schema.md):
  finance db migrate                         Create/upgrade the schema
  finance import connector --raw <dir>       Stage an extract (cumulative, dedup by connector
                                             txn id) and rebuild the journal (idempotent)
  finance categorize                         Re-apply category rules from staging
  finance transfers match [-v]               Re-run transfer matching from staging; print pairs
  finance check                              Zero-sum, reconciliation, unmatched, uncategorized
                                             (exit 1 on hard failures)

Reports:  finance report <name> [flags]
  bs | balance-sheet        --as-of D                 balance sheet (accrual)
  is | income-statement     --from D --to D [--monthly] [--detail] [--basis accrual|cash]
  cf | cash-flow            --from D --to D [--monthly]
  tb | trial-balance        --as-of D
  networth | balances       --as-of D                 net worth by account and basis
  payables                  --as-of D                 open payables / accrued / receivables /
                                                      prepaids with aging buckets
  accrual-vs-cash           --from D --to D           P&L by account on both bases
  project                   --project CODE [--as-of D] [--budget FILE]
                                                      cost by cost code, unpaid, budget/forecast
  spending                  --from D --to D           expenses by category per month
  uncategorized             --from D --to D [--by-counterparty] [--limit N]
  coverage                  --from D --to D           categorization coverage of expense $
  reconcile                                           ledger vs newest connector balances
  openings                                            opening-balance entries
  unmatched                                           transfers parked in the clearing account
  every report: --format text|md|csv; --from defaults to 12 months before --to (today)

Ad-hoc SQL (read-only; see finance sql --help):
  finance sql "SELECT ..."                   or SQL on stdin, or -f file.sql
      --format table|csv|json|md   --param k=v   --attach name=path (also $FINANCE_ATTACH)
  finance sql --tables | --schema <name>     list tables/views; show a definition
  finance sql --queries | --q <name>         saved queries from queries/ (recent, month-spend,
                                             payables, uncategorized, reconcile, staging)

  Common flags: --db (default $FINANCE_DB or $FINANCE_DATA/finance.db; FINANCE_DATA defaults
                to ../finance-data)  --repo (default .)  --local (default $FINANCE_DATA/config)

Nightly refresh: scripts/refresh.sh <raw extract dir>   (see README "Nightly refresh")

Downloaders:
  finance download kaspa              Fetch Kaspa txs/balances → imports/YYYY-MM-DD/kaspa/
  finance download kraken [--full] [--date D]
                                      Kraken read-only API (Balance, BalanceEx, Ledgers,
                                      TradesHistory + public Ticker/OHLC) saved verbatim to
                                      $FINANCE_DATA/raw/kraken/D/ (0600). Needs KRAKEN_API_KEY /
                                      KRAKEN_API_SECRET: run via "fnox exec --"
  finance import kraken --raw <dir>   Stage a Kraken pull (cumulative) and rebuild the journal
  finance download wallets [--history] [--date D]
                                      Self-custody wallet balances from public explorers (Kaspa
                                      addresses + kpub-derived receive/change addresses, BTC, LTC,
                                      Solana + SPL tokens; Monero from a Cake Wallet CSV) and USD
                                      prices, saved verbatim to $FINANCE_DATA/raw/wallets/D/ (0600).
                                      Addresses/keys come from $FINANCE_DATA/config/wallets.yaml
  finance import wallets --raw <dir>  Stage a wallet pull (+ events.csv) and rebuild the journal
  finance import coinbase --raw <dir> Stage a Coinbase pull (MCP connector responses saved to
                                      $FINANCE_DATA/raw/coinbase/D/ + events.csv) and rebuild

Config: config/wallets.yaml (gitignored; see wallets.example.yaml)

Exit codes: 0 ok, 1 failure (check failures / stub), 2 usage, 3 download failure
`)
}
