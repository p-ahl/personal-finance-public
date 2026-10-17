package wallets

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/p-ahl/personal-finance/internal/download/common"
)

// Options controls `finance download wallets`.
type Options struct {
	DataDir string    // $FINANCE_DATA
	Config  string    // default $FINANCE_DATA/config/wallets.yaml
	Day     time.Time // folder date (default today, America/Denver)
	History bool      // also save every address's full transaction history (history/)
	Out     io.Writer // progress (wallet names and totals only; never addresses or keys)
}

// Endpoints (overridable in tests).
var (
	KaspaAPI    = "https://api.kaspa.org"
	BTCAPIs     = []string{"https://blockstream.info/api", "https://mempool.space/api"}
	LTCAPIs     = []string{"https://litecoinspace.org/api"}
	SolanaRPC   = "https://api.mainnet-beta.solana.com"
	KrakenAPI   = "https://api.kraken.com/0/public"
	GeckoAPI    = "https://api.coingecko.com/api/v3"
	splPrograms = []string{"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"}
)

// SPLTokens maps well-known Solana mints (public constants) to ledger commodities.
var SPLTokens = map[string]string{
	"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": "USDC",
	"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": "USDT",
	"2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo": "PYUSD",
}

// HistoryCoins get daily OHLC with --history (wallet coins plus coins seen in swaps).
var HistoryCoins = []string{"BCH", "BTC", "KAS", "LTC", "PYUSD", "SOL", "USDC", "USDT", "XMR"}

var priceIDs = map[string][2]string{ // commodity -> Kraken pair, CoinGecko id
	"KAS": {"KASUSD", "kaspa"}, "BTC": {"XBTUSD", "bitcoin"}, "LTC": {"LTCUSD", "litecoin"},
	"SOL": {"SOLUSD", "solana"}, "XMR": {"XMRUSD", "monero"}, "USDC": {"USDCUSD", "usd-coin"},
	"USDT": {"USDTZUSD", "tether"}, "PYUSD": {"PYUSDUSD", "paypal-usd"}, "BCH": {"BCHUSD", "bitcoin-cash"},
}

// Identifier is one public identifier of a wallet (saved in wallets.json / the local DB).
type Identifier struct {
	Chain  string `json:"chain"`
	Kind   string `json:"kind"` // address | derived | xpub | file
	Value  string `json:"value"`
	Path   string `json:"path,omitempty"`
	Used   bool   `json:"used"`
	Source string `json:"source,omitempty"` // xpub fingerprint for derived addresses
}

// Holding is one wallet coin balance.
type Holding struct {
	Chain     string   `json:"chain"`
	Commodity string   `json:"commodity"`
	Account   string   `json:"account"`
	Balance   string   `json:"balance"` // decimal string in coin units
	Addresses int      `json:"addresses"`
	Used      int      `json:"used_addresses"`
	Files     []string `json:"files"`
}

// WalletOut is one wallet in balances.json.
type WalletOut struct {
	Wallet      string       `json:"wallet"`
	Account     string       `json:"account"`
	PerCoin     bool         `json:"per_coin"`
	Note        string       `json:"note,omitempty"`
	Identifiers []Identifier `json:"identifiers"`
	Holdings    []Holding    `json:"holdings"`
}

// PriceOut is one USD price at the pull.
type PriceOut struct {
	Commodity string `json:"commodity"`
	Price     string `json:"price"`
	Source    string `json:"source"`
	Ref       string `json:"ref"`
	Alt       string `json:"alt,omitempty"` // the other source's price (cross-check)
}

// Pull is balances.json.
type Pull struct {
	AsOf      string      `json:"as_of"`
	FetchedAt string      `json:"fetched_at"`
	Wallets   []WalletOut `json:"wallets"`
	Prices    []PriceOut  `json:"prices"`
	Notes     []string    `json:"notes,omitempty"`
}

type run struct {
	ctx   context.Context
	f     *fetcher
	dir   string
	files []string
	opt   Options
}

