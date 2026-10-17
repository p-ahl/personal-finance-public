# Schema design: SQLite double-entry books

Status: implemented in `internal/ledger/migrations/0001_core.sql` (tables and triggers) and
`0002_views.sql` (reporting surface). Migrations are embedded in the binary and applied in order,
and each one is recorded in `schema_migrations`.

## 1. Research pass

| System | Core model | What we took | What we left |
|---|---|---|---|
| **hledger / ledger-cli** ([hledger manual](https://hledger.org/hledger.html)) | Plain-text journal. A transaction has ≥2 postings that must sum to zero per commodity. Accounts are colon-separated paths whose top level implies the type. Balance assertions are checked on load. | Colon-path account names (`Assets:Bank:…`) with the type derived from the top segment. Balance assertions are stored as rows (`balance_assertions`) and checked by `finance check`. Zero-sum per commodity. | Text files as the store. Inferred amounts. |
| **GnuCash SQL backend** ([wiki: SQL](https://wiki.gnucash.org/wiki/SQL), [piecash object model](https://piecash.readthedocs.io/en/stable/object_model.html)) | `transactions` 1─n `splits`. Each split has a **value** (in the transaction currency) and a **quantity** (in the account commodity), stored as num/denom integers. Accounts form a parent tree. Placeholder accounts. | Entry/line split. **value vs quantity**: `journal_lines.amount` is USD cents, and the optional `quantity` + `commodity` hold fund shares, so balances in USD always zero-sum while positions are tracked separately. Integer storage. Parent tree with placeholder flag. | num/denom pairs. We use a fixed scale per commodity (`commodities.scale`) instead: simpler SQL and still exact. |
| **ERPNext GL Entry** ([gl_entry.py](https://github.com/frappe/erpnext/blob/develop/erpnext/accounts/doctype/gl_entry/gl_entry.py), [General Ledger docs](https://docs.frappe.io/erpnext/general-ledger)) | One row per account movement, with separate debit/credit columns. Each row points back to its voucher. Party and accounting dimensions (cost center, project) sit on the row. Reports are queries over GL rows. | Line-level **counterparty** and **dimensions** (`line_dimensions` → `dimensions(kind, code)`, e.g. `project:NewBuild`). The `external_id` / `raw_ref` back-pointers act as the voucher link. | Separate debit and credit columns. A signed integer is easier to sum, and `v_trial_balance` derives the two columns. |
| **Odoo `account.move` / `account.move.line`** | Move (header) plus move lines with debit, credit and `balance`. State goes draft → posted, and posted moves are locked. | **draft → posted** lifecycle enforced by triggers. Posted entries are immutable. | Multi-company and reconciliation models. |
| **Formance Ledger** ([transactions](https://docs.formance.com/modules/ledger/core-concepts/transactions), [source/destination](https://docs.formance.com/modules/ledger/core-concepts/source-destination), [accounts](https://docs.formance.com/modules/ledger/core-concepts/accounts)) | Postings move an amount of an asset from a source to a destination. Assets are written like `USD/2`, meaning integer minor units with an explicit scale. Balances are derived and never stored. | **Integer minor units with an explicit scale** (`USD` scale 2, funds scale 6). Balances are always derived from lines, with no stored running totals. | The source/destination posting form. Multi-leg entries with signed amounts handle splits more naturally. |
| **Medici** ([flash-oss/medici](https://github.com/flash-oss/medici/)) | Journals made of transactions. Writes are rejected unless debits equal credits. Free-form `meta` and book/account paths. | Writes are rejected when unbalanced, enforced in the database (a trigger), not only in app code. | MongoDB. |
| **Fowler, accounting patterns** ([Accounting Narrative](https://martinfowler.com/eaaDev/AccountingNarrative.html), [Accounting Entry](https://martinfowler.com/eaaDev/AccountingEntry.html), [Accounting Transaction](https://martinfowler.com/eaaDev/AccountingTransaction.html), [accounting.pdf](https://martinfowler.com/apsupp/accounting.pdf)) | Entries are immutable. Mistakes are fixed with adjusting or reversing entries. A multi-legged transaction is validated as a whole before posting. **Posting rules** turn events into entries. | The builder **is** a posting-rule engine: staged connector events plus YAML rules give entries deterministically. Entries are validated as a whole when posted. Builder-managed entries are replaced (draft → delete → re-insert) only by the builder and only when their content hash changes. Manual entries are never touched. | Full reversal-only correction for builder output. Raw events are kept forever in staging, so re-deriving is safe and much simpler. |
| **Kimball dimensional modeling** ([transaction fact tables](https://www.kimballgroup.com/data-warehouse-business-intelligence-resources/kimball-techniques/dimensional-modeling-techniques/transaction-fact-table/), [Fact tables](https://www.kimballgroup.com/2008/11/fact-tables/), *Data Warehouse Toolkit* ch. 7 "Accounting") | GL journal at **line grain** as a transaction fact, with dimensions (date, account hierarchy, party, project). Periodic-snapshot fact for balances. | `v_ledger` is the line-grain fact with the dimensions flattened (date parts, account levels 1-3, report line, cash-flow class, institution, counterparty, project). `v_dim_date` is the date dimension. `balance_snapshots` and `balance_assertions` are the periodic snapshot. | Physical star schema and surrogate-key ETL. Views are enough at this volume (thousands of rows). |

### Tradeoffs considered

- **Signed amount vs debit/credit columns.** One signed integer (`amount`, debit-positive) makes
  zero-sum a single `SUM() = 0`, and balances a single `SUM`. Debit and credit columns are
  derived in `v_trial_balance`. `v_ledger.natural_minor` flips the sign for credit-normal
  accounts so income and liabilities read positive.
- **Integer minor units vs DECIMAL/REAL.** SQLite has no decimal type. REAL drifts
  (`0.1 + 0.2`). Integers are exact. Parsing is exact too (`internal/money`, tested). Cost-basis
  values with more than 2 decimals are rounded half away from zero once, and the rounding is
  documented on the lot.
- **Stored vs computed retained earnings.** No closing entries are stored. The balance sheet
  computes `Equity:Net-Income` from income-statement lines, so any period can be restated and
  the books never need a year-end close.
- **Re-derive vs append-only corrections.** Connector data changes: pending rows post, and
  categories and rules evolve. Staging keeps every raw row, and the journal is a deterministic
  function of staging plus config. Upsert by `external_id` with a content hash makes re-runs
  idempotent (0 inserted / 0 updated on a no-op). Changed entries are re-posted through the
  same triggers. Manual entries (`source = 'manual'`) are outside the builder's reach.
- **SQLite vs Postgres/DuckDB as the store.** This is a single user with small data. A single
  file with backups by copy, and triggers for integrity. DuckDB and pandas read it directly
  (§6). The pure-Go `modernc.org/sqlite` driver means no cgo.

## 2. Chosen design

```text
institutions ─┐
commodities ──┼─< accounts (tree: parent_id, depth, level1..3)
              │      account_type / normal_balance / statement   (from the top segment)
              │      report_line / cash_flow_class               (config/coa.yaml, inherited)
              │      external_id (connector account id), mask, valuation_only, tracking
counterparties┤
dimensions ───┤
              └─< journal_lines >── journal_entries (draft|posted|void, external_id UNIQUE, content_hash, link_id)
                      amount INTEGER (USD cents, debit +), quantity/commodity (optional), lot_id,
                      counterparty_id, category_source, raw_ref ──> raw_transactions
                      └─< line_dimensions >── dimensions(kind, code)
staging:   raw_transactions (posted + pending, verbatim JSON), balance_snapshots, raw_holdings
market:    prices, lots (aggregate cost basis only; is_aggregate = 1)
audit:     import_runs (stats JSON), categorization_rules (copy of rules in effect), schema_migrations
assertions: balance_assertions (expected debit-positive balance per account and date)
```

### Integrity (triggers in `0001_core.sql`)

| Trigger | Rule |
|---|---|
| `je_no_insert_posted` | Entries must be inserted as `draft`. |
| `je_post_requires_balance` | On `draft → posted`: ≥ 2 lines, `SUM(amount) = 0` per currency, no line on a placeholder account. |
| `jl_no_insert_on_posted` / `jl_no_update_on_posted` / `jl_no_delete_on_posted` | Lines of a posted entry are immutable. |
| `je_no_delete_posted` | Posted entries cannot be deleted (set to draft first; only the builder does this). |
| `CHECK` constraints | `quantity` and `commodity` both null or both set. Enumerations for type, status and cash-flow class. Date format. |

### Chart of accounts attributes

- `account_type`, `normal_balance`, `statement` come from the top segment
  (Assets / Liabilities / Equity / Income / Expenses).
- `report_line` and `cash_flow_class` (cash | operating | investing | financing | opening |
  unclassified) come from `config/coa.yaml`. They are inherited from the nearest listed ancestor,
  then from per-top-level defaults.
- Parents that are not listed and have no lines are created as **placeholders**, and posting to
  them is rejected.

### Views (`0002_views.sql`)

| View | Purpose |
|---|---|
| `v_ledger` | **The data-science surface.** One row per posted line. Includes date, year, month and quarter; account and levels 1-3; type and statement; report line; cash-flow class; valuation flag; institution; `amount_minor` (exact) and `amount` (REAL); `natural_minor`; commodity and quantity; counterparty; category_source; raw_ref; dimensions and project. |
| `v_dim_date` | Calendar dimension spanning the journal. |
| `v_trial_balance`, `v_account_balances` | Debits, credits and balance per account. |
| `v_balance_sheet` | Balance-sheet accounts plus computed `Equity:Net-Income`. |
| `v_income_statement_monthly` | Income and expense by month, category and account, natural sign. |
| `v_cash_flow_lines`, `v_cash_flow_monthly` | Direct method: the non-cash legs of entries that touch a `cash`-class account, classified by the counterpart's `cash_flow_class`. In-transit legs look through `link_id` to the real counterpart, so bank → clearing → card shows as a card payment. |
| `v_balance_assertion_check` | Expected vs actual balance per assertion. |
| `v_unbalanced_entries` | Must be empty. |
| `v_holdings`, `v_uncategorized` | Positions; lines on `*:Uncategorized`. |

Parameterised statements (balance sheet as of a date, income statement over a period, monthly
columns) are plain SQL over `v_ledger` in `internal/report`.

## 3. Posting rules (the builder)

All in `internal/books`. Inputs: staged rows, `config/coa.yaml`, `rules/*.example.yaml` (or the local `rules.personal.yaml`/`transfers.personal.yaml`), and the local
`accounts.yaml` / `rules.local.yaml`.

1. **Sign.** The connector's amount is user-facing: + means money in, − means money out. That
   equals the debit-positive amount on the connector account for both cash and liability
   accounts. A card charge of −50 credits the card (the liability grows) and debits the expense.
   Connector balances become ledger balances as `+balance` for cash and investment accounts and
   `−balance` for liabilities.
2. **Pending.** Pending rows are staged but never booked. When they post, they arrive with their
   own ids.
3. **Transfers.** Matching runs before categorization. The full rule, with an example, is in
   [`rules/transfers.example.yaml`](../rules/transfers.example.yaml):
   - Hints give a row a target set of accounts: by mask in the description, by logical key, by
     institution, or by account class.
   - Pairs need **equal and opposite amounts to the cent** in two accounts, within **10 days**
     if either side is hinted or **4 days** for un-hinted Plaid `TRANSFER_*` / `LOAN_PAYMENTS`
     rows.
   - Pairs are ranked by hint strength, then smallest date gap, then date and id, and accepted
     greedily.
   - A same-day pair becomes one two-leg entry. Different days give two entries through
     `Assets:Transfers:InTransit`, linked by `link_id`.
   - `funding` hints (bank → Venmo) also accept a same-sign pair: the bank pull that funded a
     Venmo payment, which is itself categorized.
   - A hinted row whose only target is a non-connector or valuation-only account (crypto
     exchanges, Fidelity cash, a closed SoFi vault) is booked one-sided against it.
   - Everything else that looks like a transfer goes to `Assets:Transfers:Unmatched` and is
     listed by `finance check`.
   - A row caught by an explicit category rule is not used for generic (un-hinted) pairing.
4. **Categorization** applies the first match in order: local rules, then repo rules, then Plaid
   detailed category, then Plaid primary category, then `*:Uncategorized`. `category_source`
   records which one fired, so coverage can be reported with and without the Plaid fallback.
5. **Investments.**
   - **Holdings-tracked accounts (Roth IRA):** the opening is the connector positions at
     connector cost basis, minus any later portfolio activity, plus a residual USD cash line. Cash
     that bank rows post straight into the account (e.g. contributions a local rule categorizes to
     it) counts as later activity too. It is dated the day before the first such row. Portfolio rows are booked as follows:
     - buy/sell: security leg plus cash leg in the same account;
     - cash/dividend: `Income:Investments:Dividends`;
     - fee: `Expenses:Fees:Investment`.
     A mark-to-market entry at as-of − 1 day books value − cost to
     `Income:Investments:Unrealized-Gains`. Prices are stored. Lots are **aggregate** cost basis
     only (`is_aggregate = 1`); no tax lots are invented.
   - **Valuation-only accounts:** a USD balance from an opening entry only.
6. **Openings.** Every connector account gets an explicit entry against
   `Equity:Opening-Balances`, even when the amount is zero.
   - The entry is dated the day before the account's first line, or `books_start` (2025-10-06)
     if the account has no lines.
   - The amount is the connector balance minus all booked activity, established **once** and
     then pinned (`opening_pins`) for every connector account. Since migration 0008 that includes
     valuation-only accounts (a later change in reported value that known flows do not explain is
     booked as `valuation:<connector account id>` to `Income:Investments:Unrealized-Gains`) and
     holdings-tracked accounts (pinned per position in `opening_pins.lines`; a holdings/activity
     difference goes to the pending account below), so new full-history portfolio rows land as
     activity, never as opening shifts. A security-less portfolio `cash` row is the brokerage side of
     an ACH in/out: if the same amount is already booked on the account within 5 days (bank-side
     transfer or local rule) it is not booked again; otherwise it parks in the transfer clearing
     account (never dividend income). Later runs never move a pinned opening: when the reported balance differs from
     pinned opening + booked activity (activity the institution counts that has not posted in the
     feed yet, e.g. Venmo payments whose bank/card funding debit lags), the gap is booked to a
     pending account (entry `pending:<connector account id>`, source `pending`, dated at the
     snapshot, recomputed every run): the mapping's `pending_account` (Venmo:
     `Liabilities:Pending:Venmo-Funding`), else `Liabilities:Pending:Unposted` (REVIEW). When the
     funding debit posts it is a funding transfer bank → Venmo, the gap becomes zero and the
     pending entry disappears; the payment's expense is counted once. So the ledger balance at the
     as-of date equals the connector balance exactly; `finance check` verifies this from
     `balance_assertions`. An existing database pins the stored openings on upgrade;
     `finance db pin-opening --account A --amount X --note EVIDENCE` corrects a pin.
   - Internal accounts can declare `assume_end_balance` to back into an opening (a closed vault
     ends at 0).
7. **Mortgage.** Bank payments go to `Liabilities:Mortgage:…`. Escrow disbursements in the loan
   feed are categorized: county taxes → `Expenses:Taxes:Property` (or settle a property-tax
   accrual, §3a), insurance → `Expenses:Insurance:Home` (or a prepaid), escrow interest →
   `Income:Interest:Escrow`. When a rate and P&I payment are known (from a 1098 or a statement),
   a `loan` accrual (§3a) splits each payment into `Expenses:Housing:MortgageInterest` and
   principal. Without one, the opening principal absorbs the interest (§7).
8. **Rule vs transfer hint.** A transfer hint normally captures a row before categorization. A
   rule with `override_transfer: true` beats the hints. Use it for a bank "XFER FROM DDA" that is
   really money from a third party's account, such as a family gift.

## 3a. Accrual layer (`0003_accrual.sql`, `internal/books/accrual.go`)

The connector feed is cash basis. The accrual layer turns it into accrual books, using a
**local** `$FINANCE_DATA/config/accruals.yaml`. This file is never committed; the synthetic
template is [`config/accruals.example.yaml`](../config/accruals.example.yaml). The layer has
two invariants:

- A connector row never changes amount or account. Only its counter-leg is re-targeted to a
  balance account, or split. Every connector account therefore still ties to its connector
  balance, because openings are derived *after* the accrual layer runs.
- Accrual-only entries carry `source = 'accrual'` and external ids `accrual:<id>`,
  `accrual:<id>:<YYYY-MM>` or `accrual:<id>:opening`. Re-imports are idempotent like any other
  entry.

| Kind | Entries produced |
|---|---|
| `payable` | One recognition entry, Dr `account` (expense) / Cr `balance` (`Liabilities:Payable:<Provider>`). It is dated `date`, else `service_end`. `amount` defaults to the sum of the settlements. Each **settlement** is a connector row (for example the card charge that paid the bill) whose expense leg is re-targeted to the payable. The card charge therefore clears the payable instead of hitting the P&L again, and the later card payoff from cash stays an ordinary transfer. If recognition falls before `books_start`, it is booked as an opening entry against `Equity:Opening-Balances`. |
| `allocate` | `amount` spread evenly over the months from `start` to `end`, one entry per month-end (the last month takes the rounding). The kind is the same for accruals and prepaids: `balance` decides whether the entry is an accrued liability (`Liabilities:Accrued:*`), a prepaid (`Assets:Prepaid:*`) or a receivable. Months before `books_start` roll into one opening entry, net of `prior_settled` (amounts paid before the data starts). Months after the as-of date are not recognized yet. Settlements are re-targeted to `balance`. `splits` carve out parts, for example a penalty → `Expenses:Taxes:Penalties`. |
| `interest` | Simple interest on `principal` at `rate_pct`, accrued at each month-end from `start`. The first month is pro-rated actual/365; later months are principal × rate / 12. Entries are Dr `account` / Cr `balance`. |
| `loan` | Every matched payment row on the loan account is split into an interest line (`account`) and principal. The split rolls back from the connector balance: `B_before = (B_after + P&I) / (1 + r/12)`, interest = `B_before × r/12`. The payment itself is unchanged. Optional escrow: with `escrow` (account) and `escrow_from`, payments on/after that date also carry an escrow line = payment − `pi_payment`, so principal = P&I − interest; `escrow_disbursements` (selectors) pick loan-account rows paid out of escrow (county tax, insurance, escrow interest), whose loan leg is offset into the escrow account (their counter-leg, e.g. a property-tax accrual settlement, is unchanged). Loans run after the other accruals. |
| `journal` | A fully specified non-cash entry (`lines`, which must sum to zero). Example: land bought with funds the seller lent directly. |

Optional `dimensions: ['kind:code', …]` on any accrual tag the recognized line(s) (the
`account` leg; every non-equity line of a `journal`), e.g. `project:<PROJECT>` on a design
invoice, so project reports see accrual-basis amounts. The payable/accrued balance line is not
tagged. A receivable is a `payable` whose settlement is an inflow: the amount comes out
negative, so the entry is Dr `balance` (receivable) / Cr `account` (income), or the opening
balance when recognized before `books_start`. Private or family loans go to
`Liabilities:Loan:<Lender>` (lender named only in local config).

Selectors (`txn:`) pick a connector row by raw `id`, or by `account` (ledger name or key) plus
`date` or `from`/`to`, a signed `amount` as booked, and a `match` regex. By default exactly one
row must match; `all: true` allows several; `optional: true` allows none (a standing settlement for installments not paid yet, e.g. the rest of a contracted fee, usually with `from:` after the last known payment). A transfer entry cannot be a settlement. All
arithmetic is integer cents, with `big.Rat` and half-away-from-zero rounding.

**Tables.**

| Table | Contents |
|---|---|
| `journal_entries.service_start / service_end` | The service period an entry belongs to. Set on accrual recognition entries. |
| `documents` | Evidence: bill, eob, statement, receipt, invoice, paystub, k1, tax_form, email, contract or other. Each row has a source (gmail, fastmail, drive or manual), `external_ref` (message or file id), date, counterparty, amount, service period and `local_path` under `$FINANCE_DATA/documents/<kind>/`. Rows declared in `accruals.yaml` are `managed = 1` and are upserted or deleted on every run. |
| `entry_documents` | Links entries to documents (role `support`). |
| `accruals` | One row per accrual spec: kind, P&L account, balance account, service period, amount, `basis` (evidence, inferred or estimate) and notes. |
| `accrual_links` | Links an accrual to its entries, with role `accrual`, `opening`, `settlement` or `journal`. |

**Views.**

| View | Purpose |
|---|---|
| `v_accrual_balances` | Per accrual: recognized, settled and open amounts on its balance account. |
| `v_open_payables` / `v_open_receivables` / `v_prepaids` | The open items. `finance report payables --as-of` adds aging buckets (0-30, 31-60, 61-90, 90+ days from recognition). |
| `v_pnl_lines` | P&L lines on both bases (`basis` is `accrual` or `cash`). **Accrual** = every posted P&L line. **Cash** = the same lines minus `source = 'accrual'` entries, plus each settlement leg re-expressed in its accrual's P&L account at the payment date. Cash-basis interest from a `loan` split stays, because it is part of the payment. |
| `v_income_statement_monthly` | Now **accrual** basis (the default). |
| `v_income_statement_monthly_cash` | Cash basis, kept for comparison and for tax-style views. |
| `v_accrual_vs_cash` | Accrual vs cash by month and account. |

**Reports.** The reports are `finance report is --basis accrual|cash`,
`finance report payables --as-of D` and `finance report accrual-vs-cash --from --to`. The
balance sheet is always accrual: it includes payables, accrued liabilities and prepaids.

## 3b. Item-level splits (`internal/books/splits.go`)

Optional **local** `$FINANCE_DATA/config/splits.local.yaml` (never committed; synthetic template
[`config/splits.example.yaml`](../config/splits.example.yaml)) itemizes single charges, for
example an online-store order split from its confirmation email. Each entry is keyed by the
connector transaction id (`txn`, the external id with or without `plaid:`). It replaces the one
categorized counter-leg with several lines, each with its own `account`, `amount`, `memo`
(e.g. the item name) and optional `dimensions` (e.g. `hsa:eligible`). Rules:

- The connector leg is untouched, so balances still tie. Line amounts are debit-positive like
  the counter-leg (refunds negative) and must sum to it **exactly**; the build fails otherwise.
  Tax, shipping and discounts must already be allocated into the lines.
- Lines carry `category_source = split:<source>`, keep the rule's counterparty and dimensions,
  and get tag rules (`tags:`) re-applied for their own account.
- Only categorized rows can be split. A split whose id matches no categorized row (unknown,
  still pending, or matched as a transfer) fails the build. A split row cannot also be an
  accrual settlement: the accrual layer rejects multi-leg entries.
- `finance categorize` prints how many rows were itemized.

## 3c. Exchange imports: Kraken (`0004_exchange.sql`, `internal/books/exchange.go`)

`finance download kraken` saves the read-only API responses verbatim (§ README "Kraken").
`finance import kraken --raw <pull>` stages them, cumulatively like `raw_transactions`:
- `raw_exchange_ledger`: one row per Kraken ledger id (amount, fee, balance as decimal strings,
  `local_date` in America/Denver, `commodity` mapped: XXBT → BTC, XXDG → DOGE, `.S/.F/.M/.HOLD`
  suffixes → the spot asset, USD variants → USD);
- `raw_exchange_trades`: fills from TradesHistory (audit trail; the ledger rows carry amounts);
- `exchange_balance_snapshots`: Balance per pull (`as_of` = pull folder date);
- `exchange_prices`: daily OHLC closes (UTC day) and the pull's ticker last price.

The builder replays the exchange ledger (grouped by `refid`, in time order) on every build:
- **Quantities** are booked in the `Assets:Crypto:Exchange:Kraken` account with a commodity and
  `quantity` at scale 10 (`books.CryptoScale`); USD is tracked at 1/10000 and booked by cent
  rounding of the running balance, so the account's USD cash equals Kraken's rounded balance.
- **Cost** uses FIFO lots per pool (Kraken, self-custody). Buys add lots at USD cost; the fee is
  expensed (`Expenses:Fees:Investment`); sells relieve lots and book `Income:Investments:
  Realized-Gains:ShortTerm|LongTerm` (held more than a year = long term); crypto → crypto
  trades value both sides at the day's close.
- **Staking/earn rewards** → `Income:Crypto:Staking` at the day's close.
- **Crypto withdrawals** move the lots at cost to the self-custody clearing account
  (`crypto_selfcustody` = `Assets:Crypto:SelfCustody:Unassigned`; the wallet layer, §3d,
  re-points them to the receiving wallet); the network fee
  is expensed at cost. **Crypto deposits** take self-custody lots back FIFO (books-period lots
  first, then lots withdrawn before the books via `Equity:Opening-Balances` at their exchange
  cost, then zero basis, which the import reports as a note). A deposit that the wallet layer
  traced to a wallet send takes that wallet's FIFO lots instead (cost carried through the
  wallet, e.g. XMR that went out to a wallet app and came back).
- **USD deposits/withdrawals** are matched to the bank rows that the `kraken` transfer hint
  booked one-sided against the Kraken account: equal amount to the cent within the hinted
  window (10 days), greedy by smallest gap. Same day: the bank entry stays and the Kraken side
  adds nothing. Different days: the bank row's Kraken leg is re-pointed to
  `Assets:Transfers:InTransit` and both entries share `link_id = xfer:<bank id>+kraken:<ledger
  id>`. A Kraken USD transfer with no bank row goes to `Assets:Transfers:Unmatched`.
- **Opening** (`opening:kraken`): history before `books_start` only updates state; the opening
  is USD cash plus every position at FIFO cost against `Equity:Opening-Balances`, dated the day
  before the first in-books activity (never before `books_start`).
- **Mark to market** (`valuation:kraken:<as-of>`): each commodity's ticker value minus its cost
  to `Income:Investments:Unrealized-Gains`. Ticker prices go to `prices` (source `kraken-ticker`).
- **Assertion**: one `balance_assertions` row (source `kraken`) at the pull date: rounded API USD
  plus Σ round(quantity × ticker). `finance check` also compares USD cash, each commodity
  quantity and the total against the newest pull (`report.ReconcileExchange`); any difference
  fails the check.

Entry ids: `kraken:<refid>` per event, `opening:kraken`, `valuation:kraken:<date>`.

### Coinbase (`finance import coinbase`, `internal/ledger/coinbase.go`)

Same tables and builder, `exchange = 'coinbase'`, account `Assets:Crypto:Exchange:Coinbase`
(CoA key `coinbase`). A pull folder (README "Coinbase") holds Coinbase MCP connector responses
and `events.csv`. Staging:
- `raw_exchange_ledger`: connector fills (two rows per fill, `ledger_id = fill:<entry>:<asset>`,
  `refid = fill:<order id>`, commission charged in the quote currency) plus `events.csv` rows
  (`ledger_id = ev:<id>`). The pull carries the full history, so rows absent from it are deleted.
- `exchange_balance_snapshots`: `coinbase_balance` available + hold per currency, overridden by
  the portfolio's spot positions (they include staked balances).
- `exchange_prices`: daily closes from `candles_<BASE>-USD.json` (UTC day), the ticker =
  best bid/ask mid at the pull, and USDC at par (`USDC-USD (par)`) for every day of the history.

Entry ids: `coinbase:<refid>`, `opening:coinbase`, `valuation:coinbase:<date>`.

## 3d. Self-custody wallets (`0005_wallets.sql`, `internal/books/wallets.go`)

`finance download wallets [--history]` reads the local `$FINANCE_DATA/config/wallets.yaml`
(template `config/wallets.data.example.yaml`) and saves public explorer responses to
`raw/wallets/<YYYY-MM-DD>/` (0600): Kaspa (api.kaspa.org; explicit addresses or receive/change
addresses derived from an account-level kpub with a gap limit), Bitcoin/Litecoin (Esplora),
Solana (public RPC, SOL + SPL tokens), Monero (the wallet app's CSV export, carried forward),
USD prices (Kraken public ticker, CoinGecko cross-check) and, with `--history`, every used
address's transactions and daily OHLC. `balances.json`, `labels.json`, `MANIFEST.md` and
`SHA256SUMS` never contain an extended key (fingerprint only); logs never print addresses.
`scripts/wallet_events.py <pull>` infers `events.csv` (heuristic, timing + USD amounts at
daily VWAP; confidence per row): `exchange_out` / `exchange_in` (matched to the staged Kraken
and Coinbase withdrawals/deposits), `transfer` (same transaction in two own wallets),
`roundtrip_out/_in` (out to an address that sends it back), `swap` (coin X out, coin Y in
within hours), `passthrough` (inside a configured window: someone else's money, never booked),
`unmatched_in/_out`.

`finance import wallets --raw <pull>` stages `wallets`, `wallet_identifiers` (addresses,
derived addresses and the kpub itself, copied from the local config: the only place besides the
config where they live), `wallet_balance_snapshots`, `wallet_prices` and `wallet_events`
(replaced as a whole), then rebuilds. The builder (`buildCrypto`) books the exchanges and
replays the wallets on one timeline; because a deposit's cost depends on the wallet replay and
the wallet's lots on the exchange withdrawals, the two are iterated to a fixed point.

- **Accounts**: one per wallet (`Assets:Crypto:SelfCustody:<Wallet>`), per coin when
  `per_coin` (`...:<Wallet>:<COIN>`). Statements show these names only.
- **Exchange withdrawal** received by a wallet: the exchange entry's self-custody line is
  re-pointed to the wallet account (lots keep the exchange's FIFO cost and dates).
- **Swap** (`wallet:<id>`): FIFO lots of X relieved, proceeds = market value of X given up
  (daily VWAP, estimate), short/long-term gain to `Income:Investments:Realized-Gains:*`, new
  lot of Y at that value. **Transfer / round trip**: lots move at cost; network fees expensed.
- **Unmatched**: inflows enter at market value, outflows leave at FIFO cost, both against the
  clearing account (no P&L).
- **Opening** (`opening:wallets`): history before `books_start` only updates state; the wallet
  holdings at `books_start` at replayed cost against `Equity:Opening-Balances`.
- **At-cost adjustment** (`wallets:adjust:<pull date>`): sets every wallet account's quantity
  to the on-chain balance (dust, fees, untraced items) and moves whatever cost is left in the
  clearing account (withdrawals not found in any wallet, unmatched items) into the wallet
  holdings pro rata by market value. Total cost is conserved; no P&L. It is a true-up computed
  from the ledger on that date (earlier pulls' adjustments included), so a later pull with the
  same on-chain data books nothing and a rerun reproduces the same entries.
- **Mark to market** (`valuation:wallets:<date>`) to `Income:Investments:Unrealized-Gains`, dated at
  every valuation date: each month-end after the first wallet activity, and every pull date. Each
  mark values the ledger quantity of every wallet account/coin at that date's price and books the
  difference to its carrying value (cost plus earlier marks), so each period's Profit and Loss
  carries only that period's market move (the whole-holding change is not dated on the newest
  pull). Price on a date = the pull's ticker price, else the Kraken daily close stored in
  `crypto_price_history` (staged from each pull's `history/ohlc_<COIN>.json`, UTC-day candles), else
  the latest close up to 10 days earlier; a coin with no price is left at cost with a note.
  Cost basis stays in the lots (the ledger's own cost, marks excluded in `crypto_valuations`).
  One balance assertion per wallet account at the newest pull (`source = wallets`, including zero
  balances). `finance check` compares each wallet account's ledger quantity with the on-chain
  balance (hard failure on any difference) and reports the clearing balance (REVIEW if not zero).
- `crypto_valuations` (`0009_crypto_valuations.sql`, rebuilt each import): per day from books start
  and per wallet account/coin: quantity, price (and its source), value and cost basis in cents,
  for charting value against cost (`scripts/crypto_history.py`).
- The import prints realized gain/loss on wallet swaps by year (all swaps, including those
  before the books, as estimates) and writes `<pull>/events-booked.csv`;
  `wallet_events.py <pull> --timeline-md/--timeline-csv` renders the local timeline.

## 4. Idempotency

- `raw_transactions` is keyed by connector transaction id. Unchanged rows only bump
  `last_run_id`.
- Entries are keyed by `external_id`: `plaid:<id>`, `plaid:<idA>+<idB>`,
  `opening:<connector account id>`, `valuation:<acct>:<date>`. Each carries a `content_hash` of
  its canonical JSON. On re-run:
  - unchanged → skip;
  - changed → draft, replace lines, post again (through the triggers);
  - no longer produced → removed.
- Accrual metadata (`accruals`, `accrual_links`, managed `documents` and `entry_documents`) is
  rewritten from `accruals.yaml` on every run. Documents no longer declared are deleted.
- Staging is cumulative: an incremental extract (for example a nightly pull of the last ~10
  days) only adds or updates rows, so older history stays. The build uses the **newest balance
  snapshot per account** (`balance_snapshots`) and the newest holdings pull. Openings are
  "snapshot balance minus activity dated on or before the snapshot". Connector
  `balance_assertions` are replaced on every run by the current set, one per account at its
  snapshot date. The history stays in `balance_snapshots`.
- Openings of transaction-tracked accounts are pinned, so a gap in history no longer hides in
  the opening: it shows as a **pending** gap (see §6 of the build; `unreconciled` = REVIEW when
  the account has no `pending_account`). **Opening drift** (a pinned opening changed in this
  run) only happens through an explicit `finance db pin-opening`. It also counts staged rows inside the extract's date span that
  the extract no longer contains (`unseen` posted / pending).
- Every run writes an `import_runs` row with stats. A second identical import reports
  `0 inserted, 0 updated, 0 deleted` (tested).

## 5. Tests (synthetic fixtures only: `internal/testfixture`)

These cover:
- the zero-sum and immutability triggers;
- migration idempotency;
- exact decimal parsing and no float drift;
- connector dedup and pending → posted;
- sign handling for cash vs liability;
- opening entries (including zero and no-activity accounts);
- same-day, in-transit and funding transfer pairs plus unmatched;
- rule order (local overlay beats repo, then Plaid fallback, then uncategorized);
- `{institution}` templates;
- holdings opening at cost plus mark-to-market;
- idempotent import, and a rule change updating exactly the affected entries;
- incremental nightly import: cumulative staging, superseded pending rows, an account missing
  from the latest extract, holdings kept from the last pull, reconciliation on the newest
  snapshot per account, and opening drift when a balance does not follow from history;
- `finance sql`: read-only enforcement, attach, renderers, and every saved query;
- accruals: payable recognition plus settlement re-targeting, with connector accounts still
  tying; allocate with pre-books opening and future months skipped; interest pro-rating; the
  loan split; journal entries; unique-selector errors; `v_pnl_lines` accrual vs cash; and
  `override_transfer`.
- item-level splits: itemized counter-legs with memos, dimensions and `split:<source>`, the
  connector leg untouched and balances still tying; a missing file is a no-op; and errors for a
  sum off by a cent, an unknown or transfer txn, a date mismatch, duplicates, bad dimensions,
  bad accounts and unknown fields.
- Kraken: request signing against the documented test vector, nonce ordering, the read-only
  method allowlist, permission errors naming the missing key permission, rate-limit retry, no
  credentials in errors; download paging (`ofs`), verbatim 0600 files, MANIFEST/SHA256SUMS and
  identical re-runs; parsing and asset mapping; the import of a synthetic history (pre-books
  opening, in-transit and same-day USD transfers, trades with fees and gains, staking, crypto
  withdrawal and deposit, mark to market) reconciling with the API balances, idempotency, a
  reconciliation failure when the API reports a different balance, and rejected edited files.

## 6. Reading the database from DuckDB / pandas

It is a plain SQLite file. Use the views and integer cents:

```python
import sqlite3, pandas as pd
con = sqlite3.connect("file:/workspace/finance-data/finance.db?mode=ro", uri=True)
lines = pd.read_sql("SELECT * FROM v_ledger", con, parse_dates=["date"])
```

```sql
INSTALL sqlite; LOAD sqlite;
ATTACH '/workspace/finance-data/finance.db' AS f (TYPE sqlite, READ_ONLY);
SELECT month, level2 AS category, SUM(amount_minor)/100.0 AS usd
FROM f.v_ledger WHERE account_type = 'expense' GROUP BY ALL ORDER BY ALL;
```

Use `amount_minor` for anything that must tie out. `amount` (REAL) is for plotting only.

## 7. Known limitations of the model

- **Mortgage interest:** without a `loan` accrual, payments reduce the liability in full, so
  interest is absorbed into the opening balance. With one, the split depends on the configured
  rate and P&I payment. If those are derived rather than read from a statement, interest per
  month is approximate, although the balance stays exact. Escrow is tracked from the loan
  accrual's `escrow_from` (payment − P&I in, disbursements out); the escrow balance before that
  date is not known without a statement, and the connector opening absorbs it.
- **Accruals are only as good as `accruals.yaml`.** Recognition dates and amounts come from
  documents (bills, EOBs, tax returns), and each spec carries a `basis` tag (evidence, inferred
  or estimate). Current-year tax accruals are estimates until the return is filed.
- **Valuation-only accounts** (Fidelity Rollover IRA, Joint, Individual; SoFi self-directed)
  have no activity, so their growth is invisible inside the period.
- **Non-connector assets** (Strike, SoFi crypto, the land lot) are carried at net
  contributions or cost, not market value. Kraken (API) and Coinbase (saved connector
  responses + email evidence) are imported (§3c) and marked to market;
  Self-custody wallets (§3d) are reconciled to on-chain balances and marked to market; their
  cost basis and swap gains are estimates (swaps inferred from timing and amounts, daily
  prices), and untraced items are absorbed at cost by the at-cost adjustment.
- **History starts 2025-10-07** (connector limit). Earlier balances are opening entries.

### Tag rules and project cost reports

- `tags:` in `rules.personal.yaml` or `rules.local.yaml` adds dimensions to the categorized line of matching rows without changing the account. Conditions (all optional, all must hold): `match`/`exclude` (regex over name | merchant | description), `account` (regex over the categorized account), `institution`, `from`/`to` (inclusive dates). Use it for trips (`trip:<yyyy>-<place>-<season>`) where spending is spread across many categories.
- Trips paid ahead (flights for a later month) go to `Assets:Prepaid:Travel` and are released to `Expenses:Travel` by a `journal` accrual dated on the trip start.
- Home builds: `Assets:RealEstate:<Project>:Land` and `Assets:RealEstate:<Project>:CIP:<CostCode>`, lines tagged `project:<code>` plus `cost:<code>`. Co-owner shares are `payable` accruals with a negative amount (credit CIP, debit `Assets:Receivable:<CoOwner>`).
- `finance report project --project <code> [--as-of D] [--budget <local>/budgets.yaml]` shows cost to date by cost code (capitalized vs expensed), the unpaid part (open payable accruals), and committed/budget/forecast from the optional budgets file, plus a detail list with evidence document keys.
