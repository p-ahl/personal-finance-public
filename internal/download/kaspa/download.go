package kaspa

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/p-ahl/personal-finance/internal/config"
	"github.com/p-ahl/personal-finance/internal/download/common"
)

// Snapshot is the on-disk JSON artifact for one wallet address.
// Balances and txs come only from the live API — never invented.
type Snapshot struct {
	FetchedAt    time.Time     `json:"fetched_at"`
	Source       string        `json:"source"`
	WalletName   string        `json:"wallet_name"`
	Address      string        `json:"address"`
	BalanceSompi int64         `json:"balance_sompi"`
	BalanceKAS   string        `json:"balance_kas"` // display helper only; derived from sompi
	UTXOCount    int           `json:"utxo_count"`
	UTXOs        []UTXO        `json:"utxos"`
	TxCount      int           `json:"tx_count"`
	Transactions []Transaction `json:"transactions"`
}

// Options controls a download run.
type Options struct {
	RepoRoot   string
	ConfigPath string
	Day        time.Time
	Client     *Client
	// MaxTxPages caps pagination (default 40 ≈ up to ~2000 txs).
	MaxTxPages int
}

// Run loads wallets.yaml, fetches each explicit address, writes under
// imports/YYYY-MM-DD/kaspa/, prints a summary. Returns a common exit code.
func Run(ctx context.Context, opt Options) int {
	if opt.RepoRoot == "" {
		root, err := common.RepoRoot("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "config error: %v\n", err)
			return common.ExitConfig
		}
		opt.RepoRoot = root
	}
	if opt.ConfigPath == "" {
		opt.ConfigPath = filepath.Join(opt.RepoRoot, "config", "wallets.yaml")
	}
	if opt.Client == nil {
		opt.Client = NewClient()
	}
	if opt.Day.IsZero() {
		opt.Day = time.Now()
	}
	if opt.MaxTxPages <= 0 {
		opt.MaxTxPages = 40
	}

	cfg, err := config.Load(opt.ConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		fmt.Fprintf(os.Stderr, "hint: copy config/wallets.example.yaml to config/wallets.yaml\n")
		return common.ExitConfig
	}

	outDir := common.ImportDir(opt.RepoRoot, "kaspa", opt.Day)
	if err := common.EnsureDir(outDir); err != nil {
		fmt.Fprintf(os.Stderr, "download error: %v\n", err)
		return common.ExitDownloadFail
	}

	for _, w := range cfg.Kaspa.Wallets {
		if w.Address == "" {
			reason := "no explicit address"
			if w.KPub != "" {
				reason = "kpub-only (Phase 1 does not derive addresses from kpub)"
			}
			fmt.Printf("skip %s: %s\n", w.Name, reason)
		}
	}

	wallets := cfg.Kaspa.FetchableAddresses()
	if len(wallets) == 0 {
		fmt.Fprintf(os.Stderr, "config error: no kaspa wallets with explicit address in %s\n", opt.ConfigPath)
		return common.ExitConfig
	}

	fmt.Printf("kaspa download → %s\n", outDir)
	var wrote, skipped int
	for _, w := range wallets {
		snap, err := fetchWallet(ctx, opt.Client, w, opt.MaxTxPages)
		if err != nil {
			fmt.Fprintf(os.Stderr, "download error (%s): %v\n", w.Name, err)
			return common.ExitDownloadFail
		}
		fname := sanitizeFilename(w.Name) + ".json"
		path := filepath.Join(outDir, fname)

		if sameContent(path, snap) {
			skipped++
			fmt.Printf("  %s  balance=%s KAS  utxos=%d  txs=%d  [unchanged]\n",
				w.Name, snap.BalanceKAS, snap.UTXOCount, snap.TxCount)
			continue
		}

		if _, err := common.AtomicWriteJSON(path, snap); err != nil {
			fmt.Fprintf(os.Stderr, "download error: write %s: %v\n", path, err)
			return common.ExitDownloadFail
		}
		wrote++
		fmt.Printf("  %s  balance=%s KAS  utxos=%d  txs=%d  → %s\n",
			w.Name, snap.BalanceKAS, snap.UTXOCount, snap.TxCount, path)
	}
	fmt.Printf("done: %d written, %d unchanged (checksum skip)\n", wrote, skipped)
	return common.ExitOK
}

