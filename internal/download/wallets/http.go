package wallets

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// fetcher does polite GET/POST requests against public, read-only endpoints with retry on
// 429/5xx. Errors never include request bodies (they may contain addresses).
type fetcher struct {
	hc    *http.Client
	delay time.Duration
	last  map[string]time.Time
}

func newFetcher() *fetcher {
	return &fetcher{hc: &http.Client{Timeout: 60 * time.Second}, delay: 250 * time.Millisecond, last: map[string]time.Time{}}
}

func (f *fetcher) do(ctx context.Context, method, url string, body []byte, label string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		host := req.URL.Host
		delay := f.delay
		if strings.Contains(host, "solana") {
			delay = 1500 * time.Millisecond // public RPC: ~40 requests / 10 s per IP, stricter for getTransaction
		}
		if t, ok := f.last[host]; ok {
			if w := delay - time.Since(t); w > 0 {
				time.Sleep(w)
			}
		}
		f.last[host] = time.Now()
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "personal-finance-wallets/1 (read-only)")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := f.hc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s: %s request failed", label, host)
		} else {
			b, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
			switch {
			case rerr != nil:
				lastErr = fmt.Errorf("%s: %s read: %v", label, host, rerr)
			case resp.StatusCode == 200:
				return b, nil
			case resp.StatusCode == 429 || resp.StatusCode >= 500:
				lastErr = fmt.Errorf("%s: %s HTTP %d", label, host, resp.StatusCode)
			default:
				return nil, fmt.Errorf("%s: %s HTTP %d", label, host, resp.StatusCode)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		}
	}
	return nil, lastErr
}
