package wallets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestRun(t *testing.T, root, day string) *run {
	dir := filepath.Join(root, day)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := newFetcher()
	f.delay = 0
	f.hc.Timeout = 2 * time.Second
	return &run{ctx: context.Background(), f: f, dir: dir}
}

func TestLitecoinFallbackBlockCypher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/balance") {
			w.Write([]byte(`{"total_received":5000,"total_sent":3766,"balance":1234,"n_tx":4,"unconfirmed_n_tx":0}`))
			return
		}
		http.Error(w, "down", 404)
	}))
	defer srv.Close()
	oldL, oldF := LTCAPIs, LTCFallback
	LTCAPIs, LTCFallback = []string{srv.URL + "/primary"}, srv.URL
	defer func() { LTCAPIs, LTCFallback = oldL, oldF }()
	r := newTestRun(t, t.TempDir(), "2026-01-02")
	bal, used, _, err := r.esplora("W_litecoin", Chain{Chain: "litecoin", Address: "x"})
	if err != nil || bal != "0.00001234" || !used {
		t.Fatalf("got %q %v %v", bal, used, err)
	}
}

func TestOptionalCarryForward(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "2026-01-01")
	os.MkdirAll(old, 0o700)
	os.WriteFile(filepath.Join(old, "W_litecoin_address.json"),
		[]byte(`{"chain_stats":{"funded_txo_sum":1234,"spent_txo_sum":0,"tx_count":1},"mempool_stats":{"tx_count":0}}`), 0o600)
	r := newTestRun(t, root, "2026-01-02")
	cause := os.ErrDeadlineExceeded
	if bal, _, _, _, err := r.carryForward("W_litecoin", Chain{Chain: "litecoin", Optional: true, DustMax: "0.001"}, cause); err != nil || bal != "0.00001234" {
		t.Fatalf("dust: %q %v", bal, err)
	}
	r = newTestRun(t, root, "2026-01-03")
	if _, _, _, _, err := r.carryForward("W_litecoin", Chain{Chain: "litecoin", Optional: true}, cause); err == nil {
		t.Fatal("balance above default dust_max must stay an error")
	}
	if _, err := os.Stat(filepath.Join(r.dir, "W_litecoin_address.json")); err == nil {
		t.Fatal("rejected carry-forward must not leave a file")
	}
}