func (r *run) save(name string, b []byte) error {
	p := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return err
	}
	r.files = append(r.files, name)
	return nil
}

func (r *run) get(name, u, label string) ([]byte, error) {
	b, err := r.f.do(r.ctx, "GET", u, nil, label)
	if err != nil {
		return nil, err
	}
	return b, r.save(name, b)
}

func (r *run) post(name, u string, body any, label string) ([]byte, error) {
	jb, _ := json.Marshal(body)
	b, err := r.f.do(r.ctx, "POST", u, jb, label)
	if err != nil {
		return nil, err
	}
	return b, r.save(name, b)
}

func (r *run) logf(format string, a ...any) {
	if r.opt.Out != nil {
		fmt.Fprintf(r.opt.Out, format, a...)
	}
}

// Run downloads every configured wallet into $FINANCE_DATA/raw/wallets/<date>/ (0700/0600):
// verbatim API responses, balances.json (normalized), MANIFEST.md and SHA256SUMS. Manual
// files already in the folder (e.g. a Cake Wallet CSV) are kept; a missing history file is
// carried forward from the newest earlier pull.
func Run(ctx context.Context, opt Options) int {
	if opt.Out == nil {
		opt.Out = os.Stdout
	}
	if opt.Config == "" {
		opt.Config = filepath.Join(opt.DataDir, "config", "wallets.yaml")
	}
	cfg, err := LoadConfig(opt.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return common.ExitConfig
	}
	loc, _ := time.LoadLocation("America/Denver")
	if opt.Day.IsZero() {
		opt.Day = time.Now().In(loc)
	}
	day := opt.Day.Format("2006-01-02")
	root := filepath.Join(opt.DataDir, "raw", "wallets")
	dir := filepath.Join(root, day)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "download error: %v\n", err)
		return common.ExitDownloadFail
	}
	_ = os.Chmod(root, 0o700)
	_ = os.Chmod(dir, 0o700)
	r := &run{ctx: ctx, f: newFetcher(), dir: dir, opt: opt}
	pull, err := r.download(cfg, root, day)
	if err != nil {
		fmt.Fprintf(os.Stderr, "download error: %v\n", err)
		_ = os.Remove(filepath.Join(dir, "MANIFEST.md")) // an incomplete pull must not be imported
		return common.ExitDownloadFail
	}
	pull.AsOf = day
	pull.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	jb, _ := json.MarshalIndent(pull, "", "  ")
	if err := r.save("balances.json", append(jb, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "download error: %v\n", err)
		return common.ExitDownloadFail
	}
	// counterparty labels and pass-through windows for scripts/wallet_events.py (no keys)
	lb, _ := json.MarshalIndent(map[string]any{"labels": cfg.Labels, "passthrough": cfg.Passthrough}, "", "  ")
	if err := r.save("labels.json", append(lb, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "download error: %v\n", err)
		return common.ExitDownloadFail
	}
	if err := r.finish(pull); err != nil {
		fmt.Fprintf(os.Stderr, "download error: %v\n", err)
		return common.ExitDownloadFail
	}
	r.logf("wallets pull %s: %d wallets, %d files -> %s\n", day, len(pull.Wallets), len(r.files), dir)
	return common.ExitOK
}

