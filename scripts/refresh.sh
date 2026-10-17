#!/usr/bin/env bash
# Nightly refresh: stage a raw Finance-connector extract into the books and check them.
#
#   scripts/refresh.sh <raw extract dir>        e.g. "$FINANCE_DATA/raw/finance/2026-10-08"
#
# Steps: validate the extract (folder named YYYY-MM-DD, connections.json, accounts.json,
# MANIFEST.md, SHA256SUMS if present) -> finance db migrate -> finance import connector --raw
# <dir> -> finance check. Prints a short summary (net worth, check status, uncategorized count,
# rows/entries added) and writes it to $FINANCE_DATA/exports/refresh_<date>.md; the full
# command output goes to refresh_<date>.log next to it. After a passing check it also writes
# the financial statements to $FINANCE_DATA/exports/statements-<date>/ (non-fatal).
# Kraken: after the connector import it runs `fnox exec -- finance download kraken` (read-only
# API; credentials come from fnox, never from this script) and `finance import kraken --raw` on
# the newest pull, so `finance check` also reconciles Kraken against the API. A failed download
# is REVIEW (the previous pull stays in the books); a failed Kraken import is FAIL.
# Wallets: when $FINANCE_DATA/config/wallets.yaml exists, `finance download wallets --history`
# (public block explorers, no credentials), scripts/wallet_events.py (swap/transfer inference),
# `finance import wallets --raw` on the newest complete pull and the local timeline
# ($FINANCE_DATA/notes/wallet-swaps.md/.csv). Any wallet failure is REVIEW, never FAIL (the
# previous wallet pull stays in the books). WALLETS=0 skips it.
#
# Exit: 0 ok, 1 import or check failed (summary still written), 2 usage / invalid extract.
# The summary's first line is "Status: OK | REVIEW (...) | FAIL"; REVIEW (exit 0) means the
# books are consistent but something needs a human: an opening pin changed, a reported balance
# does not follow from the pinned opening + history (gap/duplicate; booked to
# Liabilities:Pending:Unposted) or posted rows vanished from the extract window. A gap on an
# account with a pending_account (Venmo funding lag) is expected and only listed. Uncategorized lines are only counted.
# Env:  FINANCE_DATA (default: ../finance-data next to the repo), FINANCE_BIN (default:
#       <repo>/bin/finance, rebuilt if missing or older than the Go sources), FINANCE_DB (default $FINANCE_DATA/finance.db),
#       FINANCE_REPO (config/rules root, default: this repo), KRAKEN=0 skips Kraken, WALLETS=0 skips wallets,
#       FNOX_AGE_KEY_FILE (default ~/.config/fnox/age.txt).
# No personal data lives in this script.
set -euo pipefail

