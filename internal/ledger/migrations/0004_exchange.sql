-- 0004: exchange staging (Kraken). Verbatim rows from the exchange's read-only API, cumulative
-- like raw_transactions: keyed by the exchange's own ids, never deleted. The journal entries
-- for the exchange account are derived from these by internal/books (exchange.go).

-- One row per exchange ledger entry (Kraken Ledgers). Amounts stay decimal strings as sent.
CREATE TABLE raw_exchange_ledger (
  exchange      TEXT NOT NULL,                 -- kraken
  ledger_id     TEXT NOT NULL,                 -- Kraken ledger id (L...)
  refid         TEXT NOT NULL,                 -- groups the legs of one event (trade, deposit, ...)
  time          TEXT NOT NULL,                 -- unix seconds, decimal string as received
  local_date    TEXT NOT NULL CHECK (local_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'), -- America/Denver
  type          TEXT NOT NULL,
  subtype       TEXT NOT NULL DEFAULT '',
  asset         TEXT NOT NULL,                 -- exchange asset code (XXBT, KAS, SOL.F, USD.HOLD)
  commodity     TEXT NOT NULL,                 -- ledger commodity (BTC, KAS, SOL, USD)
  amount        TEXT NOT NULL,
  fee           TEXT NOT NULL,
  balance       TEXT NOT NULL,
  raw_json      TEXT NOT NULL,
  first_run_id  INTEGER REFERENCES import_runs(id),
  last_run_id   INTEGER REFERENCES import_runs(id),
  PRIMARY KEY (exchange, ledger_id)
);
CREATE INDEX raw_exchange_ledger_ref ON raw_exchange_ledger(exchange, refid);

-- Trade fills (Kraken TradesHistory): audit trail for prices; the ledger rows carry the amounts.
CREATE TABLE raw_exchange_trades (
  exchange      TEXT NOT NULL,
  trade_id      TEXT NOT NULL,
  pair          TEXT NOT NULL,
  time          TEXT NOT NULL,
  side          TEXT NOT NULL,                 -- buy | sell
  price         TEXT NOT NULL,
  cost          TEXT NOT NULL,
  fee           TEXT NOT NULL,
  vol           TEXT NOT NULL,
  raw_json      TEXT NOT NULL,
  first_run_id  INTEGER REFERENCES import_runs(id),
  last_run_id   INTEGER REFERENCES import_runs(id),
  PRIMARY KEY (exchange, trade_id)
);

-- Balances reported by the exchange per pull (reconciliation source), one row per asset.
CREATE TABLE exchange_balance_snapshots (
  exchange      TEXT NOT NULL,
  as_of         TEXT NOT NULL,                 -- pull folder date (America/Denver)
  asset         TEXT NOT NULL,
  commodity     TEXT NOT NULL,
  balance       TEXT NOT NULL,
  raw_json      TEXT NOT NULL,
  import_run_id INTEGER REFERENCES import_runs(id),
  PRIMARY KEY (exchange, as_of, asset)
);

-- USD prices from the exchange's public market data: daily closes (OHLC, UTC day) and the
-- ticker's last trade at pull time (kind = 'ticker', price_date = pull date).
CREATE TABLE exchange_prices (
  exchange      TEXT NOT NULL,
  commodity     TEXT NOT NULL,
  price_date    TEXT NOT NULL,
  kind          TEXT NOT NULL CHECK (kind IN ('close','ticker')),
  price         TEXT NOT NULL,
  pair          TEXT NOT NULL,
  PRIMARY KEY (exchange, commodity, price_date, kind)
);
