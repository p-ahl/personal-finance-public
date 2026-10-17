-- Uncategorized journal lines, newest first.
-- param: from — YYYY-MM-DD (default: 90 days ago)
SELECT date, account, description, counterparty, amount_minor / 100.0 AS amount, raw_ref
FROM v_uncategorized
WHERE date >= COALESCE(:from, date('now', 'localtime', '-90 days'))
ORDER BY date DESC, line_id;
