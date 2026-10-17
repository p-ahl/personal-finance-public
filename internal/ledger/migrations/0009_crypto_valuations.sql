-- 0009: daily crypto price history and per-holding valuation history.
--
-- crypto_price_history: daily USD closes per coin (Kraken public OHLC, UTC day candles), staged
-- from every wallet pull's history/ohlc_<COIN>.json (finance import wallets). Used to value the
-- self-custody wallets at month-ends and any date without a pull ticker price.
-- crypto_valuations: derived, rebuilt on every import: per day and wallet account/coin the
-- on-chain-reconciled quantity, market price, value and cost basis (ledger cost, mark-to-market
-- adjustments excluded), so value and unrealized gain can be charted over time.
CREATE TABLE crypto_price_history (
  commodity   TEXT NOT NULL,
  price_date  TEXT NOT NULL,       -- UTC day of the candle
  price       TEXT NOT NULL,       -- decimal string, USD (daily close)
  source      TEXT NOT NULL,
  PRIMARY KEY (commodity, price_date)
);

CREATE TABLE crypto_valuations (
  val_date      TEXT NOT NULL,
  wallet        TEXT NOT NULL,
  account       TEXT NOT NULL,
  commodity     TEXT NOT NULL,
  quantity      TEXT NOT NULL,     -- decimal string
  price_usd     TEXT NOT NULL,
  price_source  TEXT NOT NULL,     -- pull ticker or daily close
  value_cents   INTEGER NOT NULL,
  cost_cents    INTEGER NOT NULL,  -- cost basis
  PRIMARY KEY (val_date, account, commodity)
);