func (r *run) download(cfg *Config, root, day string) (*Pull, error) {
	pull := &Pull{}
	coins := map[string]bool{}
	for _, w := range cfg.Wallets {
		wo := WalletOut{Wallet: w.Name, Account: w.Account, PerCoin: w.PerCoin, Note: w.Note}
		per := map[string]*Holding{}
		add := func(chain, com, bal string, files ...string) {
			h := per[com]
			if h == nil {
				h = &Holding{Chain: chain, Commodity: com, Account: w.AccountFor(com), Balance: "0"}
				per[com] = h
			}
			h.Balance = addDec(h.Balance, bal)
			h.Files = append(h.Files, files...)
			coins[com] = true
		}
		for _, ch := range w.Chains {
			com := Coin[ch.Chain].Code
			tag := w.Name + "_" + ch.Chain
			switch {
			case ch.Chain == "kaspa":
				ids, bal, files, err := r.kaspa(tag, ch)
				if err != nil {
					return nil, fmt.Errorf("%s %s: %w", w.Name, ch.Chain, err)
				}
				wo.Identifiers = append(wo.Identifiers, ids...)
				add(ch.Chain, com, bal, files...)
				h := per[com]
				for _, id := range ids {
					if id.Kind != "xpub" {
						h.Addresses++
						if id.Used {
							h.Used++
						}
					}
				}
			case ch.Chain == "bitcoin" || ch.Chain == "litecoin":
				bal, used, file, err := r.esplora(tag, ch)
				if err != nil {
					return nil, fmt.Errorf("%s %s: %w", w.Name, ch.Chain, err)
				}
				wo.Identifiers = append(wo.Identifiers, Identifier{Chain: ch.Chain, Kind: "address", Value: ch.Address, Used: used})
				add(ch.Chain, com, bal, file)
				per[com].Addresses, per[com].Used = 1, b2i(used)
			case ch.Chain == "solana":
				bals, used, files, err := r.solana(tag, ch)
				if err != nil {
					return nil, fmt.Errorf("%s %s: %w", w.Name, ch.Chain, err)
				}
				wo.Identifiers = append(wo.Identifiers, Identifier{Chain: ch.Chain, Kind: "address", Value: ch.Address, Used: used})
				add(ch.Chain, com, bals["SOL"], files...)
				per[com].Addresses, per[com].Used = 1, b2i(used)
				for c, b := range bals {
					if c != "SOL" {
						add(ch.Chain, c, b, files...)
					}
				}
			case ch.Chain == "monero":
				bal, file, note, err := r.moneroFile(root, day, ch)
				if err != nil {
					return nil, fmt.Errorf("%s %s: %w", w.Name, ch.Chain, err)
				}
				if note != "" {
					pull.Notes = append(pull.Notes, w.Name+": "+note)
				}
				wo.Identifiers = append(wo.Identifiers, Identifier{Chain: ch.Chain, Kind: "file", Value: file, Used: true})
				add(ch.Chain, com, bal, file)
			}
		}
		var cs []string
		for c := range per {
			cs = append(cs, c)
		}
		sort.Strings(cs)
		for _, c := range cs {
			h := per[c]
			sort.Strings(h.Files)
			h.Files = uniq(h.Files)
			wo.Holdings = append(wo.Holdings, *h)
			r.logf("  %-10s %-6s %s (%d/%d addresses used)\n", w.Name, c, h.Balance, h.Used, h.Addresses)
		}
		pull.Wallets = append(pull.Wallets, wo)
	}
	prices, err := r.prices(coins)
	if err != nil {
		return nil, err
	}
	pull.Prices = prices
	if r.opt.History {
		// daily OHLC (Kraken keeps the last 720 days) to value historical swaps/transfers
		for _, c := range HistoryCoins {
			if _, err := r.get("history/ohlc_"+c+".json", KrakenAPI+"/OHLC?interval=1440&pair="+priceIDs[c][0], "kraken ohlc"); err != nil {
				return nil, err
			}
		}
	}
	return pull, nil
}

// ---------- kaspa ----------

