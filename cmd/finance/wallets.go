package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/p-ahl/personal-finance/internal/download/common"
	"github.com/p-ahl/personal-finance/internal/ledger"
	"github.com/p-ahl/personal-finance/internal/money"
)

// finance import wallets --raw $FINANCE_DATA/raw/wallets/<date> [--wallets-config wallets.yaml]
func runImportWallets(args []string) int {
	fs := flag.NewFlagSet("import wallets", flag.ContinueOnError)
	p := addPathFlags(fs)
	raw := fs.String("raw", "", "wallet pull directory (raw/wallets/YYYY-MM-DD)")
	wcfg := fs.String("wallets-config", "", "wallets config with the identifiers to store in the DB (default <local>/wallets.yaml)")
	if err := fs.Parse(args); err != nil {
		return common.ExitConfig
	}
	if *raw == "" {
		fmt.Fprintln(os.Stderr, "--raw is required")
		return common.ExitConfig
	}
	if *wcfg == "" {
		*wcfg = filepath.Join(p.local, "wallets.yaml")
		if _, err := os.Stat(*wcfg); err != nil {
			*wcfg = ""
		}
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
	s, err := ledger.ImportWallets(db, cfg, *raw, *wcfg)
	if err != nil {
		return fail(err)
	}
	w := s.Wallets
	fmt.Printf("import run %d: wallet pull %s (books as of %s)\n", s.RunID, w.AsOf, s.AsOf)
	var kinds []string
	for k := range w.EventKinds {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	ks := ""
	for _, k := range kinds {
		ks += fmt.Sprintf(" %s %d,", k, w.EventKinds[k])
	}
	fmt.Printf("  staged:  %d wallets, %d identifiers, %d balances, %d prices, %d events (%s)\n", w.Wallets, w.Identifiers, w.Snapshots, w.Prices, w.Events, ks)

	// realized gains on wallet swaps by year (estimates), and the per-event replay
	type yr struct {
		n                           int
		cost, proceeds, st, lt, fee int64
		booked                      int
	}
	years := map[string]*yr{}
	out := filepath.Join(*raw, "events-booked.csv")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	cw := csv.NewWriter(f)
	cw.Write([]string{"id", "date", "kind", "booked", "cost", "proceeds", "gain_short", "gain_long", "fee", "note"})
	c := func(v int64) string { return money.Format(v, 2) }
	for _, r := range s.Result.WalletResults {
		cw.Write([]string{r.ID, r.Date, r.Kind, strconv.FormatBool(r.Booked), c(r.Cost), c(r.Proceeds), c(r.ShortTerm), c(r.LongTerm), c(r.Fee), r.Note})
		if r.Kind != "swap" {
			continue
		}
		y := r.Date[:4]
		if years[y] == nil {
			years[y] = &yr{}
		}
		a := years[y]
		a.n++
		a.cost += r.Cost
		a.proceeds += r.Proceeds
		a.st += r.ShortTerm
		a.lt += r.LongTerm
		a.fee += r.Fee
		if r.Booked {
			a.booked++
		}
	}
	cw.Flush()
	f.Close()
	if err := cw.Error(); err != nil {
		return fail(err)
	}
	var ys []string
	for y := range years {
		ys = append(ys, y)
	}
	sort.Strings(ys)
	fmt.Println("  wallet swaps, realized gain/loss by year (ESTIMATES: daily prices, FIFO, matched by timing/amount):")
	for _, y := range ys {
		a := years[y]
		fmt.Printf("    %s: %d swaps (%d in the books), proceeds %s, cost %s, short-term %s, long-term %s, total %s; network fees %s\n",
			y, a.n, a.booked, usdS(a.proceeds), usdS(a.cost), usdS(a.st), usdS(a.lt), usdS(a.st+a.lt), usdS(a.fee))
	}
	fmt.Printf("  per-event replay: %s\n", out)
	for _, n := range s.Result.Notes {
		fmt.Printf("  note:    %s\n", n)
	}
	printBuild(s)
	return 0
}

func usdS(v int64) string { return "$" + money.Format(v, 2) }
