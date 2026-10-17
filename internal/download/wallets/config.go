package wallets

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the local $FINANCE_DATA/config/wallets.yaml (never committed; template
// config/wallets.data.example.yaml). It holds public addresses and extended public keys only.
type Config struct {
	Wallets []Wallet `yaml:"wallets"`
	// Labels: third-party public address -> label, used by scripts/wallet_events.py to
	// classify counterparties ("coinbase", "kraken", swap services, ...).
	Labels map[string]string `yaml:"labels"`
	// Passthrough: time windows whose movements belong to someone else (excluded from the books).
	Passthrough []Passthrough `yaml:"passthrough"`
}

// Passthrough is an excluded time window (UTC, RFC 3339).
type Passthrough struct {
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to" json:"to"`
	Note string `yaml:"note" json:"note"`
}

// Wallet is one self-custody wallet (an app or device), booked to Account. PerCoin wallets
// get one sub-account per coin (Account:KAS, Account:BTC, ...).
type Wallet struct {
	Name    string  `yaml:"name"`
	Account string  `yaml:"account"`
	PerCoin bool    `yaml:"per_coin"`
	Note    string  `yaml:"note"`
	Chains  []Chain `yaml:"chains"`
}

// Chain is one address (or extended public key) of a wallet on one chain.
type Chain struct {
	Chain    string `yaml:"chain"`     // kaspa | bitcoin | litecoin | solana | monero
	Address  string `yaml:"address"`   // explicit public address
	KPub     string `yaml:"kpub"`      // extended public key (kaspa); receive /0/i and change /1/i
	Path     string `yaml:"path"`      // account-level path of the extended key (informational)
	GapLimit int    `yaml:"gap_limit"` // default 20
	// File: a transaction-history export saved in the pull folder (e.g. Cake Wallet CSV for
	// monero, which has no public balance lookup). Its balance is computed from the rows.
	File string `yaml:"file"`
}

// Coin is the ledger commodity of a chain's native asset and its decimals.
var Coin = map[string]struct {
	Code     string
	Decimals int
}{
	"kaspa":    {"KAS", 8},
	"bitcoin":  {"BTC", 8},
	"litecoin": {"LTC", 8},
	"solana":   {"SOL", 9},
	"monero":   {"XMR", 12},
}

var nameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)

// LoadConfig reads and validates wallets.yaml.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(c.Wallets) == 0 {
		return nil, fmt.Errorf("%s: no wallets", path)
	}
	seen := map[string]bool{}
	for i := range c.Wallets {
		w := &c.Wallets[i]
		if !nameRe.MatchString(w.Name) || seen[w.Name] {
			return nil, fmt.Errorf("wallet %d: name %q must be unique, letters/digits/dashes", i+1, w.Name)
		}
		seen[w.Name] = true
		if !strings.HasPrefix(w.Account, "Assets:") {
			return nil, fmt.Errorf("wallet %s: account must be an Assets: account", w.Name)
		}
		coins := map[string]bool{}
		for j := range w.Chains {
			ch := &w.Chains[j]
			if _, ok := Coin[ch.Chain]; !ok {
				return nil, fmt.Errorf("wallet %s: unknown chain %q", w.Name, ch.Chain)
			}
			n := 0
			for _, s := range []string{ch.Address, ch.KPub, ch.File} {
				if s != "" {
					n++
				}
			}
			if n != 1 {
				return nil, fmt.Errorf("wallet %s chain %s: set exactly one of address, kpub, file", w.Name, ch.Chain)
			}
			if ch.KPub != "" && ch.Chain != "kaspa" {
				return nil, fmt.Errorf("wallet %s: kpub is only supported for kaspa", w.Name)
			}
			if ch.File != "" && ch.Chain != "monero" {
				return nil, fmt.Errorf("wallet %s: file is only supported for monero (Cake Wallet CSV)", w.Name)
			}
			if ch.GapLimit <= 0 {
				ch.GapLimit = 20
			}
			coins[Coin[ch.Chain].Code] = true
		}
		if !w.PerCoin && len(coins) > 1 {
			return nil, fmt.Errorf("wallet %s: several coins need per_coin: true", w.Name)
		}
	}
	return &c, nil
}

// AccountFor is the ledger account holding commodity c in wallet w.
func (w Wallet) AccountFor(c string) string {
	if w.PerCoin {
		return w.Account + ":" + c
	}
	return w.Account
}