func (r *run) kaspa(tag string, ch Chain) ([]Identifier, string, []string, error) {
	type addr struct {
		a, path string
	}
	var addrs []addr
	var ids []Identifier
	var files []string
	if ch.Address != "" {
		if _, _, _, err := DecodeKaspaAddress(ch.Address); err != nil {
			return nil, "", nil, err
		}
		addrs = append(addrs, addr{a: ch.Address})
	} else {
		k, err := ParseExtPub(ch.KPub)
		if err != nil {
			return nil, "", nil, err
		}
		fp := sha256.Sum256([]byte(ch.KPub))
		fpr := "kpub:" + hex.EncodeToString(fp[:4])
		ids = append(ids, Identifier{Chain: "kaspa", Kind: "xpub", Value: fpr, Path: ch.Path, Used: true}) // fingerprint only; the key stays in wallets.yaml
		for _, branch := range []uint32{0, 1} {
			bk, err := k.Child(branch)
			if err != nil {
				return nil, "", nil, err
			}
			gap, i, batch := 0, uint32(0), 0
			for gap < ch.GapLimit {
				var chunk []string
				for j := 0; j < ch.GapLimit; j++ {
					c, err := bk.Child(i + uint32(j))
					if err != nil {
						return nil, "", nil, err
					}
					chunk = append(chunk, KaspaSchnorrAddress("kaspa", c.Key))
				}
				batch++
				name := fmt.Sprintf("kaspa_%s_active_%d_%03d.json", tag, branch, batch)
				b, err := r.post(name, KaspaAPI+"/addresses/active", map[string]any{"addresses": chunk}, "kaspa active")
				if err != nil {
					return nil, "", nil, err
				}
				files = append(files, name)
				var act []struct {
					Address string `json:"address"`
					Active  bool   `json:"active"`
				}
				if err := json.Unmarshal(b, &act); err != nil {
					return nil, "", nil, fmt.Errorf("kaspa active: %w", err)
				}
				used := map[string]bool{}
				for _, x := range act {
					used[x.Address] = x.Active
				}
				for j, a := range chunk {
					p := fmt.Sprintf("%s/%d/%d", strings.TrimSuffix(ch.Path, "/"), branch, i+uint32(j))
					if used[a] {
						gap = 0
					} else {
						gap++
					}
					ids = append(ids, Identifier{Chain: "kaspa", Kind: "derived", Value: a, Path: p, Used: used[a], Source: fpr})
					if used[a] || gap <= ch.GapLimit {
						addrs = append(addrs, addr{a: a, path: p})
					}
				}
				i += uint32(ch.GapLimit)
			}
		}
	}
	list := make([]string, 0, len(addrs))
	for _, a := range addrs {
		list = append(list, a.a)
	}
	name := "kaspa_" + tag + "_balances.json"
	b, err := r.post(name, KaspaAPI+"/addresses/balances", map[string]any{"addresses": list}, "kaspa balances")
	if err != nil {
		return nil, "", nil, err
	}
	files = append(files, name)
	var bals []struct {
		Address string `json:"address"`
		Balance *int64 `json:"balance"`
	}
	if err := json.Unmarshal(b, &bals); err != nil {
		return nil, "", nil, fmt.Errorf("kaspa balances: %w", err)
	}
	if len(bals) != len(list) {
		return nil, "", nil, fmt.Errorf("kaspa balances: %d results for %d addresses", len(bals), len(list))
	}
	var sompi int64
	for _, x := range bals {
		if x.Balance != nil {
			sompi += *x.Balance
		}
	}
	if ch.Address != "" {
		ids = append(ids, Identifier{Chain: "kaspa", Kind: "address", Value: ch.Address, Used: true})
	}
	if r.opt.History {
		n := 0
		for _, id := range ids {
			if id.Kind == "xpub" || !id.Used {
				continue
			}
			n++
			before := "0"
			for page := 1; page <= 200; page++ {
				u := fmt.Sprintf("%s/addresses/%s/full-transactions-page?limit=100&before=%s&resolve_previous_outpoints=light", KaspaAPI, url.PathEscape(id.Value), before)
				b, err := r.f.do(r.ctx, "GET", u, nil, "kaspa history")
				if err != nil {
					return nil, "", nil, err
				}
				var txs []struct {
					BlockTime int64 `json:"block_time"`
				}
				if err := json.Unmarshal(b, &txs); err != nil {
					return nil, "", nil, fmt.Errorf("kaspa history: %w", err)
				}
				if len(txs) == 0 {
					break
				}
				if err := r.save(fmt.Sprintf("history/kaspa_%s_%03d_page%03d.json", tag, n, page), b); err != nil {
					return nil, "", nil, err
				}
				min := txs[0].BlockTime
				for _, t := range txs {
					if t.BlockTime < min {
						min = t.BlockTime
					}
				}
				if len(txs) < 100 {
					break
				}
				before = strconv.FormatInt(min, 10)
			}
		}
	}
	return ids, fmtMinor(big.NewInt(sompi), 8), files, nil
}

