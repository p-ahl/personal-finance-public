package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Root is the on-disk wallets config (config/wallets.yaml).
type Root struct {
	Kaspa KaspaConfig `yaml:"kaspa"`
}

// KaspaConfig holds Kaspa wallet entries.
type KaspaConfig struct {
	Wallets []KaspaWallet `yaml:"wallets"`
}

// KaspaWallet is one named Kaspa wallet.
// Phase 1: only Address is fetched. KPub is stored for later derivation.
type KaspaWallet struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address,omitempty"`
	KPub    string `yaml:"kpub,omitempty"`
	Note    string `yaml:"note,omitempty"`
}

// Load reads and parses path (typically config/wallets.yaml).
func Load(path string) (*Root, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var root Root
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &root, nil
}

// FetchableAddresses returns wallets that have an explicit address set.
func (c KaspaConfig) FetchableAddresses() []KaspaWallet {
	var out []KaspaWallet
	for _, w := range c.Wallets {
		if w.Address != "" {
			out = append(out, w)
		}
	}
	return out
}

// KPubOnly returns wallets that only have a kpub (skipped in Phase 1).
func (c KaspaConfig) KPubOnly() []KaspaWallet {
	var out []KaspaWallet
	for _, w := range c.Wallets {
		if w.Address == "" && w.KPub != "" {
			out = append(out, w)
		}
	}
	return out
}
