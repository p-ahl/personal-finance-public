package kraken

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Example from Kraken's REST authentication docs (a published, non-functional key).
const (
	docSecret   = "kQH5HW/8p1uGOVjbgWA7FunAmGO8lsSUXNsu3eow76sz84Q18fWxnyRzBHCd3pd5nE9qa99HAZtuZuj6F1huXg=="
	docNonce    = "1616492376594"
	docPostData = "nonce=1616492376594&ordertype=limit&pair=XBTUSD&price=37500&type=buy&volume=1.25"
	docPath     = "/0/private/AddOrder"
	docSig      = "4/dpxb3iT4tp/ZCVEwSnEsLxx0bqyhLpdfOpc6fn7OR8+UClSV5n9E6aSS8MPtnRfp32bAb0nmbRn6H8ndwLUQ=="
)

func TestSignDocVector(t *testing.T) {
	got, err := SignB64(docPath, docNonce, docPostData, docSecret)
	if err != nil {
		t.Fatal(err)
	}
	if got != docSig {
		t.Fatalf("signature mismatch:\n got %s\nwant %s", got, docSig)
	}
}

func TestSignChangesWithInputs(t *testing.T) {
	sec, _ := base64.StdEncoding.DecodeString(docSecret)
	base := Sign("/0/private/Balance", "1", "nonce=1", sec)
	for _, alt := range []string{
		Sign("/0/private/Ledgers", "1", "nonce=1", sec),
		Sign("/0/private/Balance", "2", "nonce=1", sec),
		Sign("/0/private/Balance", "1", "nonce=2", sec),
		Sign("/0/private/Balance", "1", "nonce=1", []byte("other")),
	} {
		if alt == base {
			t.Fatal("signature did not change with its inputs")
		}
	}
}

func fakeSecret() string {
	return base64.StdEncoding.EncodeToString([]byte("not-a-real-secret-0123456789"))
}

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient("FAKEKEY", fakeSecret())
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = srv.URL
	c.MinInterval = 0
	c.Sleep = func(time.Duration) {}
	return c
}

func TestPrivateRequestIsSigned(t *testing.T) {
	var gotPath, gotKey, gotSign, gotBody string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotKey, gotSign, gotBody = r.URL.Path, r.Header.Get("API-Key"), r.Header.Get("API-Sign"), string(b)
		io.WriteString(w, `{"error":[],"result":{"ZUSD":"1.0000"}}`)
	})
	body, err := c.Private(context.Background(), "Balance", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"error":[],"result":{"ZUSD":"1.0000"}}` {
		t.Fatalf("body not verbatim: %s", body)
	}
	if gotPath != "/0/private/Balance" {
		t.Fatalf("path %s", gotPath)
	}
	if gotKey != "FAKEKEY" {
		t.Fatal("API-Key header missing")
	}
	form, _ := url.ParseQuery(gotBody)
	n := form.Get("nonce")
	if n == "" {
		t.Fatal("nonce missing")
	}
	sec, _ := base64.StdEncoding.DecodeString(fakeSecret())
	if want := Sign("/0/private/Balance", n, gotBody, sec); gotSign != want {
		t.Fatal("API-Sign does not verify")
	}
}

func TestNonceStrictlyIncreasing(t *testing.T) {
	c, _ := NewClient("k", fakeSecret())
	fixed := time.UnixMilli(1700000000000)
	c.Now = func() time.Time { return fixed }
	a, b := c.nonce(), c.nonce()
	if !(a < b) || a != "1700000000000" {
		t.Fatalf("nonces %s %s", a, b)
	}
}

func TestOnlyReadOnlyMethods(t *testing.T) {
	called := false
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	for _, m := range []string{"AddOrder", "CancelOrder", "Withdraw", "WithdrawCancel", "DepositAddresses",
		"WalletTransfer", "Stake", "Unstake", "Earn/Allocate", "CreateSubaccount", "AccountTransfer"} {
		if _, err := c.Private(context.Background(), m, nil); err == nil || !strings.Contains(err.Error(), "allowlist") {
			t.Fatalf("%s: expected allowlist refusal, got %v", m, err)
		}
	}
	if called {
		t.Fatal("a non-allowlisted method reached the server")
	}
	for m := range allowedPrivate {
		if PermissionHint[m] == "" {
			t.Fatalf("%s has no permission hint", m)
		}
	}
}

func TestPermissionErrorNamesPermission(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"error":["EGeneral:Permission denied"]}`)
	})
	body, err := c.Private(context.Background(), "Ledgers", url.Values{"ofs": {"0"}})
	var ae *APIError
	if !errors.As(err, &ae) || !ae.Permission() {
		t.Fatalf("want permission error, got %v", err)
	}
	if !strings.Contains(err.Error(), "Query Ledger Entries") {
		t.Fatalf("error does not name the permission: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("error body should still be returned for the manifest")
	}
}

func TestRateLimitRetried(t *testing.T) {
	n := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			io.WriteString(w, `{"error":["EAPI:Rate limit exceeded"]}`)
			return
		}
		io.WriteString(w, `{"error":[],"result":{}}`)
	})
	if _, err := c.Private(context.Background(), "Balance", nil); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("calls = %d", n)
	}
}

func TestErrorsNeverContainSecrets(t *testing.T) {
	c, _ := NewClient("FAKEKEY", fakeSecret())
	c.BaseURL = "http://127.0.0.1:1" // refused
	c.MaxRetries = 0
	c.Sleep = func(time.Duration) {}
	_, err := c.Private(context.Background(), "Balance", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "FAKEKEY") || strings.Contains(err.Error(), fakeSecret()) {
		t.Fatalf("error leaks credentials: %v", err)
	}
	if _, err := NewClient("", ""); err == nil {
		t.Fatal("empty credentials accepted")
	}
	if _, err := NewClient("k", "%%%"); err == nil || strings.Contains(err.Error(), "%%%") {
		t.Fatalf("bad secret: %v", err)
	}
}