// ---------- bitcoin / litecoin (Esplora) ----------

func (r *run) esplora(tag string, ch Chain) (string, bool, string, error) {
	apis := BTCAPIs
	if ch.Chain == "litecoin" {
		apis = LTCAPIs
	}
	var lastErr error
	for _, api := range apis {
		name := tag + "_address.json"
		b, err := r.get(name, api+"/address/"+url.PathEscape(ch.Address), ch.Chain+" address")
		if err != nil {
			lastErr = err
			continue
		}
		var raw struct {
			ChainStats   map[string]int64 `json:"chain_stats"`
			MempoolStats map[string]int64 `json:"mempool_stats"`
		}
		if err := json.Unmarshal(b, &raw); err != nil || raw.ChainStats == nil {
			lastErr = fmt.Errorf("%s address: unexpected response", ch.Chain)
			continue
		}
		cs := raw.ChainStats
		sat := cs["funded_txo_sum"] - cs["spent_txo_sum"] // confirmed balance
		used := cs["tx_count"]+raw.MempoolStats["tx_count"] > 0
		if r.opt.History && used {
			last := ""
			for page := 1; page <= 100; page++ {
				u := api + "/address/" + url.PathEscape(ch.Address) + "/txs"
				if last != "" {
					u += "/chain/" + last
				}
				hb, err := r.f.do(r.ctx, "GET", u, nil, ch.Chain+" history")
				if err != nil {
					return "", false, "", err
				}
				var txs []struct {
					Txid string `json:"txid"`
				}
				if err := json.Unmarshal(hb, &txs); err != nil {
					return "", false, "", err
				}
				if len(txs) == 0 {
					break
				}
				if err := r.save(fmt.Sprintf("history/%s_page%03d.json", tag, page), hb); err != nil {
					return "", false, "", err
				}
				if len(txs) < 25 {
					break
				}
				last = txs[len(txs)-1].Txid
			}
		}
		return fmtMinor(big.NewInt(sat), 8), used, name, nil
	}
	return "", false, "", lastErr
}

// ---------- solana ----------

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (r *run) rpc(name, method string, params ...any) (json.RawMessage, error) {
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
	jb, _ := json.Marshal(body)
	b, err := r.f.do(r.ctx, "POST", SolanaRPC, jb, "solana "+method)
	if err != nil {
		return nil, err
	}
	var resp rpcResp
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("solana %s: %w", method, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("solana %s: %s", method, resp.Error.Message)
	}
	if name != "" {
		if err := r.save(name, b); err != nil {
			return nil, err
		}
	}
	return resp.Result, nil
}

