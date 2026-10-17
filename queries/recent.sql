-- Journal lines of the last N days (default 30), newest first.
-- param: days — look-back window in days (default 30)
-- param: account — optional account-name prefix, e.g. Expenses:Food
SELECT date, account, description, counterparty,
       amount_minor / 100.0 AS amount, category_source, entry_type
FROM v_ledger
WHERE date >= date('now', 'localtime', '-' || COALESCE(:days, 30) || ' days')
  AND (:account IS NULL OR account LIKE :account || '%')
ORDER BY date DESC, entry_id DESC, line_id;
