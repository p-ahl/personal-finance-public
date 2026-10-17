package kraken

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/p-ahl/personal-finance/internal/download/common"
)

// Options controls one download run.
type Options struct {
	DataDir string    // $FINANCE_DATA; raw files go to <DataDir>/raw/kraken/<YYYY-MM-DD>/
	Day     time.Time // folder date (default: today in America/Denver)
	Full    bool      // fetch the whole ledger/trade history even when earlier pulls exist
	// Overlap re-fetched before the newest staged ledger time on incremental runs.
	Overlap time.Duration
	Client  *Client
	Out     io.Writer
	Log     io.Writer
}

// PageSize is Kraken's fixed page size for Ledgers and TradesHistory.
const PageSize = 50

// Run downloads everything for one day and returns a common exit code.
func Run(ctx context.Context, opt Options) int {
	if opt.Out == nil {
		opt.Out = os.Stdout
	}
	if opt.Log == nil {
		opt.Log = os.Stderr
	}
	if opt.Client == nil {
		c, err := NewClient(os.Getenv("KRAKEN_API_KEY"), os.Getenv("KRAKEN_API_SECRET"))
		if err != nil {
			fmt.Fprintln(opt.Log, "config error:", err)
			return common.ExitConfig
		}
		opt.Client = c
	}
	if opt.DataDir == "" {
		fmt.Fprintln(opt.Log, "config error: data dir required ($FINANCE_DATA)")
		return common.ExitConfig
	}
	if opt.Day.IsZero() {
		opt.Day = Today()
	}
	if opt.Overlap == 0 {
		opt.Overlap = 14 * 24 * time.Hour
	}
	root := filepath.Join(opt.DataDir, "raw", "kraken")
	dir := filepath.Join(root, opt.Day.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintln(opt.Log, "download error:", err)
		return common.ExitDownloadFail
	}
	_ = os.Chmod(filepath.Join(opt.DataDir, "raw"), 0o700)
	_ = os.Chmod(root, 0o700)
	_ = os.Chmod(dir, 0o700)
	d := &dl{ctx: ctx, c: opt.Client, dir: dir, out: opt.Out, written: map[string]bool{}, started: time.Now()}

	start := int64(0)
	window := "full history"
	if !opt.Full {
		if last, from, ok := newestLedgerTime(root, dir); ok {
			start = last - int64(opt.Overlap/time.Second)
			window = fmt.Sprintf("incremental: start=%d (newest staged ledger time %d in %s minus %s overlap)", start, last, from, opt.Overlap)
		}
	}
	d.window = window
	fmt.Fprintf(opt.Out, "kraken download -> %s (%s)\n", dir, window)

	// reference data (public)
	d.public("Assets", nil, "Assets.json")
	d.public("AssetPairs", nil, "AssetPairs.json")

	// balances
	d.private("Balance", nil, "Balance.json")
	d.private("BalanceEx", nil, "BalanceEx.json")

	// ledger and trades, paged with ofs
	ledgerAssets := map[string]bool{}
	d.paged("Ledgers", "ledger", start, "Ledgers", func(res json.RawMessage) {
		var r struct {
			Ledger map[string]struct {
				Asset string `json:"asset"`
			} `json:"ledger"`
		}
		if json.Unmarshal(res, &r) == nil {
			for _, e := range r.Ledger {
				ledgerAssets[e.Asset] = true
			}
		}
	})
	d.paged("TradesHistory", "trades", start, "TradesHistory", nil)

	// USD prices for every asset held or seen (public Ticker + daily OHLC)
	held := map[string]bool{}
	if b, err := os.ReadFile(filepath.Join(dir, "Balance.json")); err == nil {
		var env struct {
			Result map[string]string `json:"result"`
		}
		if json.Unmarshal(b, &env) == nil {
			for a := range env.Result {
				held[a] = true
			}
		}
	}
	// assets already staged by earlier pulls also need prices for history valuation
	for a := range assetsInEarlierPulls(root, dir) {
		ledgerAssets[a] = true
	}
	pairs, perr := loadPairs(dir)
	if perr != nil {
		d.fail("AssetPairs", perr)
	} else {
		want := map[string]bool{}
		for a := range held {
			want[a] = true
		}
		for a := range ledgerAssets {
			want[a] = true
		}
		var ps []string
		seen := map[string]bool{}
		for a := range want {
			p := pairs.USDPair(a)
			if p == "" {
				if !IsUSD(a) {
					d.notes = append(d.notes, fmt.Sprintf("no USD pair for asset %s", a))
				}
				continue
			}
			if !seen[p] {
				seen[p] = true
				ps = append(ps, p)
			}
		}
		sort.Strings(ps)
		if len(ps) > 0 {
			d.public("Ticker", url.Values{"pair": {strings.Join(ps, ",")}}, "Ticker.json")
			for _, p := range ps {
				d.public("OHLC", url.Values{"pair": {p}, "interval": {"1440"}}, "OHLC_"+p+".json")
			}
		}
	}

	if err := d.finish(); err != nil {
		fmt.Fprintln(opt.Log, "download error:", err)
		return common.ExitDownloadFail
	}
	if len(d.errs) > 0 {
		for _, e := range d.errs {
			fmt.Fprintln(opt.Log, "error:", e)
		}
		return common.ExitDownloadFail
	}
	return common.ExitOK
}

