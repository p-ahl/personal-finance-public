-- 0002_views: reporting surface. All views read posted entries only unless noted.
-- Parameterised reports (as-of, period) are done in the CLI with plain SQL over v_ledger.

CREATE VIEW v_ledger AS
SELECT
  l.id                 AS line_id,
  e.id                 AS entry_id,
  e.entry_date         AS date,
  CAST(substr(e.entry_date,1,4) AS INTEGER)                    AS year,
  substr(e.entry_date,1,7)                                     AS month,
  substr(e.entry_date,1,4) || '-Q' || ((CAST(substr(e.entry_date,6,2) AS INTEGER)+2)/3) AS quarter,
  e.description,
  e.entry_type,
  e.source,
  e.external_id,
  e.link_id,
  e.status,
  a.id                 AS account_id,
  a.name               AS account,
  a.level1, a.level2, a.level3,
  a.account_type,
  a.normal_balance,
  a.statement,
  a.report_line,
  a.cash_flow_class,
  a.valuation_only,
  i.name               AS institution,
  l.amount             AS amount_minor,               -- exact integer cents, debit-positive
  l.amount / 100.0     AS amount,                     -- convenience REAL for analysis (USD scale 2)
  CASE a.normal_balance WHEN 'credit' THEN -l.amount ELSE l.amount END AS natural_minor,
  l.currency,
  l.quantity,
  l.commodity,
  l.unit_price,
  COALESCE(lc.name, ec.name) AS counterparty,
  l.memo,
  l.category_source,
  l.raw_ref,
  (SELECT group_concat(d.kind || ':' || d.code, ',') FROM line_dimensions ld JOIN dimensions d ON d.id = ld.dimension_id WHERE ld.line_id = l.id) AS dimensions,
  (SELECT d.code FROM line_dimensions ld JOIN dimensions d ON d.id = ld.dimension_id WHERE ld.line_id = l.id AND d.kind = 'project' LIMIT 1) AS project
FROM journal_lines l
JOIN journal_entries e ON e.id = l.entry_id
JOIN accounts a        ON a.id = l.account_id
LEFT JOIN institutions i   ON i.id = a.institution_id
LEFT JOIN counterparties lc ON lc.id = l.counterparty_id
LEFT JOIN counterparties ec ON ec.id = e.counterparty_id
WHERE e.status = 'posted';

CREATE VIEW v_dim_date AS
WITH RECURSIVE d(date) AS (
  SELECT COALESCE((SELECT MIN(entry_date) FROM journal_entries), date('now'))
  UNION ALL
  SELECT date(date, '+1 day') FROM d
  WHERE date < COALESCE((SELECT MAX(entry_date) FROM journal_entries), date('now'))
)
SELECT
  date,
  CAST(strftime('%Y', date) AS INTEGER) AS year,
  (CAST(strftime('%m', date) AS INTEGER)+2)/3 AS quarter,
  CAST(strftime('%m', date) AS INTEGER) AS month_num,
  strftime('%Y-%m', date) AS month,
  date(date, 'start of month') AS month_start,
  date(date, 'start of month', '+1 month', '-1 day') AS month_end,
  CAST(strftime('%W', date) AS INTEGER) AS week,
  CAST(strftime('%w', date) AS INTEGER) AS dow
FROM d;

-- Any status: entries whose lines do not net to zero per currency, or that have < 2 lines.
CREATE VIEW v_unbalanced_entries AS
SELECT e.id AS entry_id, e.entry_date, e.description, e.status, l.currency, SUM(l.amount) AS imbalance_minor, COUNT(*) AS lines
FROM journal_entries e JOIN journal_lines l ON l.entry_id = e.id
GROUP BY e.id, l.currency
HAVING SUM(l.amount) <> 0
UNION ALL
SELECT e.id, e.entry_date, e.description, e.status, NULL, NULL, (SELECT COUNT(*) FROM journal_lines WHERE entry_id = e.id)
FROM journal_entries e
WHERE e.status = 'posted' AND (SELECT COUNT(*) FROM journal_lines WHERE entry_id = e.id) < 2;

CREATE VIEW v_account_balances AS
SELECT a.id AS account_id, a.name AS account, a.account_type, a.statement, a.report_line, a.valuation_only, a.tracking,
       COALESCE(SUM(v.amount_minor),0) AS balance_minor,
       COALESCE(SUM(v.natural_minor),0) AS natural_minor,
       COUNT(v.line_id) AS lines,
       MIN(v.date) AS first_date, MAX(v.date) AS last_date
