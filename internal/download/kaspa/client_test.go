package kaspa

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetBalanceAndUTXOs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/addresses/kaspa:qqtest/balance", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(BalanceResponse{Address: "kaspa:qqtest", Balance: 150000000})
	})
	mux.HandleFunc("/addresses/kaspa:qqtest/utxos", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]UTXO{{Address: "kaspa:qqtest"}})
	})
	mux.HandleFunc("/addresses/kaspa:qqtest/full-transactions-page", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]Transaction{{"transaction_id": "abc"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient()
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()

	bal, err := c.GetBalance(context.Background(), "kaspa:qqtest")
	if err != nil || bal.Balance != 150000000 {
		t.Fatalf("balance: %+v err=%v", bal, err)
	}
	utxos, err := c.GetUTXOs(context.Background(), "kaspa:qqtest")
	if err != nil || len(utxos) != 1 {
		t.Fatalf("utxos: %v err=%v", utxos, err)
	}
	txs, next, err := c.GetFullTransactionsPage(context.Background(), "kaspa:qqtest", 50, 0)
	if err != nil || len(txs) != 1 || next != 0 {
		t.Fatalf("txs=%v next=%d err=%v", txs, next, err)
	}
}

func TestSompiToKAS(t *testing.T) {
	cases := map[int64]string{
		0:           "0",
		100000000:   "1",
		150000000:   "1.5",
		92463303393038: "924633.03393038",
	}
	for in, want := range cases {
		if got := sompiToKAS(in); got != want {
			t.Errorf("sompiToKAS(%d)=%q want %q", in, got, want)
		}
	}
}
