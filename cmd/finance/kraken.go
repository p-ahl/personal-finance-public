package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/p-ahl/personal-finance/internal/download/common"
	"github.com/p-ahl/personal-finance/internal/ledger"
)

// finance import kraken --raw $FINANCE_DATA/raw/kraken/<date>
func runImportKraken(args []string) int {
	fs := flag.NewFlagSet("import kraken", flag.ContinueOnError)
	p := addPathFlags(fs)
	raw := fs.String("raw", "", "Kraken pull directory (raw/kraken/YYYY-MM-DD)")
	if err := fs.Parse(args); err != nil {
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
	db, err := p.open()
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	s, err := ledger.ImportKraken(db, cfg, *raw)
	if err != nil {
		return fail(err)
	}
	x := s.Exchange
	fmt.Printf("import run %d: kraken pull %s (books as of %s)\n", s.RunID, x.AsOf, s.AsOf)
	fmt.Printf("  staged:  ledger %d new, %d changed, %d unchanged; %d trades; %d balances; %d daily closes, %d ticker prices\n",
		x.LedgerNew, x.LedgerChanged, x.LedgerUnchanged, x.Trades, x.Balances, x.Closes, x.Ticks)
	kinds := map[string]int{}
	for _, pr := range s.Result.Pairs {
		if pr.Hint == "kraken" {
			kinds[pr.Kind]++
		}
	}
	fmt.Printf("  matched: %d kraken USD transfers to bank rows (same-day %d, in-transit %d)\n", kinds["same-day"]+kinds["in-transit"], kinds["same-day"], kinds["in-transit"])
	for _, n := range s.Result.Notes {
		fmt.Printf("  note:    %s\n", n)
	}
	printBuild(s)
	return 0
}