func fetchWallet(ctx context.Context, client *Client, w config.KaspaWallet, maxPages int) (*Snapshot, error) {
	bal, err := client.GetBalance(ctx, w.Address)
	if err != nil {
		return nil, fmt.Errorf("balance: %w", err)
	}
	utxos, err := client.GetUTXOs(ctx, w.Address)
	if err != nil {
		return nil, fmt.Errorf("utxos: %w", err)
	}
	txs, err := client.GetAllTransactions(ctx, w.Address, 50, maxPages)
	if err != nil {
		return nil, fmt.Errorf("transactions: %w", err)
	}
	sortUTXOs(utxos)
	sortTransactions(txs)

	return &Snapshot{
		FetchedAt:    time.Now().UTC(),
		Source:       client.BaseURL,
		WalletName:   w.Name,
		Address:      w.Address,
		BalanceSompi: bal.Balance,
		BalanceKAS:   sompiToKAS(bal.Balance),
		UTXOCount:    len(utxos),
		UTXOs:        utxos,
		TxCount:      len(txs),
		Transactions: txs,
	}, nil
}

func sortUTXOs(utxos []UTXO) {
	sort.Slice(utxos, func(i, j int) bool {
		return utxoKey(utxos[i]) < utxoKey(utxos[j])
	})
}

func utxoKey(u UTXO) string {
	b, _ := json.Marshal(u.Outpoint)
	return string(b)
}

func sortTransactions(txs []Transaction) {
	sort.Slice(txs, func(i, j int) bool {
		idI, _ := txs[i]["transaction_id"].(string)
		idJ, _ := txs[j]["transaction_id"].(string)
		if idI != idJ {
			return idI < idJ
		}
		// stable fallback
		bi, _ := json.Marshal(txs[i])
		bj, _ := json.Marshal(txs[j])
		return string(bi) < string(bj)
	})
}

// sameContent reports whether path already stores the same API payload
// (ignoring fetched_at, which always changes).
func sameContent(path string, snap *Snapshot) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var existing Snapshot
	if err := json.Unmarshal(raw, &existing); err != nil {
		return false
	}
	a, err := contentFingerprint(snap)
	if err != nil {
		return false
	}
	b, err := contentFingerprint(&existing)
	if err != nil {
		return false
	}
	return a == b
}

func contentFingerprint(s *Snapshot) (string, error) {
	stable := struct {
		Source       string        `json:"source"`
		WalletName   string        `json:"wallet_name"`
		Address      string        `json:"address"`
		BalanceSompi int64         `json:"balance_sompi"`
		UTXOs        []UTXO        `json:"utxos"`
		Transactions []Transaction `json:"transactions"`
	}{
		Source:       s.Source,
		WalletName:   s.WalletName,
		Address:      s.Address,
		BalanceSompi: s.BalanceSompi,
		UTXOs:        s.UTXOs,
		Transactions: s.Transactions,
	}
	b, err := json.Marshal(stable)
	if err != nil {
		return "", err
	}
	return common.ChecksumHex(b), nil
}

func sompiToKAS(sompi int64) string {
	// 1 KAS = 100_000_000 sompi
	neg := sompi < 0
	if neg {
		sompi = -sompi
	}
	whole := sompi / 100_000_000
	frac := sompi % 100_000_000
	s := fmt.Sprintf("%d.%08d", whole, frac)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		s = "0"
	}
	if neg {
		return "-" + s
	}
	return s
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "wallet"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('_')
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