FROM accounts a LEFT JOIN v_ledger v ON v.account_id = a.id
WHERE a.is_placeholder = 0
GROUP BY a.id;

CREATE VIEW v_trial_balance AS
SELECT account, account_type,
       SUM(CASE WHEN amount_minor > 0 THEN amount_minor ELSE 0 END)  AS debits_minor,
       SUM(CASE WHEN amount_minor < 0 THEN -amount_minor ELSE 0 END) AS credits_minor,
       SUM(amount_minor) AS balance_minor
FROM v_ledger GROUP BY account_id ORDER BY account;

-- Current balance sheet; net income is computed (no closing entries are stored).
CREATE VIEW v_balance_sheet AS
SELECT account_type AS section, report_line, account, SUM(natural_minor) AS amount_minor
FROM v_ledger WHERE statement = 'balance_sheet'
GROUP BY account_id
UNION ALL
SELECT 'equity', 'retained_earnings', 'Equity:Net-Income (computed)', -SUM(amount_minor)
FROM v_ledger WHERE statement = 'income_statement';

-- Income statement by month; natural sign (income +, expense +).
CREATE VIEW v_income_statement_monthly AS
SELECT month, account_type AS section, report_line, level2 AS category, account, SUM(natural_minor) AS amount_minor
FROM v_ledger WHERE statement = 'income_statement'
GROUP BY month, account_id;

-- Direct-method cash flow: for every entry that touches a cash account, the non-cash legs
-- explain the change in cash and are classified by their account's cash_flow_class.
-- In-transit legs (clearing account, entry has a link_id) are classified by looking through to
-- the account on the other side of the linked entry (e.g. bank -> InTransit -> credit card is a
-- credit-card payment). Bank-to-bank transfers split across days look through to a cash account
-- and are reported as 'transfer' (they net to zero once both legs post).
CREATE VIEW v_cash_flow_lines AS
WITH legs AS (
  SELECT v.*,
    CASE WHEN v.cash_flow_class = 'unclassified' AND v.link_id IS NOT NULL THEN
      (SELECT a3.id FROM journal_entries e3 JOIN journal_lines l3 ON l3.entry_id = e3.id JOIN accounts a3 ON a3.id = l3.account_id
       WHERE e3.link_id = v.link_id AND e3.id <> v.entry_id AND e3.status = 'posted' AND a3.id <> v.account_id
       ORDER BY l3.line_no LIMIT 1)
    END AS through_account_id
  FROM v_ledger v
  WHERE v.cash_flow_class <> 'cash'
    AND EXISTS (SELECT 1 FROM journal_lines l2 JOIN accounts a2 ON a2.id = l2.account_id
                WHERE l2.entry_id = v.entry_id AND a2.cash_flow_class = 'cash')
)
SELECT legs.*,
  CASE WHEN ta.id IS NULL THEN legs.cash_flow_class
       WHEN ta.cash_flow_class = 'cash' THEN 'transfer'
       ELSE ta.cash_flow_class END AS flow_class,
  CASE WHEN ta.id IS NULL THEN legs.report_line
       WHEN ta.cash_flow_class = 'cash' THEN 'Transfers between cash accounts (in transit)'
       ELSE ta.report_line END AS flow_line,
  -legs.amount_minor AS cash_effect_minor
FROM legs LEFT JOIN accounts ta ON ta.id = legs.through_account_id;

CREATE VIEW v_cash_flow_monthly AS
SELECT month, flow_class AS cash_flow_class, flow_line AS report_line, SUM(cash_effect_minor) AS amount_minor
FROM v_cash_flow_lines GROUP BY month, flow_class, flow_line;

CREATE VIEW v_balance_assertion_check AS
SELECT a.name AS account, b.as_of, b.source, b.amount AS expected_minor,
       COALESCE((SELECT SUM(l.amount) FROM journal_lines l JOIN journal_entries e ON e.id = l.entry_id
                 WHERE l.account_id = b.account_id AND e.status = 'posted' AND e.entry_date <= b.as_of AND l.currency = b.currency),0) AS actual_minor
FROM balance_assertions b JOIN accounts a ON a.id = b.account_id;

CREATE VIEW v_holdings AS
SELECT account, commodity, SUM(quantity) AS quantity_minor, SUM(amount_minor) AS book_value_minor
FROM v_ledger WHERE commodity IS NOT NULL
GROUP BY account_id, commodity;

CREATE VIEW v_uncategorized AS
SELECT * FROM v_ledger WHERE account IN ('Expenses:Uncategorized','Income:Uncategorized');
