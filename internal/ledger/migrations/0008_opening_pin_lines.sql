-- 0008: pinned openings for investment accounts.
--
-- 0007 pinned only transaction-tracked accounts. Valuation-only and holdings-tracked investment
-- accounts still re-derived their opening every run (reported value / holdings minus known
-- activity), so a full-history pull that added older portfolio rows, or a market move, shifted
-- the opening and Equity:Opening-Balances instead of landing as activity / valuation. They are
-- pinned too now. A holdings-tracked opening is per position: `lines` holds the pinned opening
-- lines as JSON ([{"commodity","quantity","amount","memo"}], quantity at security scale, amount in
-- cents); empty for single-amount openings.
ALTER TABLE opening_pins ADD COLUMN lines TEXT NOT NULL DEFAULT '';
