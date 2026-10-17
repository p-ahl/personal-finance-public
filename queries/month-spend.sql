-- Expenses by category (account level 2) for one month, largest first, with a total row.
-- param: month — YYYY-MM (default: previous calendar month)
-- param: basis — accrual (default) | cash
WITH p AS (
  SELECT COALESCE(:month, strftime('%Y-%m', 'now', 'localtime', 'start of month', '-1 month')) AS month,
         COALESCE(:basis, 'accrual') AS basis
), s AS (
  SELECT COALESCE(l.level2, '(none)') AS category, COUNT(*) AS lines, SUM(l.natural_minor) AS amount_minor
  FROM v_pnl_lines l JOIN p ON l.basis = p.basis AND l.month = p.month
  WHERE l.account_type = 'expense'
  GROUP BY 1
)
SELECT month, category, lines, amount, pct FROM (
  SELECT 0 AS k, (SELECT month FROM p) AS month, category, lines, amount_minor / 100.0 AS amount,
         ROUND(100.0 * amount_minor / NULLIF((SELECT SUM(amount_minor) FROM s), 0), 1) AS pct
  FROM s
  UNION ALL
  SELECT 1, (SELECT month FROM p), 'TOTAL', COALESCE(SUM(lines), 0), COALESCE(SUM(amount_minor), 0) / 100.0, 100.0
  FROM s
)
ORDER BY k, amount DESC;
