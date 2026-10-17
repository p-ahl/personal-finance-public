-- 0003: accrual accounting + evidence documents.
--
-- journal_entries.entry_date is the RECOGNITION date (accrual basis). service_start/service_end
-- record the period the economic event covers (medical service dates, insurance term, tax year).
-- Accrual specs (local accruals.yaml) are materialized into `accruals`; `accrual_links` ties each
-- spec to its recognition entries and to the settlement entries (payments) that clear it, which
-- is what open-payable aging and cash-basis reporting are computed from.

ALTER TABLE journal_entries ADD COLUMN service_start TEXT
  CHECK (service_start IS NULL OR service_start GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]');
ALTER TABLE journal_entries ADD COLUMN service_end TEXT
  CHECK (service_end IS NULL OR service_end GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]');

-- Evidence: bills, EOBs, statements, receipts, invoices, paystubs, K-1s, tax forms, emails.
-- Files live outside the repo (finance-data/documents/<kind>/); local_path is relative to it.
CREATE TABLE documents (
  id              INTEGER PRIMARY KEY,
  doc_key         TEXT NOT NULL UNIQUE,         -- stable id from accruals.yaml
  kind            TEXT NOT NULL CHECK (kind IN ('bill','eob','statement','receipt','invoice','paystub','k1','tax_form','email','contract','other')),
  source          TEXT NOT NULL CHECK (source IN ('gmail','fastmail','drive','manual')),
  external_ref    TEXT,                         -- gmail msg/thread id, drive file id, ...
  doc_date        TEXT,
  counterparty_id INTEGER REFERENCES counterparties(id),
  amount_minor    INTEGER,
  currency        TEXT NOT NULL DEFAULT 'USD',
  service_start   TEXT,
  service_end     TEXT,
  title           TEXT,
  local_path      TEXT,
  notes           TEXT,
  managed         INTEGER NOT NULL DEFAULT 1    -- 1 = synced from config; 0 = entered by hand
);

CREATE TABLE entry_documents (
  entry_id    INTEGER NOT NULL REFERENCES journal_entries(id) ON DELETE CASCADE,
  document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  role        TEXT,
  managed     INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (entry_id, document_id)
);

CREATE TABLE accruals (
  id                 INTEGER PRIMARY KEY,
  accrual_key        TEXT NOT NULL UNIQUE,
  kind               TEXT NOT NULL CHECK (kind IN ('payable','allocate','interest','loan','journal')),
  description        TEXT,
  counterparty_id    INTEGER REFERENCES counterparties(id),
  pnl_account_id     INTEGER REFERENCES accounts(id),   -- expense/income recognized
  balance_account_id INTEGER REFERENCES accounts(id),   -- payable/accrued/prepaid/receivable
  service_start      TEXT,
  service_end        TEXT,
  amount_minor       INTEGER,
  basis              TEXT,                              -- evidence | inferred | estimate
  notes              TEXT
);

CREATE TABLE accrual_links (
  accrual_id INTEGER NOT NULL REFERENCES accruals(id) ON DELETE CASCADE,
  entry_id   INTEGER NOT NULL REFERENCES journal_entries(id) ON DELETE CASCADE,
  role       TEXT NOT NULL CHECK (role IN ('accrual','opening','settlement','journal')),
  PRIMARY KEY (accrual_id, entry_id)
);
CREATE INDEX accrual_links_entry ON accrual_links(entry_id);

-- Balance of every accrual on its balance account (debit-positive).
CREATE VIEW v_accrual_balances AS
SELECT ac.accrual_key, ac.kind, ac.description, cp.name AS counterparty,
  ba.name AS balance_account, ba.account_type, ba.level2 AS balance_class, pa.name AS pnl_account,
  ac.service_start, ac.service_end, ac.basis,
  MIN(CASE WHEN al.role IN ('accrual','opening') THEN e.entry_date END) AS recognized_on,
  MAX(CASE WHEN al.role = 'settlement' THEN e.entry_date END)           AS last_settled_on,
  SUM(CASE WHEN al.role IN ('accrual','opening') THEN l.amount ELSE 0 END) AS recognized_minor,
  SUM(CASE WHEN al.role = 'settlement' THEN l.amount ELSE 0 END)           AS settled_minor,
  SUM(l.amount) AS balance_minor