usage() { sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
[[ $# -eq 1 && $1 != -h && $1 != --help ]] || usage

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
export FINANCE_DATA=${FINANCE_DATA:-$(cd "$REPO/.." && pwd)/finance-data}
export FINANCE_DB=${FINANCE_DB:-$FINANCE_DATA/finance.db}
BIN=${FINANCE_BIN:-$REPO/bin/finance}
CFG_REPO=${FINANCE_REPO:-$REPO}   # where config/coa.yaml, rules/*.example.yaml and queries/ live

RAW=$(cd "$1" 2>/dev/null && pwd) || { echo "refresh: no such directory: $1" >&2; exit 2; }
DATE=$(basename "$RAW")
[[ $DATE =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || { echo "refresh: extract folder must be named YYYY-MM-DD (the balance as-of date): $RAW" >&2; exit 2; }
for f in connections.json accounts.json MANIFEST.md; do
  [[ -s $RAW/$f ]] || { echo "refresh: $RAW/$f is missing (see README: Nightly refresh)" >&2; exit 2; }
done
if [[ -f $RAW/SHA256SUMS ]]; then
  (cd "$RAW" && sha256sum --quiet -c SHA256SUMS) >&2 || { echo "refresh: SHA256SUMS does not match the page files" >&2; exit 2; }
fi
shopt -s nullglob
pages=("$RAW"/cash_txns_page*.* "$RAW"/liability_txns_page*.*)
shopt -u nullglob
(( ${#pages[@]} > 0 )) || { echo "refresh: no cash_/liability_txns_pageNNN.csv|json pages in $RAW" >&2; exit 2; }

# Build the default binary when it is missing or older than the Go sources: config fields that ship
# together with code (e.g. the loan accrual's escrow fields in accruals.yaml, decoded strictly) must
# never meet a stale binary. A failed rebuild keeps the old binary (the import then fails loudly).
if [[ $BIN == "$REPO/bin/finance" ]]; then
  stale=
  if [[ ! -x $BIN ]]; then
    stale=missing
  elif [[ $REPO/go.mod -nt $BIN || $REPO/go.sum -nt $BIN ]] ||
       [[ -n $(find "$REPO/cmd" "$REPO/internal" -name '*.go' ! -name '*_test.go' -newer "$BIN" -print -quit 2>/dev/null) ]]; then
    stale=outdated
  fi
  if [[ -n $stale ]]; then
    echo "refresh: building $BIN ($stale)" >&2
    (cd "$REPO" && go build -o "$BIN" ./cmd/finance) || {
      [[ -x $BIN ]] || { echo "refresh: build failed and no binary" >&2; exit 2; }
      echo "refresh: WARNING rebuild failed; using the existing $BIN" >&2
    }
  fi
elif [[ ! -x $BIN ]]; then
  echo "refresh: FINANCE_BIN=$BIN is not executable" >&2; exit 2
fi

mkdir -p "$FINANCE_DATA/exports"
OUT=$FINANCE_DATA/exports/refresh_$DATE.md
LOG=$FINANCE_DATA/exports/refresh_$DATE.log
: >"$LOG"

# one refresh at a time
exec 9>"$FINANCE_DATA/.refresh.lock"
if command -v flock >/dev/null; then
  flock -n 9 || { echo "refresh: another refresh is running ($FINANCE_DATA/.refresh.lock)" >&2; exit 2; }
fi

fin() { "$BIN" "$@" --repo "$CFG_REPO" --db "$FINANCE_DB"; }
# single value from a read-only query (empty if the db/table does not exist yet)
q1() { [[ -f $FINANCE_DB ]] && fin sql --format csv --no-header "$@" 2>>"$LOG" | head -1 || true; }

entries_before=$(q1 "SELECT COUNT(*) FROM journal_entries"); entries_before=${entries_before:-0}
raw_before=$(q1 "SELECT COUNT(*) FROM raw_transactions"); raw_before=${raw_before:-0}

status=0
import_ok=yes
{ echo "## finance db migrate"; fin db migrate; } >>"$LOG" 2>&1 || import_ok=no
if [[ $import_ok == yes ]]; then
  { echo; echo "## finance import connector --raw $RAW"; } >>"$LOG"
  if ! import_out=$(fin import connector --raw "$RAW" 2>&1); then import_ok=no; fi
  printf '%s\n' "$import_out" >>"$LOG"
fi

# Kraken: download (via fnox for the API credentials) then import the newest pull.
kraken_status="skipped (KRAKEN=0)"
kraken_review=no
if [[ $import_ok == yes && ${KRAKEN:-1} != 0 ]]; then
  FNOX=$(command -v fnox || echo "$HOME/.local/share/mise/shims/fnox")
  [[ -z ${FNOX_AGE_KEY_FILE:-} && -f $HOME/.config/fnox/age.txt ]] && export FNOX_AGE_KEY_FILE=$HOME/.config/fnox/age.txt
  { echo; echo "## fnox exec -- finance download kraken"; } >>"$LOG"
  if [[ -x $FNOX ]] && (cd "$REPO" && "$FNOX" exec -- "$BIN" download kraken --data "$FINANCE_DATA") >>"$LOG" 2>&1; then
    kraken_status="downloaded"
  else
    kraken_status="download FAILED (see log; previous pull kept)"
    kraken_review=yes
  fi
  shopt -s nullglob
  kpulls=("$FINANCE_DATA"/raw/kraken/[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9])
  shopt -u nullglob
  if (( ${#kpulls[@]} > 0 )); then
    KRAW=${kpulls[-1]}
    { echo; echo "## finance import kraken --raw $KRAW"; } >>"$LOG"
    if kimport_out=$(fin import kraken --raw "$KRAW" 2>&1); then
      kraken_status+="; imported $(basename "$KRAW")"
    else
      kraken_status+="; import FAILED"
      import_ok=no
    fi
    printf '%s\n' "$kimport_out" >>"$LOG"
  else
    kraken_status+="; no pull to import"
  fi
fi

# Self-custody wallets: public data only; failures are REVIEW (never fail the refresh).
wallets_status="skipped (no config/wallets.yaml or WALLETS=0)"
wallets_review=no
WCFG=$FINANCE_DATA/config/wallets.yaml
if [[ $import_ok == yes && ${WALLETS:-1} != 0 && -f $WCFG ]]; then
  { echo; echo "## finance download wallets --history"; } >>"$LOG"
  if "$BIN" download wallets --data "$FINANCE_DATA" --config "$WCFG" --history >>"$LOG" 2>&1; then
    wallets_status="downloaded"
  else
    wallets_status="download FAILED (see log; previous pull kept)"
    wallets_review=yes
  fi
  WRAW=
  shopt -s nullglob
  for d in "$FINANCE_DATA"/raw/wallets/[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]; do
    [[ -f $d/MANIFEST.md && -f $d/balances.json ]] && WRAW=$d
  done
  shopt -u nullglob
  if [[ -n $WRAW ]]; then
    { echo; echo "## scripts/wallet_events.py $(basename "$WRAW")"; } >>"$LOG"
    if ! (command -v python3 >/dev/null && python3 "$REPO/scripts/wallet_events.py" "$WRAW" --db "$FINANCE_DB") >>"$LOG" 2>&1; then
      wallets_status+="; event inference FAILED"
      wallets_review=yes
    fi
    { echo; echo "## finance import wallets --raw $WRAW"; } >>"$LOG"
    if wimport_out=$(fin import wallets --raw "$WRAW" 2>&1); then
      wallets_status+="; imported $(basename "$WRAW")"
      mkdir -p "$FINANCE_DATA/notes"
      python3 "$REPO/scripts/wallet_events.py" "$WRAW" --timeline-md "$FINANCE_DATA/notes/wallet-swaps.md" \
        --timeline-csv "$FINANCE_DATA/notes/wallet-swaps.csv" >>"$LOG" 2>&1 || wallets_status+="; timeline FAILED"
    else
      wallets_status+="; import FAILED (books keep the previous wallet pull)"
      wallets_review=yes
    fi
    printf '%s\n' "$wimport_out" >>"$LOG"
  else
    wallets_status+="; no complete pull to import"
    wallets_review=yes
  fi
fi

check_status="not run"
if [[ $import_ok == yes ]]; then
  { echo; echo "## finance check"; } >>"$LOG"
  set +e
  fin check --format md >>"$LOG" 2>&1
  rc=$?
  set -e
  case $rc in
    0) check_status=PASS ;;
    1) check_status=FAIL; status=1 ;;
    *) check_status="ERROR (exit $rc)"; status=1 ;;
  esac
else
  status=1
fi

entries_after=$(q1 "SELECT COUNT(*) FROM journal_entries")
raw_after=$(q1 "SELECT COUNT(*) FROM raw_transactions")
networth=$(q1 "SELECT printf('%.2f', COALESCE(SUM(l.amount),0)/100.0) FROM journal_lines l
  JOIN journal_entries e ON e.id = l.entry_id JOIN accounts a ON a.id = l.account_id
  WHERE e.status = 'posted' AND a.account_type IN ('asset','liability') AND e.entry_date <= :d" --param "d=$DATE")
uncat=$(q1 "SELECT COUNT(*) FROM v_uncategorized")
recon=$(q1 "SELECT COUNT(*) FROM v_balance_assertion_check c WHERE c.source = 'connector' AND c.expected_minor <> c.actual_minor
  AND c.as_of = (SELECT MAX(b.as_of) FROM v_balance_assertion_check b WHERE b.account = c.account AND b.source = 'connector')")
krecon=$(q1 "SELECT COUNT(*) FROM v_balance_assertion_check c WHERE c.source = 'kraken' AND c.expected_minor <> c.actual_minor
  AND c.as_of = (SELECT MAX(b.as_of) FROM v_balance_assertion_check b WHERE b.account = c.account AND b.source = 'kraken')")
kvalue=$(q1 "SELECT printf('%.2f as of %s', amount/100.0, as_of) FROM balance_assertions WHERE source = 'kraken' ORDER BY as_of DESC LIMIT 1")
wvalue=$(q1 "SELECT printf('%.2f as of %s', SUM(amount)/100.0, as_of) FROM balance_assertions WHERE source = 'wallets'
  AND as_of = (SELECT MAX(as_of) FROM balance_assertions WHERE source = 'wallets')")
stale=$(q1 "SELECT COUNT(*) FROM balance_assertions WHERE source = 'connector' AND as_of < :d" --param "d=$DATE")

line() { printf '%s\n' "${import_out:-}" | grep -E "^  $1:" | sed -E "s/^  $1: +//" || true; }
staged=$(line staged)
journal=$(line journal)
unseen=$(line unseen)
openings=$(line openings)
pendingl=$(line pending)
unrecon=$(line unreconciled)
review=()
[[ -n $openings ]] && review+=("opening balances moved")
[[ -n $unrecon ]] && review+=("balance does not follow from pinned opening + history")
[[ $unseen =~ ^[1-9] ]] && review+=("posted rows missing from the extract window")
[[ $kraken_review == yes ]] && review+=("kraken download failed")
[[ $wallets_review == yes ]] && review+=("wallets: $wallets_status")
overall=OK
(( ${#review[@]} > 0 )) && overall="REVIEW ($(IFS=,; echo "${review[*]}" | sed 's/,/, /g'))"
(( status != 0 )) && overall=FAIL
inserted=$(sed -nE 's/^([0-9]+) inserted.*/\1/p' <<<"$journal")
diffcount() { [[ -n $1 && -n $2 ]] && echo $(( $2 - $1 )) || echo "?"; }

# Financial statements (Balance Sheet, Profit and Loss Statement, Statement of Cash Flows): trailing 12 months by
# month plus YTD/TTM, as JSON + CSV, then the phone HTML (follows the phone's light/dark
# setting), statements.pdf (light) + statements-dark.pdf and the xlsx when python3 (with
# openpyxl) and chrome are available. Only after a passing check; failures here never fail
# the refresh (noted in the summary instead).
statements="skipped (check did not pass)"
if [[ $check_status == PASS ]]; then
  SDIR=$FINANCE_DATA/exports/statements-$DATE
  SFROM=$(date -d "$DATE -1 year" +%Y-%m-01)
  sargs=(--from "$SFROM" --to "$DATE" --period month --extra ytd,ttm)
  mkdir -p "$SDIR"
  { echo; echo "## finance report statements ${sargs[*]}"; } >>"$LOG"
  if fin report statements "${sargs[@]}" --format json >"$SDIR/statements.json" 2>>"$LOG"; then
    statements="\`$SDIR\` (json, csv"
    for k in balance-sheet income-statement cash-flow; do
      fin report "$k" "${sargs[@]}" --format csv >"$SDIR/$k.csv" 2>>"$LOG" || statements+=", $k.csv FAILED"
    done
    notes=()
    [[ -f $FINANCE_DATA/config/statement-notes.json ]] && notes=(--notes "$FINANCE_DATA/config/statement-notes.json")
    if command -v python3 >/dev/null && python3 "$REPO/scripts/statements.py" "$SDIR/statements.json" "$SDIR" "${notes[@]}" >>"$LOG" 2>&1; then
      statements+=", pdf + dark pdf, html, xlsx)"
    else
      statements+="; pdf/dark pdf/html/xlsx not (all) rendered, see log)"
    fi
  else
    statements="FAILED (a statement check row is not zero or the report errored; see log)"
  fi
fi

{
  echo "# Finance refresh $DATE"
  echo
  echo "**Status: $overall**"
  echo
  echo "- extract: \`$RAW\`"
  echo "- import: $([[ $import_ok == yes ]] && echo ok || echo FAILED)"
  echo "- check: **$check_status** (connector reconciliation differences: ${recon:-?}; accounts on an older snapshot: ${stale:-?})"
  echo "- kraken: $kraken_status; API value ${kvalue:-?}; reconciliation differences: ${krecon:-?}"
  echo "- wallets: $wallets_status; on-chain value ${wvalue:-?}"
  echo "- net worth as of $DATE: ${networth:-?}"
  echo "- uncategorized lines: ${uncat:-?}"
  echo "- raw rows added to staging: $(diffcount "$raw_before" "$raw_after") (total ${raw_after:-?})"
  echo "- journal entries added: ${inserted:-?} inserted; net change $(diffcount "$entries_before" "$entries_after") (total ${entries_after:-?})"
  [[ -n $staged ]] && echo "- staged: $staged"
  [[ -n $journal ]] && echo "- journal: $journal"
  [[ -n $unseen ]] && echo "- unseen: $unseen"
  echo "- opening-balance drift: ${openings:-none} (openings are pinned)"
  echo "- pending (reported balance includes activity not posted yet): ${pendingl:-none}"
  [[ -n $unrecon ]] && echo "- unreconciled: $unrecon"
  echo "- statements: $statements"
  echo "- full output: \`$LOG\`"
} >"$OUT"
cat "$OUT" || true

exit $status