func (r *run) solana(tag string, ch Chain) (map[string]string, bool, []string, error) {
	out := map[string]string{}
	var files []string
	n1 := "solana_" + tag + "_getBalance.json"
	res, err := r.rpc(n1, "getBalance", ch.Address, map[string]string{"commitment": "finalized"})
	if err != nil {
		return nil, false, nil, err
	}
	files = append(files, n1)
	var gb struct {
		Value int64 `json:"value"`
	}
	if err := json.Unmarshal(res, &gb); err != nil {
		return nil, false, nil, err
	}
	out["SOL"] = fmtMinor(big.NewInt(gb.Value), 9)
	var tokenAccts []string
	for i, prog := range splPrograms {
		name := fmt.Sprintf("solana_%s_tokens_%d.json", tag, i+1)
		res, err := r.rpc(name, "getTokenAccountsByOwner", ch.Address, map[string]string{"programId": prog}, map[string]string{"encoding": "jsonParsed", "commitment": "finalized"})
		if err != nil {
			return nil, false, nil, err
		}
		files = append(files, name)
		var ta struct {
			Value []struct {
				Pubkey  string `json:"pubkey"`
				Account struct {
					Data struct {
						Parsed struct {
							Info struct {
								Mint        string `json:"mint"`
								TokenAmount struct {
									Amount   string `json:"amount"`
									Decimals int    `json:"decimals"`
								} `json:"tokenAmount"`
							} `json:"info"`
						} `json:"parsed"`
					} `json:"data"`
				} `json:"account"`
			} `json:"value"`
		}
		if err := json.Unmarshal(res, &ta); err != nil {
			return nil, false, nil, fmt.Errorf("solana tokens: %w", err)
		}
		for _, v := range ta.Value {
			tokenAccts = append(tokenAccts, v.Pubkey)
			info := v.Account.Data.Parsed.Info
			com, ok := SPLTokens[info.Mint]
			if !ok {
				com = "SPL-" + info.Mint[:6]
			}
			amt, ok := new(big.Int).SetString(info.TokenAmount.Amount, 10)
			if !ok {
				return nil, false, nil, fmt.Errorf("solana token amount")
			}
			if amt.Sign() == 0 && strings.HasPrefix(com, "SPL-") {
				continue
			}
			out[com] = addDec(orZero(out[com]), fmtMinor(amt, info.TokenAmount.Decimals))
		}
	}
	used := false
	{ // signatures: "used" flag, and the full history with --history
		sigs := map[string]bool{}
		var order []string
		for i, acct := range append([]string{ch.Address}, tokenAccts...) {
			before := ""
			for page := 1; page <= 50; page++ {
				opts := map[string]any{"limit": 1000}
				if before != "" {
					opts["before"] = before
				}
				name := ""
				if r.opt.History {
					name = fmt.Sprintf("history/solana_%s_sigs_%02d_%03d.json", tag, i, page)
				}
				res, err := r.rpc(name, "getSignaturesForAddress", acct, opts)
				if err != nil {
					return nil, false, nil, err
				}
				var ss []struct {
					Signature string `json:"signature"`
				}
				if err := json.Unmarshal(res, &ss); err != nil {
					return nil, false, nil, err
				}
				for _, s := range ss {
					if !sigs[s.Signature] {
						sigs[s.Signature] = true
						order = append(order, s.Signature)
					}
				}
				if len(ss) < 1000 || !r.opt.History {
					break
				}
				before = ss[len(ss)-1].Signature
			}
		}
		used = len(order) > 0
		if r.opt.History {
			for _, s := range order {
				name := fmt.Sprintf("history/solana_%s_tx_%s.json", tag, s[:20])
				if b, err := os.ReadFile(filepath.Join(r.dir, name)); err == nil && len(b) > 0 {
					r.files = append(r.files, name) // finalized transactions never change: reuse
					continue
				}
				if r.copyEarlier(name) {
					r.files = append(r.files, name)
					continue
				}
				if _, err := r.rpc(name, "getTransaction", s, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0, "commitment": "finalized"}); err != nil {
					return nil, false, nil, err
				}
			}
		}
	}
	return out, used, files, nil
}

// copyEarlier copies an immutable history file (a finalized transaction) from the newest earlier
// pull that has it, so nightly pulls only fetch new transactions.
func (r *run) copyEarlier(name string) bool {
	root, day := filepath.Dir(r.dir), filepath.Base(r.dir)
	dirs, _ := filepath.Glob(filepath.Join(root, "[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]"))
	sort.Strings(dirs)
	for i := len(dirs) - 1; i >= 0; i-- {
		if filepath.Base(dirs[i]) >= day {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dirs[i], name))
		if err != nil || len(b) == 0 {
			continue
		}
		return os.WriteFile(filepath.Join(r.dir, name), b, 0o600) == nil
	}
	return false
}

// ---------- monero (Cake Wallet CSV export; no public balance lookup) ----------

