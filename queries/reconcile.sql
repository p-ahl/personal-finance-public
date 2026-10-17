-- Ledger vs newest connector balance per account (debit-positive; liabilities are negative).
SELECT c.account, c.as_of, c.expected_minor / 100.0 AS connector, c.actual_minor / 100.0 AS ledger,
       (c.actual_minor - c.expected_minor) / 100.0 AS diff
FROM v_balance_assertion_check c
WHERE c.source = 'connector'
  AND c.as_of = (SELECT MAX(b.as_of) FROM v_balance_assertion_check b
                 WHERE b.account = c.account AND b.source = 'connector')
ORDER BY diff <> 0 DESC, c.account;
