-- 0005: self-custody wallets (finance download wallets / finance import wallets). Public data
-- from block explorers and the wallet owner's exports; nothing here can move funds. Addresses
-- and extended public keys are identifiers of the owner's accounts: they live only in this
-- local database and the local config, never in git, statements or logs (reports show wallet
-- names only).

-- One row per wallet (a hardware wallet, a mobile app, ...). account = ledger account (per_coin: one
-- sub-account per coin, <account>:<COIN>).
CREATE TABLE wallets (
  name          TEXT PRIMARY KEY,
  account       TEXT NOT NULL,
  per_coin      INTEGER NOT NULL DEFAULT 0 CHECK (per_coin IN (0, 1)),
  note          TEXT NOT NULL DEFAULT '',
  updated_run_id INTEGER REFERENCES import_runs(id)
);

-- Account identifiers: addresses (given or derived from an extended public key), the extended
-- public key itself, or a wallet export file. Kept current by each import (last_seen).
CREATE TABLE wallet_identifiers (
  wallet        TEXT NOT NULL REFERENCES wallets(name),
  chain         TEXT NOT NULL,                 -- kaspa | bitcoin | litecoin | solana | monero
  kind          TEXT NOT NULL CHECK (kind IN ('address', 'derived', 'xpub', 'file')),
  value         TEXT NOT NULL,
  path          TEXT NOT NULL DEFAULT '',      -- derivation path (derived / xpub)
  used          INTEGER NOT NULL DEFAULT 0,    -- has on-chain history
  account       TEXT NOT NULL DEFAULT '',
  first_seen    TEXT NOT NULL,
  last_seen     TEXT NOT NULL,
  PRIMARY KEY (wallet, chain, kind, value)
);

-- On-chain balances per pull (reconciliation source; finance check compares quantities).
CREATE TABLE wallet_balance_snapshots (
  wallet        TEXT NOT NULL REFERENCES wallets(name),
  as_of         TEXT NOT NULL,                 -- pull folder date (America/Denver)
  commodity     TEXT NOT NULL,
  account       TEXT NOT NULL,
  balance       TEXT NOT NULL,                 -- decimal string
  addresses     INTEGER NOT NULL DEFAULT 0,
  used_addresses INTEGER NOT NULL DEFAULT 0,
  import_run_id INTEGER REFERENCES import_runs(id),
  PRIMARY KEY (wallet, as_of, commodity)
);

-- USD prices fetched with each pull (public tickers).
CREATE TABLE wallet_prices (
  commodity     TEXT NOT NULL,
  as_of         TEXT NOT NULL,
  price         TEXT NOT NULL,
  source        TEXT NOT NULL,
  ref           TEXT NOT NULL DEFAULT '',
  alt           TEXT NOT NULL DEFAULT '',      -- cross-check price (other source)
  import_run_id INTEGER REFERENCES import_runs(id),
  PRIMARY KEY (commodity, as_of)
);

-- Inferred wallet events (scripts/wallet_events.py -> <pull>/events.csv): swaps, transfers,
-- exchange moves, pass-through, unmatched. Heuristic (timing + amounts), replaced as a whole by
-- each import; tx evidence is truncated ids only.
CREATE TABLE wallet_events (
  id            TEXT PRIMARY KEY,
  time          TEXT NOT NULL,                 -- UTC
  date          TEXT NOT NULL,                 -- America/Denver
  kind          TEXT NOT NULL,
  wallet_out    TEXT NOT NULL DEFAULT '',
  coin_out      TEXT NOT NULL DEFAULT '',
  qty_out       TEXT NOT NULL DEFAULT '',
  fee_out       TEXT NOT NULL DEFAULT '',
  wallet_in     TEXT NOT NULL DEFAULT '',
  coin_in       TEXT NOT NULL DEFAULT '',
  qty_in        TEXT NOT NULL DEFAULT '',
  ref           TEXT NOT NULL DEFAULT '',
  price_out     TEXT NOT NULL DEFAULT '',
  price_in      TEXT NOT NULL DEFAULT '',
  usd_out       TEXT NOT NULL DEFAULT '',
  usd_in        TEXT NOT NULL DEFAULT '',
  confidence    TEXT NOT NULL DEFAULT '',
  evidence      TEXT NOT NULL DEFAULT '',
  memo          TEXT NOT NULL DEFAULT '',
  pull          TEXT NOT NULL,
  import_run_id INTEGER REFERENCES import_runs(id)
);
