// Package kraken downloads read-only account data from the Kraken REST API and saves every
// response body verbatim. Only the read-only endpoints in allowedPrivate may be called; the
// client refuses anything else (trading, funding, withdrawal, staking mutations).
package kraken

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const DefaultBaseURL = "https://api.kraken.com"

// allowedPrivate is the complete list of private methods this package may call. All of them
// are queries ("Query Funds" / "Query Ledger Entries" / "Query Closed Orders & Trades").
var allowedPrivate = map[string]bool{
	"Balance":       true,
	"BalanceEx":     true,
	"Ledgers":       true,
	"TradesHistory": true,
}

// allowedPublic are the public market-data methods used for USD prices and asset metadata.
var allowedPublic = map[string]bool{
	"Assets":     true,
	"AssetPairs": true,
	"Ticker":     true,
	"OHLC":       true,
}

// PermissionHint names the API-key permission each private method needs.
var PermissionHint = map[string]string{
	"Balance":       "Query Funds",
	"BalanceEx":     "Query Funds",
	"Ledgers":       "Query Ledger Entries",
	"TradesHistory": "Query Closed Orders & Trades",
}

// Client is a minimal Kraken REST client. Key and Secret are never logged or returned in errors.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	key        string
	secret     []byte // decoded private key
	// MinInterval spaces private calls to stay under the API counter (Ledgers/TradesHistory
	// cost 2 points; the starter tier decays 0.33 points per second).
	MinInterval time.Duration
	// MaxRetries for "EAPI:Rate limit exceeded" / "EAPI:Invalid nonce" / 5xx.
	MaxRetries int
	Sleep      func(time.Duration)
	Now        func() time.Time

	mu        sync.Mutex
	lastNonce int64
	lastCall  time.Time
}

// NewClient builds a client from the API key and the base64 private key.
func NewClient(key, secretB64 string) (*Client, error) {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(secretB64) == "" {
		return nil, errors.New("KRAKEN_API_KEY and KRAKEN_API_SECRET must be set (run via `fnox exec --`)")
	}
	sec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(secretB64))
	if err != nil {
		return nil, errors.New("KRAKEN_API_SECRET is not valid base64")
	}
	return &Client{
		BaseURL:     DefaultBaseURL,
		HTTPClient:  &http.Client{Timeout: 60 * time.Second},
		key:         strings.TrimSpace(key),
		secret:      sec,
		MinInterval: 3 * time.Second,
		MaxRetries:  6,
		Sleep:       time.Sleep,
		Now:         time.Now,
	}, nil
}

// Sign returns the API-Sign header value:
// base64(HMAC-SHA512(urlPath + SHA256(nonce + postData), secret)).
func Sign(urlPath, nonce, postData string, secret []byte) string {
	sha := sha256.Sum256([]byte(nonce + postData))
	mac := hmac.New(sha512.New, secret)
	mac.Write([]byte(urlPath))
	mac.Write(sha[:])
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// SignB64 is Sign with the secret still base64-encoded (as Kraken hands it out).
func SignB64(urlPath, nonce, postData, secretB64 string) (string, error) {
	sec, err := base64.StdEncoding.DecodeString(secretB64)
	if err != nil {
		return "", err
	}
	return Sign(urlPath, nonce, postData, sec), nil
}

// APIError is a non-empty "error" array in a Kraken response.
type APIError struct {
	Method   string
	Messages []string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("kraken %s: %s", e.Method, strings.Join(e.Messages, "; "))
	if e.Permission() {
		if p := PermissionHint[e.Method]; p != "" {
			msg += fmt.Sprintf(" (the API key needs the %q permission)", p)
		}
	}
	return msg
}

// Permission reports whether the error is a missing key permission.
func (e *APIError) Permission() bool {
	for _, m := range e.Messages {
		if strings.Contains(m, "Permission denied") {
			return true
		}
	}
	return false
}

func (e *APIError) retryable() bool {
	for _, m := range e.Messages {
		if strings.Contains(m, "Rate limit") || strings.Contains(m, "Invalid nonce") ||
			strings.Contains(m, "EService:Unavailable") || strings.Contains(m, "EService:Busy") ||
			strings.Contains(m, "Too many requests") {
			return true
		}
	}
	return false
}

type envelope struct {
	Error  []string        `json:"error"`
	Result json.RawMessage `json:"result"`
}

// CheckBody parses the response envelope and returns an *APIError if Kraken reported one.
func CheckBody(method string, body []byte) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("kraken %s: bad JSON: %w", method, err)
	}
	if len(env.Error) > 0 {
		return nil, &APIError{Method: method, Messages: env.Error}
	}
	return env.Result, nil
}

