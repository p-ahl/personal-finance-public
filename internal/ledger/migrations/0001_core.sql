-- 0001_core: chart of accounts, journal, staging, reconciliation.
-- Amounts are INTEGER minor units (USD cents) with a per-commodity scale. Never REAL.
-- Sign convention: debit-positive (assets/expenses grow with +, liabilities/equity/income grow with -).

CREATE TABLE institutions (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,          -- short name used in account paths (ExampleBank, ...)
  connector_id  TEXT UNIQUE,                   -- connector connection id
  connector_institution_id TEXT,
  display_name  TEXT,
  provider      TEXT
);

CREATE TABLE commodities (
  code   TEXT PRIMARY KEY,                     -- USD, FDEWX, SPAXX, KAS
  scale  INTEGER NOT NULL CHECK (scale BETWEEN 0 AND 12),  -- minor-unit digits: USD=2
  kind   TEXT NOT NULL CHECK (kind IN ('currency','fund','stock','crypto','other')),
  name   TEXT
);

CREATE TABLE accounts (
  id              INTEGER PRIMARY KEY,
  name            TEXT NOT NULL UNIQUE,        -- hierarchical, Beancount-compatible: Assets:Bank:ExampleBank:Checking-1234
  parent_id       INTEGER REFERENCES accounts(id),
  depth           INTEGER NOT NULL,            -- 1 = top level (Assets)
  level1          TEXT NOT NULL,               -- Assets | Liabilities | Equity | Income | Expenses
  level2          TEXT,                        -- e.g. Bank, Food, Salary
  level3          TEXT,
  account_type    TEXT NOT NULL CHECK (account_type IN ('asset','liability','equity','income','expense')),
  normal_balance  TEXT NOT NULL CHECK (normal_balance IN ('debit','credit')),
  statement       TEXT NOT NULL CHECK (statement IN ('balance_sheet','income_statement')),
  report_line     TEXT NOT NULL,               -- presentation line (cash, retirement, credit_card, salary, housing, ...)
  cash_flow_class TEXT NOT NULL CHECK (cash_flow_class IN ('cash','operating','investing','financing','opening','unclassified')),
  currency        TEXT NOT NULL DEFAULT 'USD' REFERENCES commodities(code),
  institution_id  INTEGER REFERENCES institutions(id),
  external_id     TEXT UNIQUE,                 -- connector account id (NULL for internal/category accounts)
  mask            TEXT,
  is_placeholder  INTEGER NOT NULL DEFAULT 0,  -- parent nodes; no lines allowed
  valuation_only  INTEGER NOT NULL DEFAULT 0,  -- balance known, activity/holdings unknown
  tracking        TEXT NOT NULL DEFAULT 'none' CHECK (tracking IN ('connector','holdings','derived','none')),
  opened_on       TEXT,
  closed_on       TEXT,
  description     TEXT
);
CREATE INDEX accounts_parent ON accounts(parent_id);

CREATE TABLE counterparties (
  id    INTEGER PRIMARY KEY,
  name  TEXT NOT NULL UNIQUE,
  kind  TEXT NOT NULL DEFAULT 'merchant' CHECK (kind IN ('merchant','person','institution','employer','government','self','other'))
);

CREATE TABLE dimensions (
  id    INTEGER PRIMARY KEY,
  kind  TEXT NOT NULL,                         -- project | person | property | trip ...
  code  TEXT NOT NULL,
  name  TEXT,
  UNIQUE (kind, code)
);

