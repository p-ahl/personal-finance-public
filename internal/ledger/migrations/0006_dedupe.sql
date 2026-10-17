-- 0006: idempotent staging (duplicate-proof re-imports).
--
-- Dedupe keys. Every staged transaction has a stable key: raw_transactions.connector_txn_id is the
-- connector's own transaction id, or, for a row that arrives without one, the synthetic key
-- "h-<hash of normalized kind/account/date/amount/name/merchant/description>-<occurrence in its
-- page file>" (internal/connector). Staging upserts by that key (PRIMARY KEY), exchange rows by
-- (exchange, ledger_id), wallet events by id, and journal entries by external_id (UNIQUE) with a
-- content hash, so re-importing the same or an overlapping extract is a no-op.
--
-- Re-keyed rows. Institutions occasionally re-issue a posted transaction under a new id (account
-- re-link, provider migration). Staging never deletes rows, so without this the old and the new
-- copy would both be booked. superseded_by records the id of the row that replaced a staged row:
-- set when a posted row inside the new extract's covered window (per account) is absent from it
-- AND an identical row (same account, date, amount and normalized name) first appears in that
-- extract under a new id. Superseded rows stay in staging for audit but are not booked. A row
-- that shows up again is un-superseded. Identical rows that are all present in the newest
-- extract (two same-day airline tickets) are never touched.
ALTER TABLE raw_transactions ADD COLUMN superseded_by TEXT;
CREATE INDEX raw_transactions_superseded ON raw_transactions(superseded_by);