func (c *Client) nonce() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.Now().UnixMilli()
	if n <= c.lastNonce {
		n = c.lastNonce + 1
	}
	c.lastNonce = n
	return fmt.Sprintf("%d", n)
}

func (c *Client) pace() {
	if c.MinInterval <= 0 {
		return
	}
	if !c.lastCall.IsZero() {
		if d := c.MinInterval - c.Now().Sub(c.lastCall); d > 0 {
			c.Sleep(d)
		}
	}
	c.lastCall = c.Now()
}

// Private calls a read-only private method and returns the raw response body (verbatim) once
// Kraken reports no error. The body is returned even when err is an *APIError.
func (c *Client) Private(ctx context.Context, method string, params url.Values) ([]byte, error) {
	if !allowedPrivate[method] {
		return nil, fmt.Errorf("kraken: private method %q is not on the read-only allowlist", method)
	}
	path := "/0/private/" + method
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			c.Sleep(time.Duration(5*attempt) * time.Second)
		}
		c.pace()
		form := url.Values{}
		for k, v := range params {
			form[k] = v
		}
		n := c.nonce()
		form.Set("nonce", n)
		post := form.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, strings.NewReader(post))
		if err != nil {
			return nil, err
		}
		req.Header.Set("API-Key", c.key)
		req.Header.Set("API-Sign", Sign(path, n, post, c.secret))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		req.Header.Set("User-Agent", "personal-finance/kraken-download")
		body, status, err := c.do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if status >= 500 || status == 429 {
			lastErr = fmt.Errorf("kraken %s: HTTP %d", method, status)
			continue
		}
		if _, err := CheckBody(method, body); err != nil {
			var ae *APIError
			if errors.As(err, &ae) && ae.retryable() {
				lastErr = err
				continue
			}
			return body, err
		}
		return body, nil
	}
	return nil, lastErr
}

// Public calls a public market-data method (GET) and returns the raw body.
func (c *Client) Public(ctx context.Context, method string, params url.Values) ([]byte, error) {
	if !allowedPublic[method] {
		return nil, fmt.Errorf("kraken: public method %q is not allowed", method)
	}
	u := c.BaseURL + "/0/public/" + method
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			c.Sleep(time.Duration(2*attempt) * time.Second)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "personal-finance/kraken-download")
		body, status, err := c.do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if status >= 500 || status == 429 {
			lastErr = fmt.Errorf("kraken %s: HTTP %d", method, status)
			continue
		}
		if _, err := CheckBody(method, body); err != nil {
			var ae *APIError
			if errors.As(err, &ae) && ae.retryable() {
				lastErr = err
				continue
			}
			return body, err
		}
		return body, nil
	}
	return nil, lastErr
}

func (c *Client) do(req *http.Request) ([]byte, int, error) {
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		// never include the request (headers carry the key) in the error
		return nil, 0, fmt.Errorf("kraken: request failed: %s", redactURLError(err))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("kraken: read body: %w", err)
	}
	return b, resp.StatusCode, nil
}

func redactURLError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + " " + ue.URL + ": " + ue.Err.Error()
	}
	return err.Error()
}