CREATE TABLE journal_entries (
  id               INTEGER PRIMARY KEY,
  entry_date       TEXT NOT NULL CHECK (entry_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  posted_at        TEXT,
  description      TEXT NOT NULL DEFAULT '',
  counterparty_id  INTEGER REFERENCES counterparties(id),
  entry_type       TEXT NOT NULL DEFAULT 'normal' CHECK (entry_type IN ('normal','transfer','opening','valuation','adjustment')),
  source           TEXT NOT NULL,              -- connector | opening | valuation | manual
  external_id      TEXT UNIQUE,                -- idempotency key, e.g. plaid:<txn id>, opening:<account>
  link_id          TEXT,                       -- groups the legs of a transfer split across dates
  status           TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','posted','void')),
  content_hash     TEXT,                       -- builder-managed entries: detect changes on re-run
  import_run_id    INTEGER REFERENCES import_runs(id),
  created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX journal_entries_date ON journal_entries(entry_date);
CREATE INDEX journal_entries_link ON journal_entries(link_id);

CREATE TABLE journal_lines (
  id               INTEGER PRIMARY KEY,
  entry_id         INTEGER NOT NULL REFERENCES journal_entries(id) ON DELETE CASCADE,
  line_no          INTEGER NOT NULL,
  account_id       INTEGER NOT NULL REFERENCES accounts(id),
  amount           INTEGER NOT NULL,           -- value in `currency` minor units, debit-positive; zero-sum per entry per currency
  currency         TEXT NOT NULL DEFAULT 'USD' REFERENCES commodities(code),
  quantity         INTEGER,                    -- optional units of `commodity` (minor units at commodity scale), e.g. fund shares
  commodity        TEXT REFERENCES commodities(code),
  unit_price       TEXT,                       -- decimal string, informational
  lot_id           INTEGER REFERENCES lots(id),
  counterparty_id  INTEGER REFERENCES counterparties(id),
  memo             TEXT,
  category_source  TEXT,                       -- rule:<name> | plaid:<category> | transfer | opening | fallback
  raw_ref          TEXT,                       -- connector transaction id the line came from
  UNIQUE (entry_id, line_no),
  CHECK ((quantity IS NULL) = (commodity IS NULL))
);
CREATE INDEX journal_lines_account ON journal_lines(account_id);
CREATE INDEX journal_lines_entry ON journal_lines(entry_id);

CREATE TABLE line_dimensions (
  line_id       INTEGER NOT NULL REFERENCES journal_lines(id) ON DELETE CASCADE,
  dimension_id  INTEGER NOT NULL REFERENCES dimensions(id),
  PRIMARY KEY (line_id, dimension_id)
);

CREATE TABLE lots (
  id            INTEGER PRIMARY KEY,
  account_id    INTEGER NOT NULL REFERENCES accounts(id),
  commodity     TEXT NOT NULL REFERENCES commodities(code),
  acquired_on   TEXT,                          -- NULL when unknown
  quantity      INTEGER NOT NULL,              -- commodity minor units
  cost_total    INTEGER,                       -- USD cents; NULL when unknown
  is_aggregate  INTEGER NOT NULL DEFAULT 0,    -- 1 = position-level cost basis from the connector, not a real tax lot
  source        TEXT NOT NULL,
  as_of         TEXT,
  note          TEXT,
  UNIQUE (account_id, commodity, source, as_of)
);

CREATE TABLE prices (
  commodity   TEXT NOT NULL REFERENCES commodities(code),
  price_date  TEXT NOT NULL,
  price       TEXT NOT NULL,                   -- decimal string in `currency`
  currency    TEXT NOT NULL DEFAULT 'USD' REFERENCES commodities(code),
  source      TEXT NOT NULL,
  PRIMARY KEY (commodity, price_date, source)
);

CREATE TABLE import_runs (
  id           INTEGER PRIMARY KEY,
  started_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  finished_at  TEXT,
  source       TEXT NOT NULL,
  raw_dir      TEXT,
  as_of        TEXT,
  stats_json   TEXT,
  status       TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running','ok','failed'))
);

-- Verbatim connector rows (posted AND pending). Never deleted; the journal is derived from these.
CREATE TABLE raw_transactions (
  connector_txn_id      TEXT PRIMARY KEY,
  kind                  TEXT NOT NULL CHECK (kind IN ('cash','liability','portfolio')),
  connector_account_id  TEXT NOT NULL,
  txn_date              TEXT NOT NULL,
  amount                TEXT NOT NULL,         -- decimal string exactly as received
  pending               INTEGER NOT NULL,
  raw_json              TEXT NOT NULL,
  first_run_id          INTEGER REFERENCES import_runs(id),
  last_run_id           INTEGER REFERENCES import_runs(id)
);
CREATE INDEX raw_transactions_account ON raw_transactions(connector_account_id, txn_date);

CREATE TABLE raw_holdings (
  as_of                 TEXT NOT NULL,
  connector_holding_id  TEXT NOT NULL,
  connector_account_id  TEXT NOT NULL,
  raw_json              TEXT NOT NULL,
  import_run_id         INTEGER REFERENCES import_runs(id),
  PRIMARY KEY (as_of, connector_holding_id)
);

-- Connector-reported balances per account per extract (reconciliation source).
CREATE TABLE balance_snapshots (
  connector_account_id  TEXT NOT NULL,
  as_of                 TEXT NOT NULL,
  current_balance       TEXT,
  available_balance     TEXT,
  currency              TEXT,
  raw_json              TEXT NOT NULL,
  import_run_id         INTEGER REFERENCES import_runs(id),
  PRIMARY KEY (connector_account_id, as_of)
);

-- Expected ledger balance (debit-positive, minor units) inclusive of all entries dated <= as_of.
CREATE TABLE balance_assertions (
  account_id  INTEGER NOT NULL REFERENCES accounts(id),
  as_of       TEXT NOT NULL,
  amount      INTEGER NOT NULL,
  currency    TEXT NOT NULL DEFAULT 'USD' REFERENCES commodities(code),
  source      TEXT NOT NULL,
  PRIMARY KEY (account_id, as_of, currency, source)
);

-- Snapshot of the categorization rules used (the YAML in the repo is the source; this is an audit copy).
CREATE TABLE categorization_rules (
  id        INTEGER PRIMARY KEY,
  priority  INTEGER NOT NULL,
  name      TEXT NOT NULL,
  origin    TEXT NOT NULL,                     -- repo | local | plaid_fallback
  spec_json TEXT NOT NULL,
  UNIQUE (origin, name)
);

-- ===== Double-entry enforcement =====
-- Entries are created as draft, lines added, then status flipped to posted.
CREATE TRIGGER je_no_insert_posted BEFORE INSERT ON journal_entries
WHEN NEW.status = 'posted'
BEGIN SELECT RAISE(ABORT, 'insert entries as draft, add lines, then post'); END;

CREATE TRIGGER je_post_requires_balance BEFORE UPDATE OF status ON journal_entries
WHEN NEW.status = 'posted' AND OLD.status <> 'posted'
BEGIN
  SELECT RAISE(ABORT, 'cannot post: fewer than 2 lines')
  WHERE (SELECT COUNT(*) FROM journal_lines WHERE entry_id = NEW.id) < 2;
  SELECT RAISE(ABORT, 'cannot post: lines do not sum to zero per currency')
  WHERE EXISTS (SELECT 1 FROM journal_lines WHERE entry_id = NEW.id GROUP BY currency HAVING SUM(amount) <> 0);
  SELECT RAISE(ABORT, 'cannot post: line on placeholder account')
  WHERE EXISTS (SELECT 1 FROM journal_lines l JOIN accounts a ON a.id = l.account_id WHERE l.entry_id = NEW.id AND a.is_placeholder = 1);
END;

CREATE TRIGGER jl_no_insert_on_posted BEFORE INSERT ON journal_lines
WHEN (SELECT status FROM journal_entries WHERE id = NEW.entry_id) = 'posted'
BEGIN SELECT RAISE(ABORT, 'entry is posted; set it to draft first'); END;

CREATE TRIGGER jl_no_update_on_posted BEFORE UPDATE ON journal_lines
WHEN (SELECT status FROM journal_entries WHERE id = OLD.entry_id) = 'posted'
  OR (SELECT status FROM journal_entries WHERE id = NEW.entry_id) = 'posted'
BEGIN SELECT RAISE(ABORT, 'entry is posted; set it to draft first'); END;

CREATE TRIGGER jl_no_delete_on_posted BEFORE DELETE ON journal_lines
WHEN (SELECT status FROM journal_entries WHERE id = OLD.entry_id) = 'posted'
BEGIN SELECT RAISE(ABORT, 'entry is posted; set it to draft first'); END;

CREATE TRIGGER je_no_delete_posted BEFORE DELETE ON journal_entries
WHEN OLD.status = 'posted'
BEGIN SELECT RAISE(ABORT, 'entry is posted; set it to draft first'); END;