// Today is the current date in America/Denver (the books' time zone).
func Today() time.Time {
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		return time.Now()
	}
	n := time.Now().In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

type call struct {
	Method, Params, File string
	Bytes                int
	Count                string
	Err                  string
	Unchanged            bool
}

type dl struct {
	ctx     context.Context
	c       *Client
	dir     string
	out     io.Writer
	calls   []call
	errs    []string
	notes   []string
	written map[string]bool
	window  string
	started time.Time
}

func (d *dl) fail(method string, err error) {
	d.errs = append(d.errs, err.Error())
	d.calls = append(d.calls, call{Method: method, Err: err.Error()})
}

func (d *dl) save(file string, body []byte) (bool, error) {
	d.written[file] = true
	return writeVerbatim(filepath.Join(d.dir, file), body)
}

func (d *dl) record(method string, params url.Values, file string, body []byte, err error, count string) bool {
	c := call{Method: method, Params: redactParams(params), File: file, Bytes: len(body), Count: count}
	if body != nil {
		unchanged, werr := d.save(file, body)
		if werr != nil {
			err = errors.Join(err, werr)
		}
		c.Unchanged = unchanged
	}
	if err != nil {
		c.Err = err.Error()
		d.errs = append(d.errs, err.Error())
	}
	d.calls = append(d.calls, c)
	status := "ok"
	if c.Unchanged {
		status = "unchanged"
	}
	if err != nil {
		status = "ERROR: " + err.Error()
	}
	extra := ""
	if count != "" {
		extra = " " + count
	}
	fmt.Fprintf(d.out, "  %-14s %-28s %8d bytes%s  %s\n", method, file, len(body), extra, status)
	return err == nil
}

func (d *dl) public(method string, params url.Values, file string) bool {
	body, err := d.c.Public(d.ctx, method, params)
	return d.record(method, params, file, body, err, "")
}

func (d *dl) private(method string, params url.Values, file string) bool {
	body, err := d.c.Private(d.ctx, method, params)
	return d.record(method, params, file, body, err, "")
}

// paged walks ofs = 0, 50, 100, ... until every row reported by "count" has been fetched.
func (d *dl) paged(method, key string, start int64, prefix string, each func(json.RawMessage)) {
	ofs := 0
	for page := 1; ; page++ {
		params := url.Values{"ofs": {strconv.Itoa(ofs)}}
		if start > 0 {
			params.Set("start", strconv.FormatInt(start, 10))
		}
		if method == "TradesHistory" {
			params.Set("type", "all")
		}
		body, err := d.c.Private(d.ctx, method, params)
		file := fmt.Sprintf("%s_page%03d.json", prefix, page)
		n, total := 0, 0
		if err == nil {
			res, _ := CheckBody(method, body)
			var r map[string]json.RawMessage
			if jerr := json.Unmarshal(res, &r); jerr != nil {
				err = fmt.Errorf("kraken %s: bad result: %w", method, jerr)
			} else {
				var rows map[string]json.RawMessage
				_ = json.Unmarshal(r[key], &rows)
				n = len(rows)
				_ = json.Unmarshal(r["count"], &total)
				if each != nil {
					each(res)
				}
			}
		}
		if !d.record(method, params, file, body, err, fmt.Sprintf("rows=%d count=%d", n, total)) {
			return
		}
		ofs += n
		if n == 0 || ofs >= total {
			return
		}
		if page > 2000 {
			d.errs = append(d.errs, method+": more than 2000 pages; stopping")
			return
		}
	}
}

