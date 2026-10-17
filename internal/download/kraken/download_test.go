package kraken

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p-ahl/personal-finance/internal/download/common"
	"github.com/p-ahl/personal-finance/internal/testfixture"
)

// fakeKraken serves the synthetic fixture, with a 60-row ledger split by ofs into 50 + 10.
type fakeKraken struct {
	mu    sync.Mutex
	calls []string
	deny  map[string]bool
}

func ledgerRows(n int) string {
	var parts []string
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf(`"LFAKE-%03d":{"aclass":"currency","amount":"1.0000","asset":"ZUSD","balance":"%d.0000","fee":"0.0000","refid":"R%03d","time":%d.5,"type":"deposit","subtype":""}`,
			i, i+1, i, 1767000000+i*60))
	}
	return strings.Join(parts, ",")
}

func (f *fakeKraken) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		method := filepath.Base(r.URL.Path)
		var params url.Values
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			params, _ = url.ParseQuery(string(b))
			if r.Header.Get("API-Key") != "FAKEKEY" || r.Header.Get("API-Sign") == "" {
				t.Errorf("%s: unsigned private call", method)
			}
		} else {
			params = r.URL.Query()
		}
		f.mu.Lock()
		f.calls = append(f.calls, method+"?"+params.Get("ofs")+params.Get("pair"))
		f.mu.Unlock()
		if f.deny[method] {
			io.WriteString(w, `{"error":["EGeneral:Permission denied"]}`)
			return
		}
		switch method {
		case "Assets":
			io.WriteString(w, testfixture.KrakenAssetsJSON)
		case "AssetPairs":
			io.WriteString(w, testfixture.KrakenAssetPairsJSON)
		case "Balance":
			io.WriteString(w, testfixture.KrakenBalanceJSON)
		case "BalanceEx":
			io.WriteString(w, `{"error":[],"result":{"ZUSD":{"balance":"29.2800","hold_trade":"0.0000"}}}`)
		case "Ledgers":
			ofs, _ := strconv.Atoi(params.Get("ofs"))
			n := 50
			if ofs >= 50 {
				n = 10
			}
			body := `{"error":[],"result":{"ledger":{` + ledgerRows(n) + `},"count":60}}`
			if ofs >= 60 {
				body = `{"error":[],"result":{"ledger":{},"count":60}}`
			}
			io.WriteString(w, body)
		case "TradesHistory":
			if params.Get("type") != "all" {
				t.Errorf("TradesHistory without type=all")
			}
			io.WriteString(w, testfixture.KrakenTradesJSON)
		case "Ticker":
			io.WriteString(w, testfixture.KrakenTickerJSON)
		case "OHLC":
			io.WriteString(w, testfixture.KrakenOHLCJSON(params.Get("pair")))
		default:
			t.Errorf("unexpected method %s", method)
			io.WriteString(w, `{"error":["EGeneral:Unknown method"]}`)
		}
	}
}

func runFake(t *testing.T, f *fakeKraken, data string) (int, string) {
	t.Helper()
	c := testClient(t, f.handler(t))
	var out, log bytes.Buffer
	code := Run(context.Background(), Options{DataDir: data, Day: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), Client: c, Out: &out, Log: &log})
	return code, out.String() + log.String()
}

func TestDownloadSavesVerbatimPrivateFiles(t *testing.T) {
	data := t.TempDir()
	f := &fakeKraken{}
	code, out := runFake(t, f, data)
	if code != common.ExitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	dir := filepath.Join(data, "raw", "kraken", "2026-02-01")
	want := []string{"Assets.json", "AssetPairs.json", "Balance.json", "BalanceEx.json", "Ledgers_page001.json",
		"Ledgers_page002.json", "TradesHistory_page001.json", "Ticker.json", "OHLC_KASUSD.json", "OHLC_XXBTZUSD.json",
		"MANIFEST.md", "SHA256SUMS"}
	for _, n := range want {
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("missing %s: %v", n, err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", n, st.Mode().Perm())
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "Balance.json"))
	if string(b) != testfixture.KrakenBalanceJSON {
		t.Error("Balance.json is not byte-for-byte the response body")
	}
	p2, _ := os.ReadFile(filepath.Join(dir, "Ledgers_page002.json"))
	if rows, err := ParseLedgerPage(p2); err != nil || len(rows) != 10 {
		t.Fatalf("page 2: %d rows %v", len(rows), err)
	}
	// paging used ofs=0 then ofs=50 and stopped at count
	var ofs []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "Ledgers?") {
			ofs = append(ofs, strings.TrimPrefix(c, "Ledgers?"))
		}
	}
	if strings.Join(ofs, ",") != "0,50" {
		t.Fatalf("ledger paging offsets %v", ofs)
	}
	// nothing secret in the files or output
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if bytes.Contains(b, []byte("FAKEKEY")) || bytes.Contains(b, []byte(fakeSecret())) {
			t.Fatalf("%s contains credentials", e.Name())
		}
	}
	if strings.Contains(out, "FAKEKEY") || strings.Contains(out, fakeSecret()) {
		t.Fatal("output contains credentials")
	}
	sums, _ := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	tk, _ := os.ReadFile(filepath.Join(dir, "Ticker.json"))
	if !strings.Contains(string(sums), common.ChecksumHex(tk)+"  Ticker.json") {
		t.Fatal("SHA256SUMS incomplete")
	}

	// a second run on the same day rewrites nothing (data files unchanged) and asks for an
	// incremental window starting before the newest staged ledger time
	before := map[string]time.Time{}
	for _, e := range ents {
		st, _ := os.Stat(filepath.Join(dir, e.Name()))
		before[e.Name()] = st.ModTime()
	}
	f2 := &fakeKraken{}
	if code, out := runFake(t, f2, data); code != common.ExitOK {
		t.Fatalf("second run exit %d\n%s", code, out)
	}
	for n, mt := range before {
		if !strings.HasSuffix(n, ".json") {
			continue
		}
		st, _ := os.Stat(filepath.Join(dir, n))
		if !st.ModTime().Equal(mt) {
			t.Errorf("%s rewritten on identical re-run", n)
		}
	}
}

func TestDownloadReportsMissingPermission(t *testing.T) {
	data := t.TempDir()
	f := &fakeKraken{deny: map[string]bool{"Ledgers": true}}
	code, out := runFake(t, f, data)
	if code == common.ExitOK {
		t.Fatal("want failure when Ledgers is denied")
	}
	if !strings.Contains(out, "Query Ledger Entries") {
		t.Fatalf("output does not name the missing permission:\n%s", out)
	}
}