func (r *run) moneroFile(root, day string, ch Chain) (bal, file, note string, err error) {
	file = ch.File
	p := filepath.Join(r.dir, file)
	if _, err := os.Stat(p); err != nil {
		// carry the newest earlier export forward (the export is a manual snapshot)
		dirs, _ := filepath.Glob(filepath.Join(root, "[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]"))
		sort.Strings(dirs)
		found := ""
		for i := len(dirs) - 1; i >= 0; i-- {
			if filepath.Base(dirs[i]) < day {
				if _, err := os.Stat(filepath.Join(dirs[i], file)); err == nil {
					found = dirs[i]
					break
				}
			}
		}
		if found == "" {
			return "", "", "", fmt.Errorf("%s not found in this or an earlier pull (export it from the wallet app)", file)
		}
		b, err := os.ReadFile(filepath.Join(found, file))
		if err != nil {
			return "", "", "", err
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return "", "", "", err
		}
		note = fmt.Sprintf("%s carried forward from the %s pull (manual export)", file, filepath.Base(found))
	}
	_ = os.Chmod(p, 0o600)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", "", "", err
	}
	rows, err := ParseCakeCSV(b)
	if err != nil {
		return "", "", "", err
	}
	total := new(big.Int)
	for _, x := range rows {
		total.Add(total, x.Delta)
	}
	return fmtMinor(total, 12), file, note, nil
}

// CakeRow is one Cake Wallet transaction-history row.
type CakeRow struct {
	Time      time.Time
	Type      string // incoming | outgoing
	Amount    *big.Int
	Fee       *big.Int
	Delta     *big.Int // incoming: +amount; outgoing: -(amount + fee); 1e-12 XMR
	TxID      string
	Note      string
	Confirmed bool
}

