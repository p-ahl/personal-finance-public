package kaspa

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const DefaultBaseURL = "https://api.kaspa.org"

// Client talks to the public Kaspa explorer REST API (api.kaspa.org).
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewClient() *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		HTTPClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// BalanceResponse is GET /addresses/{addr}/balance.
// Balance is in sompi (1 KAS = 1e8 sompi). Never invent — only echo API.
type BalanceResponse struct {
	Address string `json:"address"`
	Balance int64  `json:"balance"`
}

// UTXO is one unspent output from GET /addresses/{addr}/utxos.
type UTXO struct {
	Address   string `json:"address"`
	Outpoint  any    `json:"outpoint"`
	UTXOEntry any    `json:"utxoEntry"`
}

// Transaction is one element from full-transactions-page.
type Transaction map[string]any

// GetBalance fetches the confirmed balance for address.
func (c *Client) GetBalance(ctx context.Context, address string) (*BalanceResponse, error) {
	u := fmt.Sprintf("%s/addresses/%s/balance", c.BaseURL, url.PathEscape(address))
	var out BalanceResponse
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetUTXOs fetches UTXOs for address.
func (c *Client) GetUTXOs(ctx context.Context, address string) ([]UTXO, error) {
	u := fmt.Sprintf("%s/addresses/%s/utxos", c.BaseURL, url.PathEscape(address))
	var out []UTXO
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []UTXO{}
	}
	return out, nil
}

// GetFullTransactionsPage fetches one page of full transactions.
// before is epoch-millis; pass 0 for the newest page.
// Returns txs and nextBefore from X-Next-Page-Before (0 if no more pages).
func (c *Client) GetFullTransactionsPage(ctx context.Context, address string, limit int, before int64) ([]Transaction, int64, error) {
	if limit <= 0 {
		limit = 50
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("resolve_previous_outpoints", "light")
	if before > 0 {
		q.Set("before", strconv.FormatInt(before, 10))
	}
	u := fmt.Sprintf("%s/addresses/%s/full-transactions-page?%s", c.BaseURL, url.PathEscape(address), q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("GET %s: status %d: %s", u, resp.StatusCode, truncate(string(body), 200))
	}
	var txs []Transaction
	if err := json.Unmarshal(body, &txs); err != nil {
		return nil, 0, fmt.Errorf("decode transactions: %w", err)
	}
	if txs == nil {
		txs = []Transaction{}
	}
	var nextBefore int64
	if h := resp.Header.Get("X-Next-Page-Before"); h != "" {
		nextBefore, _ = strconv.ParseInt(h, 10, 64)
	}
	return txs, nextBefore, nil
}

// GetAllTransactions pages through full-transactions-page until exhausted.
// Caps at maxPages to avoid runaway rate-limit hits (0 = default 40).
func (c *Client) GetAllTransactions(ctx context.Context, address string, pageLimit, maxPages int) ([]Transaction, error) {
	if pageLimit <= 0 {
		pageLimit = 50
	}
	if maxPages <= 0 {
		maxPages = 40
	}
	var all []Transaction
	var before int64
	for page := 0; page < maxPages; page++ {
		txs, next, err := c.GetFullTransactionsPage(ctx, address, pageLimit, before)
		if err != nil {
			return nil, err
		}
		all = append(all, txs...)
		if next == 0 || len(txs) == 0 {
			break
		}
		// Avoid infinite loop if API keeps returning same cursor.
		if before != 0 && next >= before {
			break
		}
		before = next
	}
	return all, nil
}

func (c *Client) getJSON(ctx context.Context, u string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d: %s", u, resp.StatusCode, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return fmt.Errorf("decode %s: %w", u, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
