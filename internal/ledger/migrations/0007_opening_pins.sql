-- 0007: pinned opening balances.
--
-- The opening of a transaction-tracked connector account used to be re-derived on every run as
-- "newest reported balance minus posted activity". Any activity the institution already counts
-- but that has not posted in the feed yet (Venmo payments whose bank funding debit lags) then
-- moved the opening and Equity:Opening-Balances instead of showing up as pending. An opening is
-- now established once and stored here; later builds use the pinned amount and book any gap to a
-- pending account (journal entry `pending:<connector account id>`, source 'pending', recomputed
-- every run, so it disappears once the missing row posts).
--
-- Pins are created by the pipeline: an account with a stored opening entry keeps that amount;
-- a new account gets the derived opening of its first build. `finance db pin-opening` corrects a
-- pin explicitly (with a note recording the evidence).
CREATE TABLE IF NOT EXISTS opening_pins (
  external_id  TEXT PRIMARY KEY,               -- opening:<connector account id>
  account      TEXT NOT NULL,
  amount       INTEGER NOT NULL,               -- cents, debit-positive
  as_of        TEXT NOT NULL,                  -- snapshot date the pin was established from
  note         TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
