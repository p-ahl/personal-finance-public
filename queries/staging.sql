-- Staging coverage per connector account: raw rows, pending, date span, newest balance snapshot.
-- Use after a nightly import to confirm history was kept and new rows arrived.
SELECT r.kind, COALESCE(a.name, r.connector_account_id) AS account,
       COUNT(*) AS raw_rows, SUM(r.pending) AS pending,
       MIN(r.txn_date) AS first_date, MAX(r.txn_date) AS last_date,
       SUM(r.first_run_id = (SELECT MAX(id) FROM import_runs WHERE source = 'connector')) AS new_in_last_import,
       (SELECT MAX(s.as_of) FROM balance_snapshots s WHERE s.connector_account_id = r.connector_account_id) AS snapshot_as_of
FROM raw_transactions r
LEFT JOIN accounts a ON a.external_id = r.connector_account_id
GROUP BY r.kind, r.connector_account_id
ORDER BY r.kind, account;