FROM accruals ac
JOIN accrual_links al   ON al.accrual_id = ac.id
JOIN journal_entries e  ON e.id = al.entry_id AND e.status = 'posted'
JOIN journal_lines l    ON l.entry_id = e.id AND l.account_id = ac.balance_account_id
JOIN accounts ba        ON ba.id = ac.balance_account_id
LEFT JOIN accounts pa   ON pa.id = ac.pnl_account_id
LEFT JOIN counterparties cp ON cp.id = ac.counterparty_id
GROUP BY ac.id;

-- Open payables / accrued liabilities (open_minor positive = owed). Aging is computed by the
-- report against its --as-of date from recognized_on.
CREATE VIEW v_open_payables AS
SELECT *, -balance_minor AS open_minor FROM v_accrual_balances
WHERE account_type = 'liability' AND balance_minor <> 0;

CREATE VIEW v_open_receivables AS
SELECT *, balance_minor AS open_minor FROM v_accrual_balances
WHERE account_type = 'asset' AND balance_class = 'Receivable' AND balance_minor <> 0;

CREATE VIEW v_prepaids AS
SELECT *, balance_minor AS open_minor FROM v_accrual_balances
WHERE account_type = 'asset' AND balance_class = 'Prepaid' AND balance_minor <> 0;

-- P&L lines on both bases.
--   accrual: every posted income/expense line (recognition date = entry_date).
--   cash:    income/expense lines of non-accrual entries, plus each settlement leg that hit an
--            accrual's balance account, re-expressed in that accrual's P&L account on the
--            payment date. Recognition entries (source 'accrual') are excluded.
CREATE VIEW v_pnl_lines AS
SELECT 'accrual' AS basis, date, month, account_type, report_line, level2, account, account_id, natural_minor, entry_id
FROM v_ledger WHERE statement = 'income_statement'
UNION ALL
SELECT 'cash', date, month, account_type, report_line, level2, account, account_id, natural_minor, entry_id
FROM v_ledger WHERE statement = 'income_statement' AND source <> 'accrual'
UNION ALL
SELECT 'cash', e.entry_date, substr(e.entry_date,1,7), pa.account_type, pa.report_line, pa.level2, pa.name, pa.id,
  CASE pa.normal_balance WHEN 'credit' THEN -l.amount ELSE l.amount END, e.id
FROM accrual_links al
JOIN accruals ac       ON ac.id = al.accrual_id AND al.role = 'settlement'
JOIN journal_entries e ON e.id = al.entry_id AND e.status = 'posted'
JOIN journal_lines l   ON l.entry_id = e.id AND l.account_id = ac.balance_account_id
JOIN accounts pa       ON pa.id = ac.pnl_account_id AND pa.statement = 'income_statement';

-- Accrual basis is the default income statement; the cash-basis view is kept alongside.
DROP VIEW v_income_statement_monthly;
CREATE VIEW v_income_statement_monthly AS
SELECT month, account_type AS section, report_line, level2 AS category, account, SUM(natural_minor) AS amount_minor
FROM v_pnl_lines WHERE basis = 'accrual' GROUP BY month, account_id;

CREATE VIEW v_income_statement_monthly_cash AS
SELECT month, account_type AS section, report_line, level2 AS category, account, SUM(natural_minor) AS amount_minor
FROM v_pnl_lines WHERE basis = 'cash' GROUP BY month, account_id;

CREATE VIEW v_accrual_vs_cash AS
SELECT month, account_type AS section, account,
  SUM(CASE WHEN basis = 'accrual' THEN natural_minor ELSE 0 END) AS accrual_minor,
  SUM(CASE WHEN basis = 'cash'    THEN natural_minor ELSE 0 END) AS cash_minor,
  SUM(CASE WHEN basis = 'accrual' THEN natural_minor ELSE -natural_minor END) AS diff_minor
FROM v_pnl_lines GROUP BY month, account_id;