// ParseCakeCSV reads a Cake Wallet export (record_type,date_time,type,amount,currency,fee,tx_id,...).
// Only confirmed XMR transactions count; the fee on incoming rows is the sender's.
func ParseCakeCSV(b []byte) ([]CakeRow, error) {
	recs, err := csv.NewReader(strings.NewReader(string(b))).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("cake csv: %w", err)
	}
	if len(recs) < 1 {
		return nil, fmt.Errorf("cake csv: empty")
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[strings.TrimSpace(h)] = i
	}
	for _, k := range []string{"record_type", "date_time", "type", "amount", "currency", "fee", "tx_id", "status"} {
		if _, ok := col[k]; !ok {
			return nil, fmt.Errorf("cake csv: column %s missing", k)
		}
	}
	var out []CakeRow
	for _, rec := range recs[1:] {
		g := func(k string) string {
			if i, ok := col[k]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		if g("record_type") != "transaction" || g("currency") != "XMR" {
			continue
		}
		t, err := time.Parse(time.RFC3339, g("date_time"))
		if err != nil {
			return nil, fmt.Errorf("cake csv: date %q", g("date_time"))
		}
		amt, err := parseDec(g("amount"), 12)
		if err != nil {
			return nil, err
		}
		fee, err := parseDec(orZero(g("fee")), 12)
		if err != nil {
			return nil, err
		}
		row := CakeRow{Time: t.UTC(), Type: g("type"), Amount: amt, Fee: fee, TxID: g("tx_id"), Note: g("note"), Confirmed: g("status") == "confirmed"}
		switch row.Type {
		case "incoming":
			row.Delta = new(big.Int).Set(amt)
		case "outgoing":
			row.Delta = new(big.Int).Neg(new(big.Int).Add(amt, fee))
		default:
			return nil, fmt.Errorf("cake csv: type %q", row.Type)
		}
		if !row.Confirmed {
			row.Delta = new(big.Int)
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// ---------- prices ----------

func (r *run) prices(coins map[string]bool) ([]PriceOut, error) {
	var cs []string
	var ids []string
	for c := range coins {
		cs = append(cs, c)
		if p, ok := priceIDs[c]; ok {
			ids = append(ids, p[1])
		}
	}
	sort.Strings(cs)
	sort.Strings(ids)
	gecko := map[string]map[string]float64{}
	if b, err := r.get("prices_coingecko.json", GeckoAPI+"/simple/price?ids="+strings.Join(ids, ",")+"&vs_currencies=usd&include_last_updated_at=true", "coingecko"); err == nil {
		_ = json.Unmarshal(b, &gecko)
	}
	var out []PriceOut
	for _, c := range cs {
		p, ok := priceIDs[c]
		if !ok {
			continue
		}
		po := PriceOut{Commodity: c}
		if b, err := r.get("prices_kraken_"+c+".json", KrakenAPI+"/Ticker?pair="+p[0], "kraken ticker"); err == nil {
			var t struct {
				Error  []string                        `json:"error"`
				Result map[string]struct{ C []string } `json:"result"`
			}
			if json.Unmarshal(b, &t) == nil && len(t.Error) == 0 {
				for k, v := range t.Result {
					if len(v.C) > 0 && v.C[0] != "" {
						po.Price, po.Source, po.Ref = trimDec(v.C[0]), "kraken-ticker", k
					}
				}
			}
		}
		if g, ok := gecko[p[1]]["usd"]; ok {
			gs := strconv.FormatFloat(g, 'f', -1, 64)
			if po.Price == "" {
				po.Price, po.Source, po.Ref = gs, "coingecko", p[1]
			} else {
				po.Alt = "coingecko " + gs
			}
		}
		if po.Price == "" {
			return nil, fmt.Errorf("no USD price for %s from Kraken or CoinGecko", c)
		}
		out = append(out, po)
	}
	return out, nil
}

// ---------- manifest ----------

func (r *run) finish(p *Pull) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Wallet pull %s\n\nFetched %s by `finance download wallets` (public, read-only endpoints: api.kaspa.org, Esplora (blockstream.info / mempool.space / litecoinspace.org), Solana public RPC, Kraken public Ticker, CoinGecko simple price). Manual exports (Cake Wallet CSV) are listed with their wallet. Addresses and extended keys are not written here.\n\n", p.AsOf, p.FetchedAt)
	sb.WriteString("| Wallet | Coin | Balance | Addresses (used/scanned) |\n|---|---|---:|---:|\n")
	for _, w := range p.Wallets {
		for _, h := range w.Holdings {
			fmt.Fprintf(&sb, "| %s | %s | %s | %d/%d |\n", w.Wallet, h.Commodity, h.Balance, h.Used, h.Addresses)
		}
	}
	sb.WriteString("\n| Coin | USD | Source | Cross-check |\n|---|---:|---|---|\n")
	for _, x := range p.Prices {
		fmt.Fprintf(&sb, "| %s | %s | %s | %s |\n", x.Commodity, x.Price, x.Source, x.Alt)
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&sb, "\nNote: %s\n", n)
	}
	if err := r.save("MANIFEST.md", []byte(sb.String())); err != nil {
		return err
	}
	// checksums over every file in the folder (downloaded and manual)
	var all []string
	err := filepath.Walk(r.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(r.dir, path)
		if rel != "SHA256SUMS" {
			all = append(all, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(all)
	var sums strings.Builder
	for _, f := range all {
		b, err := os.ReadFile(filepath.Join(r.dir, f))
		if err != nil {
			return err
		}
		h := sha256.Sum256(b)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h[:]), f)
	}
	return os.WriteFile(filepath.Join(r.dir, "SHA256SUMS"), []byte(sums.String()), 0o600)
}

// ---------- decimals ----------

func parseDec(s string, scale int) (*big.Int, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return nil, fmt.Errorf("bad decimal %q", s)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)))
	if !r.IsInt() {
		return nil, fmt.Errorf("decimal %q has more than %d places", s, scale)
	}
	return new(big.Int).Set(r.Num()), nil
}

func fmtMinor(v *big.Int, scale int) string {
	r := new(big.Rat).SetFrac(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
	return trimDec(r.FloatString(scale))
}

func addDec(a, b string) string {
	x, _ := new(big.Rat).SetString(orZero(a))
	y, _ := new(big.Rat).SetString(orZero(b))
	return trimDec(new(big.Rat).Add(x, y).FloatString(18))
}

func trimDec(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

func orZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func uniq(s []string) []string {
	out := s[:0]
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}