// finish removes files of an earlier run of the same day that this run did not produce,
// then writes MANIFEST.md and SHA256SUMS.
func (d *dl) finish() error {
	ents, err := os.ReadDir(d.dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || n == "MANIFEST.md" || n == "SHA256SUMS" || d.written[n] || !strings.HasSuffix(n, ".json") {
			continue
		}
		if len(d.errs) == 0 { // keep the previous good files when this run was incomplete
			if err := os.Remove(filepath.Join(d.dir, n)); err != nil {
				return err
			}
		}
	}
	var m strings.Builder
	loc, _ := time.LoadLocation("America/Denver")
	if loc == nil {
		loc = time.Local
	}
	fmt.Fprintf(&m, "# Kraken raw pull %s\n\n", filepath.Base(d.dir))
	fmt.Fprintf(&m, "- fetched: %s (America/Denver)\n", d.started.In(loc).Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&m, "- source: %s (read-only private: Balance, BalanceEx, Ledgers, TradesHistory; public: Assets, AssetPairs, Ticker, OHLC)\n", d.c.BaseURL)
	fmt.Fprintf(&m, "- window: %s\n", d.window)
	fmt.Fprintf(&m, "- files are response bodies saved byte-for-byte; never edit them\n\n")
	fmt.Fprintf(&m, "| method | params | file | bytes | rows | result |\n|---|---|---|---|---|---|\n")
	for _, c := range d.calls {
		res := "ok"
		if c.Unchanged {
			res = "ok (unchanged)"
		}
		if c.Err != "" {
			res = "ERROR: " + c.Err
		}
		fmt.Fprintf(&m, "| %s | %s | %s | %d | %s | %s |\n", c.Method, c.Params, c.File, c.Bytes, c.Count, res)
	}
	for _, n := range d.notes {
		fmt.Fprintf(&m, "\n- note: %s", n)
	}
	m.WriteString("\n")
	if _, err := writeVerbatim(filepath.Join(d.dir, "MANIFEST.md"), []byte(m.String())); err != nil {
		return err
	}
	// SHA256SUMS over the json files
	ents, err = os.ReadDir(d.dir)
	if err != nil {
		return err
	}
	var sums strings.Builder
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d.dir, e.Name()))
		if err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%s  %s\n", common.ChecksumHex(b), e.Name())
	}
	if _, err := writeVerbatim(filepath.Join(d.dir, "SHA256SUMS"), []byte(sums.String())); err != nil {
		return err
	}
	fmt.Fprintf(d.out, "done: %d calls, %d errors -> %s\n", len(d.calls), len(d.errs), d.dir)
	return nil
}

func redactParams(p url.Values) string {
	if len(p) == 0 {
		return ""
	}
	q := url.Values{}
	for k, v := range p {
		if k == "nonce" || strings.EqualFold(k, "otp") {
			continue
		}
		q[k] = v
	}
	return q.Encode()
}

// writeVerbatim writes body to path (mode 0600) unless the file already holds exactly these
// bytes (idempotent re-runs). Writes are atomic (temp file + rename).
func writeVerbatim(path string, body []byte) (unchanged bool, err error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(body) {
		_ = os.Chmod(path, 0o600)
		return true, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return false, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return false, os.Rename(name, path)
}

// newestLedgerTime finds the newest ledger entry time in earlier day folders (not `skip`).
func newestLedgerTime(root, skip string) (int64, string, bool) {
	dirs, _ := filepath.Glob(filepath.Join(root, "[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]"))
	var best float64
	from := ""
	for _, dir := range dirs {
		if dir == skip {
			continue
		}
		pages, _ := filepath.Glob(filepath.Join(dir, "Ledgers_page*.json"))
		if len(pages) == 0 {
			continue
		}
		for _, p := range pages {
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			ents, err := ParseLedgerPage(b)
			if err != nil {
				continue
			}
			for _, e := range ents {
				if e.Time > best {
					best, from = e.Time, filepath.Base(dir)
				}
			}
		}
	}
	if best == 0 {
		return 0, "", false
	}
	return int64(math.Floor(best)), from, true
}

func assetsInEarlierPulls(root, skip string) map[string]bool {
	out := map[string]bool{}
	pages, _ := filepath.Glob(filepath.Join(root, "*", "Ledgers_page*.json"))
	for _, p := range pages {
		if filepath.Dir(p) == skip {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		ents, err := ParseLedgerPage(b)
		if err != nil {
			continue
		}
		for _, e := range ents {
			out[e.Asset] = true
		}
	}
	return out
}

func loadPairs(dir string) (*Pairs, error) {
	pb, err := os.ReadFile(filepath.Join(dir, "AssetPairs.json"))
	if err != nil {
		return nil, err
	}
	ab, _ := os.ReadFile(filepath.Join(dir, "Assets.json"))
	return ParsePairs(pb, ab)
}
