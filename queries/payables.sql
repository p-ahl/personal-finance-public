-- Open payables and accrued liabilities (amount owed > 0), oldest recognition first.
-- (finance report payables --as-of D gives the same with aging buckets as of a past date.)
SELECT accrual_key, kind, counterparty, balance_account, pnl_account, recognized_on, last_settled_on,
       CAST(julianday('now', 'localtime') - julianday(recognized_on) AS INTEGER) AS age_days,
       open_minor / 100.0 AS open, basis
FROM v_open_payables
ORDER BY recognized_on, accrual_key;
