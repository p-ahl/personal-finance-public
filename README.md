# personal-finance

Personal double-entry books in **SQLite**, built by a Go CLI (`cmd/finance`), plus downloaders.
The SQLite database is the only source of truth. Beancount is no longer used.

- Design, research notes and tradeoffs: [`docs/schema.md`](docs/schema.md)
- Code lives in this repo. **Data never does** (see [Where data lives](#where-data-lives)).

## Architecture

```text
Finance connector (Plaid-backed)          repo (code + templates)          data dir (local only)
──────────────────────────────           ───────────────────────          ─────────────────────
finance_list_accounts / *_transactions   cmd/finance  (CLI)                raw/finance/YYYY-MM-DD/   raw pages + MANIFEST
finance_list_portfolio_holdings   ──▶    internal/connector  read+dedup ─▶ finance.db               SQLite books
                                         internal/books      build      │  config/accounts.yaml     connector id map
                                         internal/ledger     schema/sync│  config/rules.*.yaml     personal rules
                                         internal/report     statements ◀┘
                                         config/coa.yaml, rules/*.example.yaml
```

Pipeline (each step is deterministic and idempotent):

1. **Extract**: connector pages are saved verbatim to `raw/finance/<date>/` (see below).
2. **Stage**: `finance import connector --raw <dir>` copies every row into `raw_transactions`,
   `balance_snapshots` and `raw_holdings`. Pending rows are staged but never booked.
3. **Build**: `internal/books` turns the staged rows plus YAML config into balanced journal entries:
   transfer matching, categorization, holdings, and one opening entry per account.
4. **Sync**: `internal/ledger` upserts the entries by `external_id` and content hash. A re-run
   with unchanged inputs writes nothing. Triggers enforce zero-sum and immutability when an entry
   is posted.
5. **Report / check**: SQL views (`v_ledger`, `v_trial_balance`, `v_balance_sheet`, …) and the
   `finance report` / `finance check` commands.

## Setup

```bash
mise trust && mise install            # Go 1.22 via mise.toml
go build -o bin/finance ./cmd/finance
export FINANCE_DATA=/workspace/finance-data   # defaults to ../finance-data next to the repo
mkdir -p "$FINANCE_DATA/config"
cp config/accounts.example.yaml     "$FINANCE_DATA/config/accounts.yaml"      # fill in connector ids
cp config/rules.local.example.yaml  "$FINANCE_DATA/config/rules.local.yaml"   # optional: extra personal rules (evaluated first)
cp rules/categories.example.yaml     "$FINANCE_DATA/config/rules.personal.yaml"   # optional: your full rule set (replaces the example)
cp rules/transfers.example.yaml      "$FINANCE_DATA/config/transfers.personal.yaml" # optional: your transfer hints (replaces the example)
cp config/coa.local.example.yaml     "$FINANCE_DATA/config/coa.local.yaml"        # optional: personal accounts added to config/coa.yaml
cp config/report.example.yaml        "$FINANCE_DATA/config/report.local.yaml"     # optional: names/labels/goal for the weekly report, charts, statements
cp config/accruals.example.yaml      "$FINANCE_DATA/config/accruals.yaml"      # optional: accrual layer
cp config/splits.example.yaml        "$FINANCE_DATA/config/splits.local.yaml"  # optional: item-level splits
```

The SQLite driver is pure Go (`modernc.org/sqlite`), so no cgo or system SQLite is needed.

## Usage

```bash
finance db migrate
finance import connector --raw "$FINANCE_DATA/raw/finance/2026-10-08"   # idempotent, cumulative
finance categorize                 # after editing the rule files: rebuild from staging
finance transfers match -v         # re-run matching and list the pairs
finance check                      # zero-sum, reconciliation vs connector, unmatched, uncategorized

finance report bs  --as-of 2026-10-07
finance report is  --from 2026-07-01 --to 2026-09-30 --monthly [--detail] [--basis accrual|cash]
finance report cf  --from 2026-07-01 --to 2026-09-30 --monthly
finance report tb  --as-of 2026-10-08
finance report networth --as-of 2026-10-08
finance report payables --as-of 2026-10-07          # open payables / accrued / receivables / prepaids, aged
finance report accrual-vs-cash --from 2025-10-01 --to 2026-09-30
finance report project --project <code> [--as-of D] [--budget "$FINANCE_DATA/config/budgets.yaml"]
finance report spending --from 2026-07-01 --to 2026-09-30
finance report uncategorized [--by-counterparty] [--limit N] --from … --to …
finance report coverage --from … --to …
finance report reconcile | openings | unmatched
# every report takes --format text|md|csv; aliases: trial-balance, balances

# classified (company-style) statements, accrual basis, one column per period:
finance report balance-sheet    --from 2025-10-01 --to 2026-10-07 --period month [--dates D1,D2]
finance report income-statement --from 2025-10-01 --to 2026-10-07 --period month --extra ytd,ttm
finance report cash-flow        --from 2025-10-01 --to 2026-10-07 --period month --extra ytd,ttm
finance report statements       --from … --to … --format json    # all three, for scripts/statements.py
# --period month|quarter|year|none; --extra ytd,ttm,total; --format table|text|md|csv|json;
# --labels "$FINANCE_DATA/config/labels.yaml" (account/institution prefix -> plain label).
# Exit 1 if a check row (balance sheet balances, cash flow ties to balance-sheet cash) is not 0.
scripts/statements.py statements.json OUTDIR [--notes notes.json]   # phone PDFs (light, dark) + HTML, xlsx (+ glossary)
```

Statements: the balance sheet is classified (current assets, investments, property, current and
long-term liabilities, equity = opening equity + accumulated net income), one column per period
end. The income statement groups income, living expenses, non-operating items and taxes, with a
savings rate. The cash flow uses the indirect method: net income, adjusted for non-cash items
(shares received as pay, depreciation) and investment gains, plus working-capital changes, then
investing and financing. Entries with no cash line (e.g. land bought with a loan) are left out and
shown as memo rows. `scripts/statements.py`
(python3 + openpyxl, headless chrome for the PDF) turns the JSON into a portrait phone-width
PDF/HTML summary and an xlsx workbook with the monthly detail. The HTML follows the viewer's
light/dark setting (`prefers-color-scheme`); the PDF comes as `statements.pdf` (light) and
`statements-dark.pdf`. `scripts/refresh.sh` runs both. Labels use standard accounting terms
(current / non-current assets, accounts payable, accrued liabilities, cash flows from operating /
investing / financing activities, net worth (equity), ...). In the HTML, less familiar terms are
dotted-underlined buttons that open a one- or two-sentence definition on tap (native `popover`,
small inline script fallback; tap again or outside to close); the PDFs underline them and add a
Glossary section, the xlsx a Glossary sheet. Local `statement-notes.json` can add takeaways and
footnotes (with `[[term|text]]` markup) and override definitions (`"glossary": {term: text}`).

`categorize` and `transfers match` rebuild the whole journal from staging. The raw folder is
not needed after import. Each command prints the part of the result it is named for.
`finance --help` lists every command and report.

### Ad-hoc SQL: `finance sql`

```bash
finance sql "SELECT month, SUM(amount_minor) FROM v_ledger WHERE account_type='expense' GROUP BY 1"
echo "SELECT COUNT(*) FROM raw_transactions" | finance sql --format csv
finance sql -f my-query.sql --format json
finance sql --tables                       # tables + views (all attached schemas), row counts
finance sql --schema v_ledger              # CREATE statement + columns (also schema.name)
finance sql --queries                      # saved queries and their params
finance sql --q recent [--param days=7] [--param account=Expenses:Food]
finance sql --q month-spend --param month=2026-09 [--param basis=cash]
finance sql --q payables
finance sql --attach health=/workspace/health-data/health.db \
  "SELECT … FROM v_ledger l JOIN health.<table> h ON h.date = l.date"
FINANCE_ATTACH="health=/workspace/health-data/health.db" finance sql --tables
```

- **Read-only by default**: the file is opened with `mode=ro` and `PRAGMA query_only`, attached
  databases are opened read-only too, and `ATTACH`/`DETACH`/`VACUUM`/`PRAGMA query_only` are
  rejected in SQL text. Any write fails with `write rejected` (exit 1). `--write` opens the
  database read-write for deliberate maintenance only.
- Formats: `--format table` (default; aligned, numbers right-aligned, cells truncated at
  `--max-width 60`, at most `--max-rows 1000` rows shown), `csv` (`--no-header`), `json` (array
  of objects in column order, NULL → null), `md`.
- Several statements separated by `;` run in order; each result set is printed.
- Params: `:name` / `@name` / `$name` placeholders bind `--param name=value`. Unset placeholders
  bind NULL, so saved queries default with `COALESCE(:name, …)`. A `--param` that no statement
  uses is an error (typo guard).
- Saved queries are `queries/<name>.sql` in this repo (generic, no personal data), then
  `$FINANCE_DATA/queries/<name>.sql` for local ones. The leading `--` comment is the
  description and `-- param:` lines document parameters. Shipped: `recent`, `month-spend`,
  `payables`, `uncategorized`, `reconcile`, `staging`.

## Link your accounts: the Finance connector in Grok Bot

Plain-English steps. Steps marked *(check the app)* could not be confirmed from this repo or the
connector tools, so look for the matching option in the live Grok Bot app: menu names and
screens may differ from what is written here.

**What it does.** The Finance connector lets Grok Bot *read* your bank, credit card, loan and
investment accounts (balances, transactions, holdings). The connector tools are read-only: they
list connections, accounts, transactions and holdings; they do not move money. Linking goes
through a Plaid-backed sign-in window, so Grok Bot never sees your bank password *(check the
app for how the sign-in screen looks)*.

1. **Open Grok Bot and sign in** with your own account *(check the app: account or plan
   requirements)*.
2. **Find the connectors / integrations area** (probably in Settings) and look for **Finance**
   *(check the app: exact menu name and location; the connector id is "Finance" and its tools are
   named `finance_*`)*.
3. **Turn on / connect Finance** and approve the access request *(check the app)*.
4. **Add an institution.** Choose your bank, card issuer, brokerage or lender and sign in in the
   secure window; complete any text-message or app code *(check the app: the add-account flow is not
   described in this repo; once linked, each connection shows an institution name, a provider and
   a health status)*.
5. **Pick the accounts** to share (checking, savings, cards, mortgage, investments), then finish.
   Repeat for each institution *(check the app)*.
6. **Check it worked.** Ask Grok Bot: "List my linked financial institutions and their status."
   It uses `finance_list_connections`; each should show a healthy status. Then ask "List my
   accounts and balances" (`finance_list_accounts`).
7. **Try a few questions:** "What did I spend last month?", "What's pending on my cards?",
   "Show my investment holdings."
8. **If a link shows an unhealthy status** it needs re-linking (the tool description says so):
   repeat step 4 for that institution. Until then its balance shows the last known value *(check
   the app: where the re-link button lives)*.

Good to know:

- Some accounts or institutions may not be supported (for example some crypto wallets and
  exchanges, private / off-platform holdings, closed accounts); those are handled outside the
  connector here (see the Kraken, Coinbase and wallet sections). *(Check the app for the
  supported-institution list.)*
- Data can lag: pending items are visible but balances may not include them yet.
- Amount signs: plus = money in, minus = money out.
- To stop sharing, remove the institution in the same connectors area *(check the app)*.
- Never share bank passwords or one-time codes in chat; only enter them in the institution's own
  sign-in window.

Once linked, continue with [Setup](#setup) and [Nightly refresh](#nightly-refresh) to save the
connector output to a local `finance-data` folder and build the books. That developer workflow is
not needed just to link banks.

## Extract procedure (MANIFEST approach)

Extraction is done by an agent that calls the Finance connector tools
(`finance_list_connections`, `finance_list_accounts`, `finance_query_account_transactions`,
`finance_query_liability_transactions`, `finance_query_portfolio_transactions`,
`finance_list_portfolio_holdings`) with `limit=200` and cursor paging.

- Every page is saved unmodified as `cash_txns_pageNNN.csv/.json`,
  `liability_txns_pageNNN.*`, `portfolio_txns_pageNNN.*`, `holdings*.csv/.json`, `accounts.json`
  and `connections.json` in `raw/finance/<YYYY-MM-DD>/`. The folder date is the balance as-of
  date. A page may be just the verbatim tool result (`*_pageNNN.json` with `csv`, `row_count`,
  `next_cursor`): the importer reads the `csv` field when there is no `.csv` twin and fails if
  `row_count` disagrees with the rows (a truncated or mis-saved page).
- `MANIFEST.md` records each call (tool, filters, cursor, re-fetches) and the unique and pending
  counts. `SHA256SUMS` covers the page files.
- Pages may overlap. For example, a small final page can come back inline instead of as a file,
  so it is re-fetched with `date_to`. The importer dedups by connector transaction id; a posted
  row wins over a pending row with the same id.
- Never edit raw files. Fix problems in config/rules and re-run `finance categorize`.

### How imports accumulate

- **Staging is cumulative.** `raw_transactions` is keyed by connector transaction id and rows
  are never deleted. An extract that covers only the last few days adds new ids, updates
  changed ones (pending → posted under the same id, corrected text) and leaves every older row
  in place, so the journal keeps the full history. Re-importing the same folder is a no-op.
- **Balances: newest snapshot per account.** `balance_snapshots` keeps one row per account per
  extract date. The build uses each account's newest snapshot, so an account missing from
  tonight's `accounts.json` (closed, or its connection failed) keeps its last balance instead
  of dropping out. The opening entry is that balance minus the staged activity dated on or
  before the snapshot, and the connector balance assertions (`finance check`, `report
  reconcile`, `--q reconcile`) are rebuilt from the same newest snapshots. Re-running an
  extract for the same date replaces that date's snapshot.
- **Holdings** come from the newest extract that pulled holdings, so a night without a holdings
  file does not zero the positions (but always pull them; see below).
- **What reconciliation cannot see.** Because the opening absorbs any difference, a connector
  account always ties out against its newest balance. A gap or duplicate in history therefore
  shows up as the account's **opening balance moving** between imports instead. The import
  prints `openings: … changed` (REVIEW) when that happens. A consistent nightly never moves an
  opening, posting lag included. Causes: a missed page, a row the institution removed or
  re-issued under a new id, or a balance that includes pending activity.
- **Rows that vanished.** The import counts staged rows dated inside the extract's span (its
  earliest row date through the as-of date) that are not in the extract: `unseen: N posted, M
  pending`. Pending ones are normally superseded by a posted row with a new id. Pending rows are
  never booked, so they only add to the "pending (not booked)" count in `finance check`. Posted
  ones need review: the institution removed or re-issued them, and they are still booked.

## Nightly refresh

Run once a day (any time after midnight America/Denver). The folder date is today's date in
America/Denver. Never filter transaction queries by account, and keep pending rows.

1. **Window.** `LAST=$(finance sql --format csv --no-header "SELECT MAX(as_of) FROM balance_snapshots")`.
   `date_from` = `LAST` − 10 days (the overlap covers posting lag and pending → posted). Use no
   date filter at all (full history) on the first run, when `LAST` is more than 60 days ago,
   and once a week (for example Sunday) to pick up late corrections. Do not set `date_to`.
2. **Folder.** `D=$FINANCE_DATA/raw/finance/$(TZ=America/Denver date +%F)`; `mkdir -p "$D"`.
3. **Calls** (CallDynamicTool, namespace `user-Finance-xai`). Save each result **verbatim** as
   JSON, the whole object, in `$D`:

   | Call | Arguments | File |
   |---|---|---|
   | `finance_list_connections` | `{}` | `connections.json` |
   | `finance_list_accounts` | `{}` (no class filter) | `accounts.json` |
   | `finance_query_account_transactions` | `{"date_from": F, "limit": 200}`, then `{"cursor": next_cursor, "limit": 200}` until `next_cursor` is empty | `cash_txns_page001.json`, `002`, … |
   | `finance_query_liability_transactions` | same | `liability_txns_pageNNN.json` |
   | `finance_query_portfolio_transactions` | same | `portfolio_txns_pageNNN.json` |
   | `finance_list_portfolio_holdings` | `{"limit": 200}` (no cursor). If `row_count` is 200, also fetch per `account_ids` into `holdings_<n>.json` | `holdings.json` |

   For a full pull, omit `date_from`. Check every page's `row_count` against its CSV rows. The
   importer enforces this for `.json` pages. If a connection's `status` is not healthy, still
   import, but note it: those accounts keep their last balance.
4. **MANIFEST.md** (required; the script refuses a folder without it). Record the extraction
   time (America/Denver), the namespace, the window (`date_from`, full or incremental, and why),
   one line per call (tool, arguments or "cursor from pageNNN", file, `row_count`, whether
   `next_cursor` was set), connection statuses, re-fetches, and the unique and pending counts.
   Then write `SHA256SUMS`: `(cd "$D" && sha256sum *_page*.* holdings*.* > SHA256SUMS)`.
5. **Refresh.** `scripts/refresh.sh "$D"`. This validates the folder (name, `connections.json`,
   `accounts.json`, `MANIFEST.md`, pages, and `SHA256SUMS` if present). Then it runs `finance db
   migrate`, `finance import connector --raw "$D"`, `fnox exec -- finance download kraken`,
   `finance import kraken --raw <newest pull>` (`KRAKEN=0` skips; a failed download is REVIEW),
   the self-custody wallets when `$FINANCE_DATA/config/wallets.yaml` exists (`finance download
   wallets --history`, `scripts/wallet_events.py`, `finance import wallets`; `WALLETS=0` skips;
   any wallet failure is REVIEW, never FAIL) and `finance check`. It prints a summary and
   writes it to `$FINANCE_DATA/exports/refresh_<date>.md`, with the full output in
   `refresh_<date>.log`.
   - Exit **1**: import failed or `finance check` failed. Stop and report.
   - Exit **2**: invalid folder or usage.
   - `Status: REVIEW (…)` with exit 0: the books are consistent, but an opening balance moved
     or posted rows vanished from the window. Report it; do not edit raw files.
6. Do not commit anything from `$FINANCE_DATA`.

## Where data lives

| What | Where | In git? |
|---|---|---|
| Code, schema migrations, CoA template, generic rules, example configs, docs | this repo | yes |
| Raw connector extracts | `$FINANCE_DATA/raw/finance/<date>/` | **no** |
| SQLite books | `$FINANCE_DATA/finance.db` | **no** |
| Connector account map (ids, masks) | `$FINANCE_DATA/config/accounts.yaml` | **no** |
| Person-specific rules (names, vendors) | `$FINANCE_DATA/config/rules.local.yaml` (extra, first), `rules.personal.yaml` / `transfers.personal.yaml` (replace the shipped examples) | **no** |
| Personal chart-of-accounts additions | `$FINANCE_DATA/config/coa.local.yaml` | **no** |
| Report names, labels, goal, narrative notes | `$FINANCE_DATA/config/report.local.yaml` | **no** |
| Accruals, prepaids, loans, evidence index | `$FINANCE_DATA/config/accruals.yaml` | **no** |
| Item-level splits (itemized charges, item names) | `$FINANCE_DATA/config/splits.local.yaml` | **no** |
| Evidence documents (bills, returns, contracts) | `$FINANCE_DATA/documents/<kind>/` | **no** |
| Report exports with real numbers, `refresh_<date>.md/.log` | `$FINANCE_DATA/exports/` | **no** |
| Personal saved queries | `$FINANCE_DATA/queries/` | **no** |

The data dir sits outside the repo, so nothing in it can be committed by accident. `.gitignore`
also covers `config/wallets.yaml`, `imports/**/*.json`, `sessions/*`, `documents/*`, `.env*`
and binaries. Before every commit, run `git diff --cached` and confirm it has no
transactions, balances, account numbers or raw exports.

## Analysis with DuckDB / pandas

The database is plain SQLite. Amounts are INTEGER cents (`amount_minor`, debit-positive). Views
also expose a REAL `amount` for convenience.

```python
import sqlite3, pandas as pd
con = sqlite3.connect("file:/workspace/finance-data/finance.db?mode=ro", uri=True)
df = pd.read_sql("SELECT * FROM v_ledger", con)          # one row per journal line
```

```sql
-- duckdb
INSTALL sqlite; LOAD sqlite;
ATTACH '/workspace/finance-data/finance.db' AS f (TYPE sqlite, READ_ONLY);
SELECT month, level2, SUM(amount_minor)/100.0 FROM f.v_ledger WHERE account_type='expense' GROUP BY ALL;
```

## Kaspa download

`finance download kaspa` fetches balances, UTXOs and transactions from
[api.kaspa.org](https://api.kaspa.org/) for every wallet in `config/wallets.yaml` (gitignored)
that has an explicit `address`. It writes `imports/YYYY-MM-DD/kaspa/<Wallet>.json`. There are no
bank downloaders: bank, card, loan and brokerage data comes from the Finance connector instead.

## Kraken

```bash
fnox exec -- finance download kraken          # -> $FINANCE_DATA/raw/kraken/<today, Denver>/
finance import kraken --raw "$FINANCE_DATA/raw/kraken/$(TZ=America/Denver date +%F)"
finance check                                  # includes "exchange reconciliation"
```

- Credentials: `KRAKEN_API_KEY` / `KRAKEN_API_SECRET` from the environment, stored in fnox
  (`fnox exec --`). Never put them in files or arguments. The key needs only **Query Funds**,
  **Query Ledger Entries** and **Query Closed Orders & Trades**; a missing permission is
  reported by name.
- Only read-only methods are callable (client allowlist): private `Balance`, `BalanceEx`,
  `Ledgers`, `TradesHistory`; public `Assets`, `AssetPairs`, `Ticker`, `OHLC`.
- Files are saved byte-for-byte (dir 0700, files 0600) with `MANIFEST.md` and `SHA256SUMS`;
  re-running the same day rewrites nothing that did not change. Ledgers and TradesHistory are
  paged with `ofs` (50 per page); after the first pull only the newest staged ledger time minus
  14 days is fetched (`--full` for everything).
- How the history is booked (lots, transfers, staking, opening, mark to market) is in
  [docs/schema.md §3c](docs/schema.md). `scripts/refresh.sh` runs both steps nightly.

## Coinbase

Coinbase is read through the **Coinbase MCP connector** (an agent tool), not from Go, so a pull
is a folder of saved connector responses that `finance import coinbase` stages:

```bash
D="$FINANCE_DATA/raw/coinbase/$(TZ=America/Denver date +%F)"   # dir 0700, files 0600, never in git
# save (byte-for-byte) with read-only connector tools only:
#   coinbase_portfolios_list, coinbase_portfolios_get -> portfolio_<id>.json,
#   coinbase_balance -> balance_<id>.json, coinbase_orders_fills -> fills_*.json,
#   coinbase_products_best_bid_ask -> best_bid_ask.json, coinbase_products_candles (1d) -> candles_<BASE>-USD.json
# add events.csv (below), MANIFEST.md (what was called, when) and SHA256SUMS:
(cd "$D" && sha256sum $(ls | grep -v -e '^MANIFEST.md$' -e '^SHA256SUMS$') > SHA256SUMS)
finance import coinbase --raw "$D"
finance check                                  # "exchange reconciliation" covers coinbase too
```

- Never call the connector's trading or money-moving tools (orders create/edit/cancel,
  convert, transfer, portfolio edits, x402). The importer only reads files.
- The connector has **no deposit / withdrawal / send history and no consumer ("simple buy")
  transactions**, only Advanced Trade fills. Consumer activity goes in `events.csv`
  (`id,time,refid,type,asset,amount,fee,evidence,memo`; time RFC 3339; type
  `deposit|withdrawal|trade|staking`; rows of one trade share a `refid`; `fee` is paid on top of
  `amount`). Every row needs evidence: a Coinbase email message id, a bank row, or `inferred`
  with the reason in `memo`. Each pull carries the full file; rows dropped from it are unstaged.
- A USD-funded simple buy is a `deposit` (matched to the bank row the `coinbase` transfer hint
  booked) plus a `trade`. Crypto `withdrawal`s are sends to own wallets and move to
  `Assets:Crypto:SelfCustody` at FIFO cost. USDC is priced at par (Coinbase converts USD↔USDC
  1:1 without a fee). Balances come from `coinbase_balance`, overridden by the portfolio's spot
  positions (which include staked amounts). Booking is shared with Kraken
  ([docs/schema.md §3c](docs/schema.md)).

## Self-custody wallets

```bash
cp config/wallets.data.example.yaml "$FINANCE_DATA/config/wallets.yaml"; chmod 600 "$FINANCE_DATA/config/wallets.yaml"
# fill in public addresses / the account-level kpub (watch-only), labels and pass-through windows
finance download wallets --history        # -> $FINANCE_DATA/raw/wallets/<today, Denver>/ (0700/0600)
P="$FINANCE_DATA/raw/wallets/$(TZ=America/Denver date +%F)"
python3 scripts/wallet_events.py "$P"     # -> $P/events.csv (inferred swaps/transfers; estimates)
finance import wallets --raw "$P"         # books + realized gains by year (estimates) + $P/events-booked.csv
python3 scripts/wallet_events.py "$P" --timeline-md "$FINANCE_DATA/notes/wallet-swaps.md" --timeline-csv "$FINANCE_DATA/notes/wallet-swaps.csv"
finance check                             # "wallet reconciliation" compares on-chain quantities
```

- Public, read-only endpoints only (api.kaspa.org, Esplora, Solana public RPC, Kraken public
  ticker, CoinGecko). Monero has no public balance: save the wallet app's CSV export in the pull
  folder (it is carried forward to later pulls).
- Addresses and extended public keys live only in the local config and the local DB
  (`wallet_identifiers`); raw pulls are local; nothing with an address goes to git, statements
  or logs. Statements show wallet names (`Assets:Crypto:SelfCustody:<Wallet>[:<COIN>]`).
- Swaps and transfers are inferred from timing and amounts; review the timeline and the
  UNMATCHED list. Booking rules: [docs/schema.md §3d](docs/schema.md).

## GitHub

Remote git operations use **fnox**, so tokens never reach the shell history:
`eval "$(mise activate bash)"; fnox exec -- git push origin main`.

## Layout

```text
cmd/finance/            CLI (db, import, categorize, transfers, check, report, sql, download)
internal/connector/     raw extract reader (dedup, pending handling)
internal/books/         config loading + deterministic build (transfers, rules, openings, holdings, exchange lots)
internal/download/kraken/ read-only Kraken API client (signing, allowlist), raw download, parsers
internal/download/coinbase/ parsers for saved Coinbase connector responses + events.csv
internal/download/wallets/ public on-chain balances/history (Kaspa kpub derivation, Esplora, Solana, Monero CSV)
internal/ledger/        SQLite open/migrate, embedded migrations, staging, idempotent sync
internal/report/        statements and checks
internal/sqlq/          `finance sql`: read-only sessions, attach, renderers, saved queries
queries/                generic saved queries for `finance sql --q <name>`
scripts/refresh.sh      nightly migrate + import + check + summary
scripts/wallet_events.py wallet swap/transfer inference (events.csv) and the local timeline
internal/money/         exact decimal <-> integer minor units
internal/testfixture/   synthetic extract + config used by tests (fake data only)
config/coa.yaml         chart-of-accounts template (attributes, hierarchy)
config/*.example.yaml   templates for the local account map / rules
rules/categories.example.yaml  generic starter categorization rules + Plaid fallback (used when no rules.personal.yaml)
rules/transfers.example.yaml   starter transfer-matching hints (rule documented in the file)
docs/schema.md          research pass + schema design
```
