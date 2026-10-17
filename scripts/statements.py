#!/usr/bin/env python3
"""Render the classified statements (JSON from `finance report statements`) as:

  statements.xlsx   one sheet per statement, full monthly detail (numbers as numbers,
                    frozen headers, negatives in parentheses) + a Summary and a Glossary sheet
  statements.html   phone-friendly single file (summary, balance sheet, income statement,
                    cash flow; two number columns max; whole dollars); light or dark following
                    the viewer's system setting (prefers-color-scheme); glossary terms are
                    dotted-underlined buttons that open a short definition on tap (popover)
  statements.pdf    the same HTML printed to a portrait phone-width PDF (headless Chrome), light;
                    glossary terms underlined and defined in a Glossary section at the end
  statements-dark.pdf  the same layout in the dark palette (full-page dark background)

Labels use standard accounting terms (current / non-current assets, accounts payable, accrued
liabilities, cash flows from operating / investing / financing activities, ...); net worth is
shown as "Net worth (equity)".

Usage:
  finance report statements --from YYYY-MM-01 --to YYYY-MM-DD --period month --extra ytd,ttm \
      --format json > statements.json
  scripts/statements.py statements.json OUTDIR [--notes notes.json] [--chrome PATH]

notes.json (optional, local): {"takeaways": ["..."], "footnotes": ["..."],
"glossary": {"<term key>": "definition", ...}}: takeaways and footnotes are appended to the
automatic ones and may mark glossary terms as [[key|shown text]]; glossary entries replace the
generic definitions (e.g. with household-specific examples). No personal data lives in this
script; item labels come from the JSON.
Needs openpyxl for the workbook; the PDF needs google-chrome/chromium (skipped if missing).
"""
import argparse
import html
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from datetime import date

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import reportcfg as RC  # noqa: E402  (local labels: $FINANCE_DATA/config/report.local.yaml)

HL = RC.load()["highlights"]
BROKER = RC.load()["brokerage_name"].lower()

# ---------------------------------------------------------------- helpers

def load(path):
    d = json.load(open(path))
    st = {s["kind"]: s for s in d["statements"]}
    return d.get("as_of"), st["balance-sheet"], st["income-statement"], st["cash-flow"]


def col_index(stmt, key=None, label=None):
    for i, c in enumerate(stmt["columns"]):
        if (key and c["key"] == key) or (label and c["label"] == label):
            return i
    return None


def rows(stmt, kind=None, section=None, group=None, label=None):
    out = []
    for r in stmt["rows"]:
        if kind and r["kind"] != kind:
            continue
        if section and r.get("section") != section:
            continue
        if group and r.get("group") != group:
            continue
        if label and r["label"] != label:
            continue
        out.append(r)
    return out


def val(r, i):
    v = r.get("values") or []
    return float(v[i]) if i is not None and i < len(v) else 0.0


def group_total(stmt, groups, i, section=None):
    t = 0.0
    for r in stmt["rows"]:
        if r["kind"] == "group" and r.get("group") in groups and (section is None or r.get("section") == section):
            t += val(r, i)
    return t


def row_val(stmt, label, i):
    rs = rows(stmt, label=label)
    return val(rs[0], i) if rs else 0.0


def money(v, dollars=True):
    n = round(v)
    s = f"{abs(n):,}"
    if dollars:
        s = "$" + s
    return f"({s})" if n < 0 else s


def short(v):
    n = abs(v)
    s = f"${n/1000:,.0f}k" if n >= 10000 else f"${n:,.0f}"
    return f"({s})" if v < 0 else s


# ---------------------------------------------------------------- glossary and labels

# Label markup: "[[key|shown text]]" (or "[[key]]" for the glossary term itself) marks a glossary
# term. The HTML turns it into a tap-to-open definition (popover), the PDFs into a dotted
# underline plus a Glossary section at the end, the xlsx into plain text (+ a Glossary sheet).
# Definitions here are generic; notes.json {"glossary": {key: "definition"}} overrides them locally
# (e.g. household-specific examples), so no personal data lives in this script.
GLOSSARY = {
    "accrual": ("Accrual basis",
                "Income counts when it is earned and costs when they are incurred, not when cash moves. "
                "A bill we owe but haven't paid is already an expense; pay earned but not yet deposited is already income."),
    "ytd": ("YTD (year to date)", "From January 1 through the report date."),
    "ttm": ("TTM (trailing twelve months)",
            "The most recent 12 months. Unlike YTD it always covers a full year, so lumpy items like tax "
            "payments and distributions even out."),
    "networth": ("Net worth (equity)",
                 "Everything we own minus everything we owe (total assets minus total liabilities). Companies call "
                 "this owners' or shareholders' equity; it grows by net income."),
    "opening_equity": ("Opening equity",
                       "Net worth on the day these books start (the balances carried in), before any of the income "
                       "and spending they track."),
    "retained": ("Retained earnings",
                 "All net income since these books start: the part of net worth added by earning more than we spent."),
    "current_assets": ("Current assets",
                       "Cash and things that turn into cash or get used up within about a year: receivables, "
                       "prepaid expenses and transfers in transit."),
    "noncurrent_assets": ("Non-current assets",
                          "Assets held for longer than a year: investment and retirement accounts, real estate, "
                          "construction in progress and vehicles."),
    "ar": ("Accounts receivable",
           "Money others owe us that we have already earned or advanced but not yet received, such as pay earned "
           "but not yet deposited or costs someone has agreed to repay."),
    "prepaid": ("Prepaid expenses and other current assets",
                "Costs paid in advance that haven't been used up yet (trips, insurance, subscriptions); each becomes "
                "an expense over the months it covers. 'Other' is escrow and money moving between our accounts."),
    "ppe": ("Property, plant and equipment",
            "Long-lived physical assets: here the home, land, construction in progress and vehicles, carried at cost "
            "(vehicles at owner value)."),
    "at_cost": ("At cost",
                "Shown at what we paid (plus improvements), not today's market value, which could be higher or lower. "
                "Vehicles are at our own estimate of their value."),
    "mtm": ("Mark-to-market",
            "Valued at today's market price each period; the change shows up as an unrealized gain or loss in net "
            "income. Brokerage and retirement accounts are shown this way."),
    "cip": ("Construction in progress (CIP)",
            "Money spent on a building that isn't finished yet (design, engineering, permits, construction). It is "
            "carried at cost and becomes part of the home's cost once the house is done."),
    "current_liabilities": ("Current liabilities",
                            "What we owe that is due within about a year: card balances, unpaid bills and accrued "
                            "taxes and interest."),
    "ap": ("Accounts payable",
           "Bills received or contracted but not yet paid, such as medical bills, HOA dues and design or engineering "
           "balances."),
    "accrued": ("Accrued liabilities",
                "Costs already incurred but not yet paid or billed, such as income and property tax owed for the year "
                "and interest building up on a loan. Also money we hold that belongs to others."),
    "ltd": ("Long-term debt",
            "Loans repaid over more than a year: the mortgage and the related-party note. Shown in full; the part due "
            "within a year is not split out."),
    "related": ("Related-party note",
                "A loan from someone closely connected to us (here, family) rather than a bank or other lender."),
    "revenue": ("Revenue and gains",
                "Everything earned in the period: wages, S-corp distributions, investment gains, interest and other "
                "income. Total income is the base for the savings rate."),
    "scorp": ("S-corp distributions",
              "Cash an S corporation pays its owners out of profits. Unlike wages, no payroll tax is withheld; the "
              "company's profit is taxed on the owners' personal return. Counted as income when received."),
    "gains": ("Investment gains (realized and unrealized)",
              "Realized gains are profits on investments sold; unrealized gains are increases in the value of "
              "investments still held (mark-to-market)."),
    "opex": ("Operating expenses",
             "Everyday living costs: housing, food, healthcare, transportation and so on. Loan interest and taxes "
             "are shown separately below them."),
    "income_tax": ("Income tax expense",
                   "Federal and state income tax for the period on the accrual basis (withholding plus the tax "
                   "expected for the year, and any penalties), not just what was paid."),
    "fica": ("Payroll taxes (FICA)", "Social Security and Medicare taxes withheld from wages."),
    "savings_rate": ("Savings rate",
                     "Net income divided by total income: the share of everything that came in that we kept (it "
                     "added to net worth) instead of spending it or paying it in tax."),
    "indirect": ("Indirect method",
                 "The cash flow statement starts from net income and adjusts it to cash (removing non-cash items "
                 "and timing differences), then adds cash spent or raised by investing and financing."),
    "operating": ("Operating activities",
                  "Cash from everyday earning and spending: net income, adjusted for non-cash items (investment gains, pay "
                  "received in shares, depreciation), and for bills, card balances and taxes paid earlier or later than expensed."),
    "investing": ("Investing activities",
                  "Cash spent on or received from long-term assets: buying and selling investments, retirement "
                  "contributions, land, construction and vehicles."),
    "financing": ("Financing activities",
                  "Cash from borrowing and repaying debt: loan proceeds come in, mortgage and loan principal payments "
                  "go out (interest is under operating)."),
    "noncash_adj": ("Adjustments for non-cash items",
                    "Income that didn't arrive as cash (investment gains left in the account, pay received as shares) "
                    "is subtracted and expenses that cost no cash (depreciation) are added back, so the total counts only cash."),
    "wc": ("Changes in working capital",
           "Timing differences between expenses and cash: an unpaid bill is an expense now but cash later, while "
           "paying last year's tax uses cash now for an expense counted earlier."),
    "noncash": ("Non-cash investing and financing activities",
                "Purchases that added assets or debt without cash moving through our bank accounts, such as land paid "
                "for directly by a lender or work billed but not yet paid. For reference only; not in the cash totals."),
    "gross_investing": ("Gross investing lines",
                        "Cash spent and cash received are listed separately (purchases vs proceeds), not netted into one "
                        "line. The section total is still the net cash from investing activities."),
    "land_payments": ("Land purchase payments",
                      "Cash paid toward land we already put on the books (for example settling an amount owed to a "
                      "co-owner). Financed purchases where the lender paid the seller directly appear under non-cash "
                      "investing and financing activities instead."),
    "inv_purchases": ("Purchases of investments",
                      "Cash sent into brokerage, retirement and crypto accounts to buy securities or crypto (or as "
                      "contributions). Sales are listed separately as proceeds."),
    "inv_proceeds": ("Proceeds from sales of investments",
                     "Cash received from selling investments (for example private-company share sales and brokerage "
                     "withdrawals of sale proceeds). Purchases are listed separately."),
}

TERM_RE = re.compile(r"\[\[([a-z_]+)(?:\|([^\]]*))?\]\]")
TERM_NW_RE = re.compile(r"(\(?)\[\[([a-z_]+)(?:\|([^\]]*))?\]\]([).,;:!?]*)")


def plain(s):
    """Label markup -> plain text (xlsx, takeaways sorting)."""
    return TERM_RE.sub(lambda m: m.group(2) if m.group(2) is not None else GLOSSARY[m.group(1)][0], s)


def glossary(notes, IS=None):
    """GLOSSARY with local overrides from notes.json and the YTD/TTM ranges filled in."""
    g = {k: [t, d] for k, (t, d) in GLOSSARY.items()}
    if IS is not None:
        iy, it = col_index(IS, key="ytd"), col_index(IS, key="ttm")
        if iy is not None and IS["columns"][iy].get("from"):
            c = IS["columns"][iy]
            g["ytd"][1] = f"From January 1 through the report date ({nice_date(c['from'])} to {nice_date(c['to'])})."
        if it is not None and IS["columns"][it].get("from"):
            c = IS["columns"][it]
            g["ttm"][1] = (f"The most recent 12 months ({nice_date(c['from'])} to {nice_date(c['to'])}). "
                           + GLOSSARY["ttm"][1].split(". ", 1)[1])
    for k, v in (notes.get("glossary") or {}).items():
        if isinstance(v, dict):
            g.setdefault(k, ["", ""])
            g[k][0] = v.get("term", g[k][0])
            g[k][1] = v.get("definition", g[k][1])
        elif k in g:
            g[k][1] = v
        else:
            print(f"statements: glossary override for unknown term {k!r} ignored", file=sys.stderr)
    return g


def nice_date(iso):
    try:
        d = date.fromisoformat(iso)
    except ValueError:
        return iso
    return f"{d:%b} {d.day}, {d.year}"


# ---------------------------------------------------------------- phone model

# (label markup, statement groups). Assets and liabilities are split current / non-current.
CURRENT_ASSETS = [
    ("Cash and cash equivalents", ["Cash"]),
    ("[[ar|Accounts receivable]]", ["Money owed to us"]),
    ("[[prepaid|Prepaid expenses and other current assets]]", ["Prepaid expenses", "Escrow", "Transfers in transit"]),
]
NONCURRENT_ASSETS = [
    ("Investments (brokerage, private shares, crypto)", ["Brokerage and private shares", "Crypto"]),
    ("Retirement accounts", ["Retirement accounts"]),
    ("Real estate: home ([[at_cost|at cost]])", ["Home (at cost)"]),
    ("Land (at cost)", ["Land"]),
    ("[[cip|Construction in progress (CIP)]]", ["Construction in progress"]),
    ("Vehicles (owner value)", ["Vehicles"]),
    ("Other non-current assets", ["Other assets"]),
]
CURRENT_LIABS = [
    ("Credit cards", ["Credit cards"]),
    ("[[ap|Accounts payable]]", ["Bills and payables"]),
    ("Pending funding (not posted yet)", ["Pending funding"]),
    ("[[accrued|Accrued liabilities]]", ["Accrued taxes", "Accrued interest and other",
                                        "Owed to others (pass-through)", "Other liabilities"]),
]
LONGTERM_DEBT = [
    ("Mortgage", ["Mortgages"]),
    ("[[related|Related-party note]]", ["Loans"]),
]
INCOME = [
    ("Wages and salaries", ["Wages and salaries"]),
    ("[[scorp|S-corp distributions]] and business income", ["Business income"]),
    ("[[gains|Investment gains]] (realized and unrealized)", ["Investment gains"]),
    ("Interest, dividends and other income", ["Interest and dividends", "Retirement distributions", "Other income"]),
]
OPEX = [
    ("Housing and utilities", ["Housing", "Utilities"]),
    ("Food", ["Food"]),
    ("Healthcare and childcare", ["Healthcare", "Baby and childcare"]),
    ("Charitable giving", ["Giving"]),
    ("Shopping", ["Shopping"]),
    ("Travel and entertainment", ["Entertainment and travel"]),
    ("Transportation", ["Transportation"]),
    ("Insurance", ["Insurance"]),
    ("Subscriptions, education and other", ["Subscriptions and software", "Education", "Personal care, gifts and other"]),
]
INTEREST = ("Interest expense on loans", ["Interest on loans"])  # mortgage interest is in Housing
# the Taxes group split by item (account key)
TAX_LINES = [("[[income_tax|Income tax expense]]", None),
             ("[[fica|Payroll taxes (FICA)]]", (":SocialSecurity", ":Medicare", ":FICA", ":Payroll")),
             ("Property tax", (":Property",))]

# cash flow investing / financing lines by statement group (gross investing keys from the report)
INV_NAMES = {
    "Land purchase payments": "[[land_payments|Land purchase payments]]",
    "Construction in progress": "[[cip|Construction in progress (CIP)]] (design and engineering)",
    "Home improvements": "Home improvements",
    "Purchases of investments": "[[inv_purchases|Purchases of investments]] (brokerage, retirement, crypto)",
    "Proceeds from sales of investments": "[[inv_proceeds|Proceeds from sales of investments]]",
    "Proceeds from land sales": "Proceeds from land sales",
    "Vehicles bought": "Vehicles bought",
    "Vehicles sold": "Vehicles sold",
    # legacy keys (older JSON) kept so a stale statements.json still renders
    "Brokerage and private shares": "Securities bought and sold, net",
    "Retirement accounts": "Retirement account contributions",
    "Crypto": "Crypto bought and sold, net",
    "Land": "[[land_payments|Land purchase payments]]",
    "Home (at cost)": "Home improvements",
    "Vehicles": "Vehicles bought and sold, net",
}
INV_ORDER = [
    "[[land_payments|Land purchase payments]]",
    "[[cip|Construction in progress (CIP)]] (design and engineering)",
    "Home improvements",
    "[[inv_purchases|Purchases of investments]] (brokerage, retirement, crypto)",
    "Vehicles bought",
    "[[inv_proceeds|Proceeds from sales of investments]]",
    "Proceeds from land sales",
    "Vehicles sold",
    "Securities bought and sold, net",
    "Retirement account contributions",
    "Crypto bought and sold, net",
    "Vehicles bought and sold, net",
]
FIN_NAMES = {"Mortgages": "Mortgage principal repaid", "Loans": "[[related|Related-party note]], net"}


def is_lines(IS, i):
    """(income lines, operating-expense lines, other expense lines: interest and taxes); expenses positive."""
    inc = [(n, group_total(IS, g, i)) for n, g in INCOME]
    opx = [(n, group_total(IS, g, i)) for n, g in OPEX]
    other = [(INTEREST[0], -group_total(IS, INTEREST[1], i))]  # shown negative inside non-operating
    taxes = [0.0] * len(TAX_LINES)
    for r in rows(IS, kind="item", group="Taxes"):
        k = 0
        for j, (_, pats) in enumerate(TAX_LINES):
            if pats and any(p.lower() in (r.get("key") or "").lower() for p in pats):
                k = j
        taxes[k] += val(r, i)
    tax_total = group_total(IS, ["Taxes"], i)
    if abs(sum(taxes) - tax_total) > 0.005:  # e.g. no item rows: keep the group total as income tax
        taxes = [tax_total - sum(taxes[1:])] + taxes[1:]
    other += [(n, v) for (n, _), v in zip(TAX_LINES, taxes)]
    return inc, opx, other


def cf_lines(CF, i):
    ni = row_val(CF, "Net income", i)
    adj = sum(val(r, i) for r in rows(CF, kind="item", section="Operating") if r.get("group") == "Non-cash items")
    wc = sum(val(r, i) for r in rows(CF, kind="item", section="Operating") if r.get("group") == "Changes in working capital")
    op = row_val(CF, "Net cash from operating activities", i)
    inv_items = {}
    for r in rows(CF, kind="item", section="Investing"):
        name = INV_NAMES.get(r.get("group"), r["label"])
        inv_items[name] = inv_items.get(name, 0) + val(r, i)
    fin_items = {}
    for r in rows(CF, kind="item", section="Financing"):
        name = FIN_NAMES.get(r.get("group"), r["label"])
        fin_items[name] = fin_items.get(name, 0) + val(r, i)
    return dict(ni=ni, adj=adj, wc=wc, op=op, inv=inv_items, invt=row_val(CF, "Net cash from investing activities", i),
                fin=fin_items, fint=row_val(CF, "Net cash from financing activities", i),
                net=row_val(CF, "Net change in cash", i), start=row_val(CF, "Cash at start of period", i),
                end=row_val(CF, "Cash at end of period", i), bs=row_val(CF, "Cash on the balance sheet at end of period", i),
                check=row_val(CF, "Check: start + net change - balance-sheet cash (must be 0)", i),
                memo=[(memo_label(r), val(r, i)) for r in rows(CF, kind="memo") if val(r, i)])


MEMO_GROUP = {"Construction in progress": "[[cip|Construction in progress (CIP)]]", "Land": "Land",
              "Home (at cost)": "Home improvements", "Vehicles": "Vehicles"}


def memo_label(r):
    """Label (markup) for a non-cash memo row keyed 'Group|how'."""
    g, _, how = r.get("key", "").partition("|")
    name = MEMO_GROUP.get(g, g or r["label"])
    if how == "new loans":
        return f"{name} acquired with new debt (lender paid directly)"
    if how.startswith("amounts owed"):
        return f"{name} acquired through [[ap|accounts payable]] (later payments are under investing)"
    return r["label"].replace("Non-cash: ", "").capitalize()


def find_db(explicit=""):
    """Resolve the ledger SQLite path for optional cash-flow highlights."""
    for p in (explicit, os.environ.get("FINANCE_DB", ""),
              "/workspace/finance-data/finance.db",
              os.path.join(os.path.dirname(__file__), "..", "..", "finance-data", "finance.db")):
        if p and os.path.isfile(p):
            return os.path.abspath(p)
    return ""


def cash_flow_highlights(db_path, start, end, limit=5):
    """Biggest YTD cash movements in plain English (3-5), from the ledger.

    Aggregates related payments (income taxes, medical bills, land, share sales, …)
    so the highlight block names the economic event, not every bank line.
    """
    if not db_path or not start or not end:
        return []
    try:
        import sqlite3
        db = sqlite3.connect(db_path)
    except Exception as e:
        print(f"statements: highlights skipped ({e})", file=sys.stderr)
        return []
    db.row_factory = sqlite3.Row
    # Per cash-touching entry: net cash effect and the non-cash counterpart accounts.
    rows = list(db.execute("""
        WITH cash AS (
          SELECT entry_id, date, SUM(amount_minor) AS cash_minor
          FROM v_ledger
          WHERE date >= ? AND date <= ?
            AND cash_flow_class = 'cash' AND entry_type <> 'opening'
          GROUP BY entry_id, date
        ),
        other AS (
          SELECT entry_id,
                 GROUP_CONCAT(DISTINCT account) AS accounts,
                 GROUP_CONCAT(DISTINCT IFNULL(counterparty, '')) AS cps,
                 GROUP_CONCAT(DISTINCT cash_flow_class) AS classes,
                 GROUP_CONCAT(DISTINCT IFNULL(report_line, '')) AS lines,
                 MAX(description) AS description
          FROM v_ledger
          WHERE cash_flow_class <> 'cash'
          GROUP BY entry_id
        )
        SELECT c.date, c.cash_minor, o.accounts, o.cps, o.classes, o.lines, o.description
        FROM cash c JOIN other o ON o.entry_id = c.entry_id
        WHERE ABS(c.cash_minor) >= 10000
        """, (start, end)))
    buckets = {}  # label -> cents (signed; cash in +, cash out -; we store cash effect on cash account flipped)

    def add(label, cash_minor):
        # cash_minor is debit-positive on the cash account: + = money in, - = money out.
        buckets[label] = buckets.get(label, 0) + cash_minor

    for r in rows:
        cash = int(r["cash_minor"])
        accts = r["accounts"] or ""
        cps = (r["cps"] or "").lower()
        desc = (r["description"] or "").lower()
        lines = (r["lines"] or "").lower()
        classes = r["classes"] or ""
        # Skip pure transfers between our own cash accounts.
        acct_list = [a for a in accts.split(",") if a]
        if acct_list and all(
            a.startswith("Assets:Bank:") or a.startswith("Assets:Transfers:") or a.startswith("Assets:Cash")
            for a in acct_list
        ):
            continue
        if "Liabilities:Accrued:Taxes" in accts or "usataxpymt" in desc or "tax paymnt" in desc or (
            "irs" in cps and "tax" in (lines + desc + cps)
        ):
            for yl in HL["tax_year_labels"]:
                if yl["acct"] in accts or (yl.get("desc") and yl["desc"] in desc):
                    add(yl["label"], cash)
                    break
            else:
                add("[[income_tax|Income taxes]]", cash)
            continue
        if any(x in accts for x in HL["land"]["accts"]) or any(x in cps for x in HL["land"]["cps"]) or (
            "Assets:RealEstate" in accts and "Land" in accts
        ):
            add(HL["land"]["label"], cash)
            continue
        if "Payable:Medical" in accts or any(x in cps for x in HL["medical"]["cps"]) or \
                any(x in desc for x in HL["medical"]["desc"]):
            add(HL["medical"]["label"], cash)
            continue
        SP = HL["sale_proceeds"]
        if (
            any(x in cps for x in SP["cps"])
            or any(x in desc for x in SP["desc"])
            or any(x in accts.lower() for x in SP["accts"])
            or (BROKER in cps and any(x in desc for x in SP["brokerage_desc"]))
            or ("Investments" in accts and cash > 0 and "investing" in classes)
        ):
            # Share / brokerage sale proceeds (cash in)
            if cash > 0:
                add(SP["label"], cash)
                continue
        if "investing" in classes and cash < 0 and (
            "Crypto" in accts or "Investments" in accts or "Retirement" in lines or "Crypto" in lines
        ):
            add("[[inv_purchases|Purchases of investments]] (brokerage, retirement, crypto)", cash)
            continue
        if "investing" in classes and ("CIP" in accts or "Construction" in lines or "RealEstate" in accts):
            add("[[cip|Construction in progress]] (design and engineering)", cash)
            continue
        if (HL["business_account_token"] and HL["business_account_token"] in accts) or "distributions" in lines:
            if cash > 0:
                add("[[scorp|S-corp distributions]]", cash)
                continue
        if "Mortgage" in accts or "mortgage" in lines:
            add("Mortgage principal repaid", cash)
            continue

    # Rank by absolute size; keep 3-5.
    ranked = sorted(buckets.items(), key=lambda kv: -abs(kv[1]))
    out = []
    for label, cents in ranked:
        if abs(cents) < 500000:  # under $5k: not a "highlight"
            continue
        # Present as cash movement: negative cash account = cash out
        out.append((label, cents / 100.0))
        if len(out) >= limit:
            break
    return out


# ---------------------------------------------------------------- html

# Light and dark palettes (CSS custom properties). The HTML follows the viewer's system setting
# (prefers-color-scheme); class="light" / class="dark" on <html> pins one theme (used for the PDFs).
# Text contrast is WCAG AA or better on both backgrounds (muted text on #111: ~7.7:1, red ~7:1).
LIGHT = ("--bg:#ffffff; --card:#ffffff; --ink:#1b1f24; --muted:#5b6470; --line:#e3e6ea; --rule:#1b1f24; "
         "--pos:#17663a; --neg:#a12a2a; --accent:#2457a6; --axis:#9aa3ad; --bar-pos:#2f8f5b; --bar-neg:#c0504d; "
         "--tip-bg:#ffffff; --tip-line:#c9ced6; --tip-shadow:rgba(20,30,45,.22); --term-hl:#e8eefb;")
DARK = ("--bg:#111214; --card:#17191c; --ink:#ecebe8; --muted:#a3abb5; --line:#2a2e34; --rule:#8b939c; "
        "--pos:#5cc98a; --neg:#ff7b72; --accent:#8ab4ff; --axis:#5f6872; --bar-pos:#3fae72; --bar-neg:#e5675f; "
        "--tip-bg:#25282d; --tip-line:#3d434b; --tip-shadow:rgba(0,0,0,.6); --term-hl:#24324a;")

CSS = """
:root { @LIGHT@ color-scheme: light dark; }
@media (prefers-color-scheme: dark) { :root:not(.light) { @DARK@ } }
:root.light { @LIGHT@ color-scheme: light; }
:root.dark { @DARK@ color-scheme: dark; }
* { box-sizing: border-box; }
html { -webkit-text-size-adjust: 100%; background: var(--bg);
       -webkit-print-color-adjust: exact; print-color-adjust: exact; }
body { margin: 0; font-family: -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
       color: var(--ink); background: var(--bg); font-size: 17px; line-height: 1.45; }
main { max-width: 480px; margin: 0 auto; padding: 16px 14px 40px; }
h1 { font-size: 24px; margin: 6px 0 2px; }
h2 { font-size: 21px; margin: 0 0 4px; color: var(--accent); }
.sub { color: var(--muted); font-size: 14px; margin: 0 0 14px; }
section { padding: 4px 0 18px; }
.page { break-before: page; }
.kpi { border: 1px solid var(--line); background: var(--card); border-radius: 12px; padding: 12px 14px; margin: 10px 0; }
.kpi .big { font-size: 30px; font-weight: 700; letter-spacing: -0.5px; }
.kpi .lbl { color: var(--muted); font-size: 14px; }
.grid { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; }
.grid .kpi { margin: 0; }
.grid .kpi .big { font-size: 22px; }
table { width: 100%; border-collapse: collapse; margin: 6px 0 4px; }
th { text-align: right; font-size: 13px; color: var(--muted); font-weight: 600; padding: 4px 0 6px 8px;
     border-bottom: 2px solid var(--rule); vertical-align: bottom; }
th:first-child { text-align: left; padding-left: 0; }
td { padding: 7px 0 7px 8px; border-bottom: 1px solid var(--line); text-align: right; white-space: nowrap;
     font-variant-numeric: tabular-nums; }
td:first-child { text-align: left; padding-left: 0; white-space: normal; }
tr.head td { font-weight: 700; border-bottom: 1px solid var(--rule); padding-top: 14px; }
tr.sub td { color: var(--muted); font-size: 15px; }
tr.sub td:first-child { padding-left: 14px; }
tr.total td { font-weight: 700; border-top: 2px solid var(--rule); border-bottom: none; }
tr.grand td { font-weight: 800; font-size: 19px; border-top: 3px double var(--rule); border-bottom: none; }
tr.ok td { color: var(--pos); font-size: 14px; }
tr.bad td { color: var(--neg); font-weight: 700; }
.neg { color: var(--neg); }
ul.take { padding-left: 20px; margin: 8px 0; }
ul.take li { margin: 0 0 9px; }
.notes { color: var(--muted); font-size: 13px; line-height: 1.4; padding-left: 16px; margin: 8px 0 0; }
.notes li { margin-bottom: 5px; }
.chart { margin: 8px 0 2px; }
.chart text { font-size: 10px; fill: var(--muted); }
.chart .axis { stroke: var(--axis); }
.chart .bar-pos { fill: var(--bar-pos); }
.chart .bar-neg { fill: var(--bar-neg); }
/* glossary terms: a tap-to-open definition (native popover; tiny script fallback). Print: dotted underline. */
button.term { -webkit-appearance: none; appearance: none; background: none; border: 0; border-radius: 3px; margin: 0;
       padding: 0; font: inherit; color: inherit; letter-spacing: inherit; line-height: inherit; text-align: inherit;
       cursor: help; -webkit-tap-highlight-color: transparent; touch-action: manipulation;
       text-decoration: underline dotted; text-decoration-color: var(--muted); text-decoration-thickness: 1.5px;
       text-underline-offset: 3px; white-space: normal; }
.nw { white-space: nowrap; }
button.term:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
button.term[aria-expanded="true"] { background: var(--term-hl); text-decoration-color: var(--accent); }
.term-p { white-space: normal; text-decoration: underline dotted; text-decoration-color: var(--muted); text-decoration-thickness: 1px;
          text-underline-offset: 3px; }
.tip { display: none; position: fixed; inset: 30vh auto auto 5vw; margin: 0;
       box-sizing: border-box; width: min(90vw, 320px); max-width: 90vw; max-height: 70vh; overflow: auto;
       padding: 10px 12px; border: 1px solid var(--tip-line); border-radius: 10px; background: var(--tip-bg);
       color: var(--ink); box-shadow: 0 8px 28px var(--tip-shadow); font-size: 15px; font-weight: 400;
       line-height: 1.4; text-align: left; white-space: normal; letter-spacing: normal; z-index: 20; }
.tip.open { display: block; }
.tip:popover-open { display: block; }
.tip b { font-weight: 700; }
dl.gloss { margin: 8px 0 0; font-size: 14px; line-height: 1.4; }
dl.gloss dt { font-weight: 700; margin-top: 9px; break-after: avoid; }
dl.gloss dd { margin: 2px 0 0; color: var(--muted); }
details.gloss { margin-top: 16px; border-top: 1px solid var(--line); padding-top: 10px; }
details.gloss summary { font-weight: 700; font-size: 17px; color: var(--accent); cursor: pointer; }
@page { size: 4.5in 11in; margin: 0.28in 0.25in; }
tr, .kpi, li, .keep { break-inside: avoid; }
h2 { break-after: avoid; }
@media print { body { font-size: 14px; line-height: 1.35; } main { padding: 0; max-width: none; }
  h1 { font-size: 22px; } .sub { margin-bottom: 8px; } .kpi .big { font-size: 24px; } .grid .kpi .big { font-size: 20px; }
  .kpi { margin: 5px 0; padding: 6px 12px; } .grid { gap: 8px; } td { padding: 3.5px 0 3.5px 8px; }
  tr.head td { padding-top: 8px; } tr.grand td { font-size: 16px; } tr.sub td { font-size: 13px; }
  ul.take li { margin: 2px 0; } section { padding-bottom: 4px; } .notes { font-size: 11.5px; }
  dl.gloss { font-size: 11px; line-height: 1.3; } dl.gloss dt { margin-top: 5px; } dl.gloss dd { margin-top: 1px; } }
""".replace("@LIGHT@", LIGHT).replace("@DARK@", DARK)


# Tap-to-open definitions. Native popover (iOS Safari 17+, Chrome 114+): popovertarget toggles
# it, a second tap on the term or a tap outside closes it (light dismiss). The script only places
# the bubble next to its term (inside the viewport) and keeps aria-expanded current; without
# popover support it toggles the bubble itself (tap again / outside / Escape closes). Tapping the
# bubble closes it too.
TIP_JS = """
(function () {
  var nat = typeof HTMLElement === "function" && HTMLElement.prototype.hasOwnProperty("popover");
  var btns = [].slice.call(document.querySelectorAll("button.term"));
  function tipOf(b) { return document.getElementById(b.getAttribute("popovertarget")); }
  function place(b, t) {
    var r = b.getBoundingClientRect(), de = document.documentElement;
    var vw = de.clientWidth, vh = de.clientHeight;
    var w = t.offsetWidth || Math.min(vw * 0.9, 320), h = t.offsetHeight;
    var x = Math.min(Math.max(8, r.left + r.width / 2 - w / 2), Math.max(8, vw - w - 8));
    var y = r.bottom + 6;
    if (h && y + h > vh - 8 && r.top - 6 - h >= 8) y = r.top - 6 - h;
    t.style.position = "absolute";
    t.style.left = Math.round(x + window.pageXOffset) + "px";
    t.style.top = Math.round(y + window.pageYOffset) + "px";
  }
  function closeAll(keep) {
    btns.forEach(function (b) {
      var t = tipOf(b);
      if (t && t !== keep && t.classList.contains("open")) { t.classList.remove("open"); b.setAttribute("aria-expanded", "false"); }
    });
  }
  btns.forEach(function (b) {
    var t = tipOf(b);
    if (!t) return;
    b.setAttribute("aria-expanded", "false");
    t.addEventListener("click", function (e) {  // tapping the bubble itself also closes it
      e.stopPropagation();
      if (nat) { t.hidePopover(); } else { closeAll(); }
    });
    if (nat) {
      t.addEventListener("beforetoggle", function (e) { if (e.newState === "open") place(b, t); });
      t.addEventListener("toggle", function (e) {
        var open = e.newState === "open";
        b.setAttribute("aria-expanded", open ? "true" : "false");
        if (open) place(b, t);
      });
    } else {
      b.addEventListener("click", function (e) {
        e.preventDefault(); e.stopPropagation();
        var open = !t.classList.contains("open");
        closeAll();
        if (open) { t.classList.add("open"); b.setAttribute("aria-expanded", "true"); place(b, t); }
      });
    }
  });
  if (!nat) {
    var outside = function (e) {
      var el = e.target;
      if (el && el.closest && (el.closest(".tip") || el.closest("button.term"))) return;
      closeAll();
    };
    document.addEventListener("click", outside);
    document.addEventListener("touchstart", outside, { passive: true });
    document.addEventListener("keydown", function (e) { if (e.key === "Escape") closeAll(); });
  }
  window.addEventListener("resize", function () {
    btns.forEach(function (b) {
      var t = tipOf(b);
      if (t && (t.classList.contains("open") || (nat && t.matches(":popover-open")))) place(b, t);
    });
  });
})();
"""


class Terms:
    """Renders label markup ([[key|text]]) for one document and records which terms were used."""

    def __init__(self, gloss, interactive):
        self.g, self.interactive, self.used, self.n = gloss, interactive, [], 0

    def term(self, key, text=None):
        if key not in self.g:
            raise KeyError(f"unknown glossary term {key!r}")
        text = self.g[key][0] if text is None else text
        if key not in self.used:
            self.used.append(key)
        if not self.interactive:
            return f'<span class="term-p">{text}</span>'
        self.n += 1
        tid = f"tip-{key}-{self.n}"
        title, defn = (html.escape(x) for x in self.g[key])
        return (f'<button type="button" class="term" popovertarget="{tid}" aria-controls="{tid}">{text}</button>'
                f'<span class="tip" id="{tid}" popover role="tooltip"><b>{title}.</b> {defn}</span>')

    def __call__(self, s, escape=True):
        """Markup -> HTML. escape=False for strings that already are HTML (takeaways). An opening
        bracket before a term and punctuation after it stay on the term's line."""
        s = html.escape(s, quote=False) if escape else s

        def sub(m):
            t = self.term(m.group(2), m.group(3))
            return f'<span class="nw">{m.group(1)}{t}{m.group(4)}</span>' if m.group(1) or m.group(4) else t
        return TERM_NW_RE.sub(sub, s)

    def glossary_html(self):
        items = sorted(self.used, key=lambda k: self.g[k][0].lower())
        return '<dl class="gloss">' + "".join(
            f"<dt>{html.escape(self.g[k][0])}</dt><dd>{html.escape(self.g[k][1])}</dd>" for k in items) + "</dl>"


def td(v, cls=""):
    c = "neg" if round(v) < 0 else ""
    return f'<td class="{c} {cls}">{money(v)}</td>'


def table(headers, body):
    """headers: already-rendered HTML."""
    h = "".join(f"<th>{x}</th>" for x in headers)
    return f"<table><thead><tr>{h}</tr></thead><tbody>{''.join(body)}</tbody></table>"


def tr(label, vals, cls=""):
    """label: already-rendered HTML."""
    return f'<tr class="{cls}"><td>{label}</td>' + "".join(td(v) for v in vals) + "</tr>"


def head(label):
    return f'<tr class="head"><td>{label}</td><td></td><td></td></tr>'


def chart_svg(labels, values, width=440, height=220):
    if not values:
        return ""
    vmax = max(max(values), 0) or 1
    vmin = min(min(values), 0)
    span = (vmax - vmin) or 1
    pad_top, pad_bot = 16, 34
    h = height - pad_top - pad_bot
    bw = width / len(values)
    zero = pad_top + h * vmax / span
    parts = [f'<svg class="chart" viewBox="0 0 {width} {height}" width="100%" role="img" aria-label="Net income by month">',
             f'<line x1="0" x2="{width}" y1="{zero:.1f}" y2="{zero:.1f}" class="axis" stroke="#9aa3ad" stroke-width="1"/>']
    for k, (lab, v) in enumerate(zip(labels, values)):
        bh = abs(v) * h / span
        y = zero - bh if v >= 0 else zero
        color, cls = ("#2f8f5b", "bar-pos") if v >= 0 else ("#c0504d", "bar-neg")  # attr = fallback; CSS var wins
        x = k * bw + bw * 0.15
        parts.append(f'<rect x="{x:.1f}" y="{y:.1f}" width="{bw*0.7:.1f}" height="{max(bh,1):.1f}" class="{cls}" fill="{color}" rx="2"/>')
        parts.append(f'<text x="{k*bw+bw/2:.1f}" y="{height-3}" text-anchor="middle">{html.escape(lab)}</text>')
        if v == max(values) or v == min(values):
            ty = y - 3 if v >= 0 else y + bh + 11
            parts.append(f'<text x="{k*bw+bw/2:.1f}" y="{ty:.1f}" text-anchor="middle">{short(v)}</text>')
    parts.append("</svg>")
    return "".join(parts)


def warn_sum(what, parts, total):
    if abs(sum(parts) - total) > 0.005:
        print(f"statements: {what}: lines sum to {sum(parts):,.2f} but the statement total is {total:,.2f}", file=sys.stderr)


def build_html(as_of, BS, IS, CF, notes, theme="", highlights=None):
    """theme: "" follows the system light/dark setting (interactive definitions); "light" or "dark"
    pins one for the PDFs (terms underlined, Glossary section at the end).
    highlights: optional list of (label markup, signed dollars) for the YTD cash-flow highlights block."""
    T = Terms(glossary(notes, IS), interactive=not theme)
    highlights = highlights or []
    nb = len(BS["columns"])
    i_open, i_now = 0, nb - 1
    open_lbl, now_lbl = BS["columns"][i_open]["label"], BS["columns"][i_now]["label"]
    iy, it = col_index(IS, key="ytd"), col_index(IS, key="ttm")
    ylbl = IS["columns"][iy]["label"]
    ttm_rng = f'{nice_date(IS["columns"][it]["from"])} to {nice_date(IS["columns"][it]["to"])}'

    def yh():  # column headers, each with its definition
        return T(ylbl.replace("YTD", "[[ytd|YTD]]"))

    def th():
        return T("[[ttm|TTM]]")

    def nw(i):
        return row_val(BS, "Net worth (assets minus liabilities)", i)

    def tot(name, i):
        return row_val(BS, name, i)

    inc_y, opx_y, oth_y = is_lines(IS, iy)
    inc_t, opx_t, oth_t = is_lines(IS, it)
    income_y, income_t = sum(v for _, v in inc_y), sum(v for _, v in inc_t)
    opex_y, opex_t = sum(v for _, v in opx_y), sum(v for _, v in opx_t)
    spend_y, spend_t = opex_y + sum(v for _, v in oth_y), opex_t + sum(v for _, v in oth_t)
    ni_y, ni_t = row_val(IS, "Net income", iy), row_val(IS, "Net income", it)
    warn_sum("net income (YTD)", [income_y, -spend_y], ni_y)
    warn_sum("net income (TTM)", [income_t, -spend_t], ni_t)
    rate_y = ni_y / income_y if income_y else 0
    rate_t = ni_t / income_t if income_t else 0
    cy, ct = cf_lines(CF, iy), cf_lines(CF, it)

    months = [(c["label"].split(" (")[0][:3] + ("*" if "(" in c["label"] else ""), k)
              for k, c in enumerate(IS["columns"]) if c["key"] not in ("ytd", "ttm", "total")]
    mvals = [row_val(IS, "Net income", k) for _, k in months]

    def flow(v, pos="provided", neg="used"):
        return f"{pos if v >= 0 else neg} {money(abs(v))}"

    # automatic takeaways (HTML + label markup)
    take = []
    d_nw = nw(i_now) - nw(i_open)
    take.append(f"[[networth|Net worth (equity)]] is <b>{money(nw(i_now))}</b>, {'up' if d_nw >= 0 else 'down'} "
                f"{money(abs(d_nw))} since {html.escape(open_lbl)}.")
    take.append(f"{html.escape(ylbl.replace('YTD', '[[ytd|YTD]]'))} net income is <b>{money(ni_y)}</b> on total income of "
                f"{money(income_y)}, a <b>{rate_y:.0%}</b> [[savings_rate|savings rate]] ([[ttm|TTM]]: {rate_t:.0%}).")
    top = sorted(opx_y + oth_y, key=lambda x: -x[1])[:3]
    take.append("Largest expenses [[ytd|YTD]]: " + ", ".join(f"{html.escape(plain(n)).lower()} {money(v)}" for n, v in top) + ".")
    take.append(f"Cash {'rose' if cy['net'] >= 0 else 'fell'} {money(abs(cy['net']))} [[ytd|YTD]]: "
                f"[[operating|operating activities]] {flow(cy['op'])}, [[investing|investing activities]] {flow(cy['invt'])} "
                f"and [[financing|financing activities]] {flow(cy['fint'])}"
                f"{' (debt repayment)' if cy['fint'] < 0 else ''}.")
    if mvals:
        take.append(f"Monthly net income swings a lot ({money(min(mvals))} to {money(max(mvals))}), mostly because "
                    "[[scorp|S-corp distributions]] and income tax expense land in particular months.")
    extra = notes.get("takeaways", [])
    if len(take) + len(extra) > 5:
        take.pop(2)  # largest expenses: the income statement page lists them
    take = (take + extra)[:5]

    foot = [
        "[[accrual|Accrual basis]]: income when earned, expenses when incurred. Whole dollars; totals may differ by $1 from rounding.",
        "Real estate, land, [[cip|construction in progress (CIP)]] and vehicles are carried [[at_cost|at cost]] or owner "
        "value, not market value. Brokerage and retirement accounts are [[mtm|mark-to-market]]; crypto is at cost.",
    ]
    if any(c["label"].endswith(")") for c in IS["columns"]):
        foot.append("* Partial month (to the report date); month-end accruals for it are not booked yet.")
    foot += notes.get("footnotes", [])

    out = []
    pin = f' class="{theme}"' if theme else ""
    scheme = theme or "light dark"
    out.append(f'<!doctype html><html lang="en"{pin}><head><meta charset="utf-8">'
               f'<meta name="viewport" content="width=device-width, initial-scale=1">'
               f'<meta name="color-scheme" content="{scheme}">'
               f'<title>Household financial statements, {html.escape(now_lbl)}</title><style>{CSS}</style></head><body><main>')
    # ---- page 1 summary
    hint = "Tap a dotted-underlined term for its definition." if not theme else "Dotted-underlined terms are defined in the Glossary at the end."
    out.append(f'<section><h1>Our finances</h1><p class="sub">As of {html.escape(now_lbl)} · {T("[[accrual|accrual basis]]")}'
               f'<br>{hint}</p>')
    out.append(f'<div class="kpi"><div class="lbl">{T("[[networth|Net worth (equity)]]")}</div><div class="big">{money(nw(i_now))}</div>'
               f'<div class="lbl">{"+" if d_nw >= 0 else "−"}{money(abs(d_nw))} since {html.escape(open_lbl)}</div></div>')
    out.append('<div class="grid">'
               f'<div class="kpi"><div class="lbl">{yh()} net income</div><div class="big">{money(ni_y)}</div>'
               f'<div class="lbl">{T("[[savings_rate|savings rate]]")} {rate_y:.0%}</div></div>'
               f'<div class="kpi"><div class="lbl">{th()} net income</div><div class="big">{money(ni_t)}</div>'
               f'<div class="lbl">savings rate {rate_t:.0%}</div></div></div>')
    out.append(table(["", yh(), th()], [
        tr("Total income", [income_y, income_t]),
        tr("Total expenses (incl. taxes)", [spend_y, spend_t]),
        tr("Net income", [ni_y, ni_t], "total"),
    ]))
    out.append(table(["Cash flows", yh(), th()], [
        tr(T("[[operating|Operating activities]]"), [cy["op"], ct["op"]]),
        tr(T("[[investing|Investing activities]]"), [cy["invt"], ct["invt"]]),
        tr(T("[[financing|Financing activities]]"), [cy["fint"], ct["fint"]]),
        tr("Net change in cash", [cy["net"], ct["net"]], "total"),
    ]))
    if highlights:
        def hl_line(label, amt):
            # amt is cash-account signed: + in, − out
            if amt < 0:
                return f"{T(label, escape=False)} — cash out <b>{money(abs(amt))}</b>"
            return f"{T(label, escape=False)} — cash in <b>{money(amt)}</b>"
        out.append('<h2 style="margin-top:14px">Cash flow highlights</h2>'
                   f'<p class="sub">{yh()}: the largest cash movements '
                   f'({T("[[gross_investing|gross investing lines]]")} and operating settlements)</p>'
                   '<ul class="take">' +
                   "".join(f"<li>{hl_line(lab, amt)}</li>" for lab, amt in highlights) +
                   "</ul>")
    out.append('<h2 style="margin-top:14px">Takeaways</h2><ul class="take">' +
               "".join(f"<li>{T(t, escape=False)}</li>" for t in take) + "</ul></section>")

    # ---- balance sheet
    def bs_block(lines, i, j):
        rows_, pa, pb = [], [], []
        for name, groups in lines:
            a, b = group_total(BS, groups, i), group_total(BS, groups, j)
            pa.append(a)
            pb.append(b)
            if round(a) or round(b):
                rows_.append(tr(T(name), [a, b]))
        return rows_, sum(pa), sum(pb)

    body = [head(T("[[current_assets|Current assets]]"))]
    r_, ca_a, ca_b = bs_block(CURRENT_ASSETS, i_open, i_now)
    warn_sum("current assets", [ca_b], tot("Total current assets", i_now))
    body += r_ + [tr("Total current assets", [ca_a, ca_b], "total"), head(T("[[noncurrent_assets|Non-current assets]]"))]
    r_, nca_a, nca_b = bs_block(NONCURRENT_ASSETS, i_open, i_now)
    warn_sum("total assets", [ca_b, nca_b], tot("Total assets", i_now))
    body += r_ + [tr("Total non-current assets", [nca_a, nca_b], "total"),
                  tr("Total assets", [tot("Total assets", i_open), tot("Total assets", i_now)], "grand"),
                  head(T("[[current_liabilities|Current liabilities]]"))]
    r_, cl_a, cl_b = bs_block(CURRENT_LIABS, i_open, i_now)
    warn_sum("current liabilities", [cl_b], tot("Total current liabilities", i_now))
    body += r_ + [tr("Total current liabilities", [cl_a, cl_b], "total"), head(T("[[ltd|Long-term debt]]"))]
    r_, lt_a, lt_b = bs_block(LONGTERM_DEBT, i_open, i_now)
    warn_sum("total liabilities", [cl_b, lt_b], tot("Total liabilities", i_now))
    body += r_ + [tr("Total long-term debt", [lt_a, lt_b], "total"),
                  tr("Total liabilities", [tot("Total liabilities", i_open), tot("Total liabilities", i_now)], "grand"),
                  tr(T("[[networth|Net worth (equity)]]"), [nw(i_open), nw(i_now)], "grand")]
    eq_names = {"opening-equity": "[[opening_equity|Opening equity]]",
                "net-income": "[[retained|Retained earnings]] (net income since books start)"}
    for r in rows(BS, kind="item", group="Equity"):
        if round(val(r, i_open)) or round(val(r, i_now)):
            body.append(tr(T(eq_names.get(r.get("key"), r["label"])), [val(r, i_open), val(r, i_now)], "sub"))
    chk = row_val(BS, "Check: assets - liabilities - equity (must be 0)", i_now)
    body.append(f'<tr class="{"ok" if round(chk, 2) == 0 else "bad"}"><td>Check: total assets − total liabilities = net worth (equity)</td>'
                f'<td></td><td>{"✓" if round(chk, 2) == 0 else money(chk)}</td></tr>')
    owed = [r for r in rows(BS, kind="item", group="Money owed to us") if round(val(r, i_now))]
    sub = "".join(tr(html.escape(r["label"]), [val(r, i_open), val(r, i_now)], "sub") for r in owed)
    hdr = ["", html.escape(open_lbl), html.escape(now_lbl)]
    out.append('<section class="page"><h2>Balance sheet</h2><p class="sub">Statement of financial position: what we own and owe</p>')
    out.append(table(hdr, body))
    if sub:
        out.append(f'<div class="keep"><p class="sub" style="margin-top:12px">{T("[[ar|Accounts receivable]]")} detail</p>'
                   + table(hdr, [sub]) + '</div>')
    out.append("</section>")

    # ---- income statement
    body = [head(T("[[revenue|Revenue and gains]]"))]
    for (n, a), (_, b) in zip(inc_y, inc_t):
        if round(a) or round(b):
            body.append(tr(T(n), [a, b]))
    body.append(tr("Total income", [income_y, income_t], "total"))
    body.append(head(T("[[opex|Operating expenses]]")))
    for (n, a), (_, b) in sorted(zip(opx_y, opx_t), key=lambda x: -x[0][1]):
        if round(a) or round(b):
            body.append(tr(T(n), [a, b]))
    body.append(tr("Total operating expenses", [opex_y, opex_t], "total"))
    body.append(head("Interest and taxes"))
    for (n, a), (_, b) in zip(oth_y, oth_t):
        if round(a) or round(b):
            body.append(tr(T(n), [a, b]))
    body.append(tr("Total expenses", [spend_y, spend_t], "total"))
    body.append(tr("Net income", [ni_y, ni_t], "grand"))
    body.append(f'<tr><td>{T("[[savings_rate|Savings rate]]")}</td><td>{rate_y:.0%}</td><td>{rate_t:.0%}</td></tr>')
    out.append(f'<section class="page"><h2>Income statement</h2><p class="sub">{yh()} and {th()} ({html.escape(ttm_rng)})</p>')
    out.append(table(["", yh(), th()], body))
    wages = [r for r in rows(IS, kind="item", group="Wages and salaries") if round(val(r, iy)) or round(val(r, it))]
    if wages:
        out.append('<div class="keep"><p class="sub" style="margin-top:12px">Wages and salaries by person</p>' +
                   table(["", yh(), th()], [tr(html.escape(r["label"]), [val(r, iy), val(r, it)], "sub") for r in wages]) + '</div>')
    out.append('<div class="keep"><p class="sub" style="margin-top:12px">Net income by month</p>' + chart_svg([m for m, _ in months], mvals) + '</div>')
    out.append("</section>")

    # ---- cash flow
    body = [head(T("Cash flows from [[operating|operating activities]]")),
            tr("Net income", [cy["ni"], ct["ni"]]),
            tr(T("[[noncash_adj|Adjustments for non-cash items]]"), [cy["adj"], ct["adj"]]),
            tr(T("[[wc|Changes in working capital]]"), [cy["wc"], ct["wc"]]),
            tr("Net cash from operating activities", [cy["op"], ct["op"]], "total"),
            head(T("Cash flows from [[investing|investing activities]]"))]
    inv_names = set(cy["inv"]) | set(ct["inv"])
    inv_ordered = [n for n in INV_ORDER if n in inv_names] + sorted(inv_names - set(INV_ORDER), key=plain)
    for n in inv_ordered:
        body.append(tr(T(n), [cy["inv"].get(n, 0), ct["inv"].get(n, 0)]))
    body.append(tr("Net cash from investing activities", [cy["invt"], ct["invt"]], "total"))
    body.append(head(T("Cash flows from [[financing|financing activities]]")))
    for n in sorted(set(cy["fin"]) | set(ct["fin"]), key=plain):
        body.append(tr(T(n), [cy["fin"].get(n, 0), ct["fin"].get(n, 0)]))
    body.append(tr("Net cash from financing activities", [cy["fint"], ct["fint"]], "total"))
    body.append(tr("Net change in cash", [cy["net"], ct["net"]], "total"))
    body.append(tr("Cash and cash equivalents, beginning of period", [cy["start"], ct["start"]]))
    body.append(tr("Cash and cash equivalents, end of period", [cy["end"], ct["end"]], "grand"))
    ok = round(cy["check"], 2) == 0 and round(ct["check"], 2) == 0
    body.append(f'<tr class="{"ok" if ok else "bad"}"><td>Check: ties to cash on the balance sheet</td>'
                f'<td>{"✓" if round(cy["check"],2)==0 else money(cy["check"])}</td><td>{"✓" if round(ct["check"],2)==0 else money(ct["check"])}</td></tr>')
    out.append(f'<section class="page"><h2>Statement of cash flows</h2><p class="sub">{T("[[indirect|Indirect method]]")}: '
               'net income adjusted to cash</p>')
    out.append(table(["", yh(), th()], body))
    if cy["memo"] or ct["memo"]:
        labs = sorted(set(l for l, _ in cy["memo"]) | set(l for l, _ in ct["memo"]), key=plain)
        md, mt = dict(cy["memo"]), dict(ct["memo"])
        out.append(f'<div class="keep"><p class="sub" style="margin-top:12px">{T("[[noncash|Non-cash investing and financing activities]]")}'
                   ' (for reference)</p>' +
                   table(["", yh(), th()], [tr(T(l), [md.get(l, 0), mt.get(l, 0)], "sub") for l in labs]) + '</div>')
    out.append('<h2 style="margin-top:16px;font-size:17px">Notes</h2><ul class="notes">' +
               "".join(f"<li>{T(f)}</li>" for f in foot) + "</ul></section>")
    if theme:
        out.append('<section class="page"><h2>Glossary</h2><p class="sub">Terms underlined with dots in the statements</p>'
                   + T.glossary_html() + "</section>")
    else:
        out.append('<details class="gloss"><summary>Glossary</summary>' + T.glossary_html() + "</details>")
        out.append(f"<script>{TIP_JS}</script>")
    out.append("</main></body></html>")
    return "".join(out)


# ---------------------------------------------------------------- xlsx

# statement row labels (from `finance report`) -> standard terms for the workbook
XLSX_LABELS = {
    # balance sheet
    "Investments": "Non-current assets: investments",
    "Property and equipment": "Non-current assets: property, plant and equipment",
    "Total property and equipment": "Total property, plant and equipment",
    "Other assets": "Non-current assets: other",
    "Total other assets": "Total other non-current assets",
    "Long-term liabilities": "Long-term debt",
    "Total long-term liabilities": "Total long-term debt",
    "Equity": "Net worth (equity)",
    "Total equity": "Total net worth (equity)",
    "Total liabilities and equity": "Total liabilities and net worth (equity)",
    "Net worth (assets minus liabilities)": "Net worth (equity) = total assets - total liabilities",
    "Check: assets - liabilities - equity (must be 0)": "Check: assets - liabilities - net worth (equity) (must be 0)",
    # income statement
    "Income": "Revenue",
    "Total income": "Total revenue",
    "Living expenses": "Operating expenses",
    "Total living expenses": "Total operating expenses",
    "Operating surplus (income minus living expenses)": "Operating income (revenue - operating expenses)",
    "Non-operating": "Non-operating income (expense)",
    "Net non-operating income": "Total non-operating income (expense)",
    "Net income before taxes": "Income before taxes",
    "Taxes": "Tax expense (income, payroll and property)",
    "Total taxes": "Total tax expense",
    "Savings rate (net income / all income)": "Savings rate (net income / total income incl. investment and other income)",
    # cash flow
    "Cash from operating activities (earning and spending)": "Cash flows from operating activities",
    "Cash from investing activities (investing and building)": "Cash flows from investing activities",
    "Cash from financing activities (borrowing and repaying)": "Cash flows from financing activities",
    "Non-cash items (pay received in shares, depreciation)": "Adjustment: non-cash items (stock-based pay, depreciation)",
    "Less investment gains (any sale proceeds are under investing)": "Adjustment: investment gains (sale proceeds are under investing)",
    "Cash at start of period": "Cash and cash equivalents, beginning of period",
    "Cash at end of period": "Cash and cash equivalents, end of period",
    "Cash on the balance sheet at end of period": "Cash and cash equivalents per balance sheet, end of period",
    "Land purchase payments": "Land purchase payments",
    "Construction in progress": "Construction in progress (CIP): design and engineering",
    "Purchases of investments": "Purchases of investments (brokerage, retirement, crypto)",
    "Proceeds from sales of investments": "Proceeds from sales of investments",
    "Proceeds from land sales": "Proceeds from land sales",
    "Home improvements": "Home improvements",
    "Vehicles bought": "Vehicles bought",
    "Vehicles sold": "Vehicles sold",
}
# group rows (balance sheet / income statement) and "Change in <group>" working-capital lines
XLSX_GROUPS = {
    "Cash": "Cash and cash equivalents", "Money owed to us": "Accounts receivable", "Escrow": "Escrow deposits",
    "Home (at cost)": "Real estate: home (at cost)", "Land": "Land (at cost)",
    "Construction in progress": "Construction in progress (CIP)", "Vehicles": "Vehicles (owner value)",
    "Other assets": "Other non-current assets", "Bills and payables": "Accounts payable",
    "Pending funding": "Pending funding (not posted yet)",
    "Accrued taxes": "Accrued liabilities: taxes", "Accrued interest and other": "Accrued liabilities: interest and other",
    "Owed to others (pass-through)": "Due to others (pass-through)", "Other liabilities": "Other current liabilities",
    "Mortgages": "Mortgage notes payable", "Loans": "Related-party note", "Equity": "Net worth (equity)",
    "Business income": "S-corp distributions and business income",
    "Investment gains": "Investment gains (realized and unrealized)",
    "Interest and dividends": "Interest and dividend income", "Interest on loans": "Interest expense on loans",
    "Taxes": "Income, payroll and property taxes", "Giving": "Charitable giving",
    "Entertainment and travel": "Travel and entertainment",
}
XLSX_EQUITY = {"opening-equity": "Opening equity (balances at books start)",
               "net-income": "Retained earnings (net income since books start)"}


def xlsx_label(r):
    k, label, g = r["kind"], r["label"], r.get("group")
    if k == "memo":
        return plain(memo_label(r))
    if k == "group" and label == g and g in XLSX_GROUPS:
        return XLSX_GROUPS[g]
    if k == "item":
        if r.get("section") == "Investing" and g in INV_NAMES:
            return plain(INV_NAMES[g])
        if r.get("section") == "Financing" and g in FIN_NAMES:
            return plain(FIN_NAMES[g])
        if g == "Equity" and r.get("key") in XLSX_EQUITY:
            return XLSX_EQUITY[r["key"]]
        if label.startswith("Change in "):
            rest = label[len("Change in "):]
            for gg, new in XLSX_GROUPS.items():
                if rest == gg.lower():
                    return "Change in " + new[0].lower() + new[1:]
    if k in ("section", "subtotal", "total", "ratio", "check", "item") and label in XLSX_LABELS:
        return XLSX_LABELS[label]
    return label


def build_xlsx(path, as_of, stmts, summary_rows, gloss):
    from openpyxl import Workbook
    from openpyxl.styles import Alignment, Border, Font, PatternFill, Side
    from openpyxl.utils import get_column_letter

    fmt = '#,##0.00;(#,##0.00);"-"'
    pct = '0.0%;(0.0%)'
    wb = Workbook()
    ws0 = wb.active
    ws0.title = "Summary"
    ws0.append(["Household financial statements", f"as of {as_of}"])
    ws0["A1"].font = Font(bold=True, size=14)
    ws0.append(["Accrual basis; definitions of the terms are on the Glossary sheet."])
    ws0["A2"].font = Font(italic=True, color="5B6470")
    ws0.append([])
    for r in summary_rows:
        ws0.append(r)
        is_pct = bool(r) and str(r[0]).startswith("Savings rate")
        for c in ws0[ws0.max_row][1:]:
            if isinstance(c.value, (int, float)):
                c.number_format = pct if is_pct else fmt
    ws0.column_dimensions["A"].width = 48
    for col in "BCD":
        ws0.column_dimensions[col].width = 18
    bold = Font(bold=True)
    head_fill = PatternFill("solid", fgColor="DCE6F1")
    sec_fill = PatternFill("solid", fgColor="F2F2F2")
    thin = Side(style="thin", color="000000")
    names = {"balance-sheet": "Balance sheet", "income-statement": "Income statement", "cash-flow": "Cash flow statement"}
    for s in stmts:
        ws = wb.create_sheet(names[s["kind"]])
        ws.append([s["title"] + " - " + s["basis"] + " basis"] + [c["label"] for c in s["columns"]])
        for c in ws[1]:
            c.font = bold
            c.fill = head_fill
            c.alignment = Alignment(horizontal="center" if c.column > 1 else "left", wrap_text=True, vertical="bottom")
        ws.row_dimensions[1].height = 32
        memo_head = False
        for r in s["rows"]:
            k = r["kind"]
            if k == "memo" and not memo_head:
                memo_head = True
                ws.append(["Non-cash investing and financing activities (not in the cash totals)"])
                for c in ws[ws.max_row]:
                    c.fill = sec_fill
                    c.font = bold
            label = xlsx_label(r)
            indent = {"section": 0, "group": 1, "item": 3, "subtotal": 1, "total": 0, "check": 0, "ratio": 0, "memo": 1}.get(k, 0)
            vals = [float(v) for v in r.get("values", [])] if r.get("values") else [None] * len(s["columns"])
            ws.append([label] + vals)
            row = ws[ws.max_row]
            row[0].alignment = Alignment(indent=indent)
            for c in row[1:]:
                c.number_format = pct if k == "ratio" else fmt
            if k in ("section",):
                for c in row:
                    c.fill = sec_fill
                    c.font = bold
            if k in ("subtotal", "group"):
                for c in row:
                    c.font = Font(bold=(k == "subtotal"))
            if k == "total":
                for c in row:
                    c.font = bold
                    c.border = Border(top=thin)
            if k == "check":
                for c in row:
                    c.font = Font(italic=True, color="17663A")
            if k == "memo":
                for c in row:
                    c.font = Font(italic=True, color="5B6470")
        ws.append([])
        for n in s.get("notes", []):
            ws.append([n])
            ws[ws.max_row][0].font = Font(italic=True, color="5B6470")
        ws.freeze_panes = "B2"
        ws.column_dimensions["A"].width = 62
        for j in range(2, len(s["columns"]) + 2):
            ws.column_dimensions[get_column_letter(j)].width = 15
        ws.sheet_view.zoomScale = 90
    wg = wb.create_sheet("Glossary")
    wg.append(["Term", "Definition"])
    for c in wg[1]:
        c.font = bold
        c.fill = head_fill
    for key in sorted(gloss, key=lambda k: gloss[k][0].lower()):
        wg.append(gloss[key])
        wg[wg.max_row][0].font = bold
        for c in wg[wg.max_row]:
            c.alignment = Alignment(wrap_text=True, vertical="top")
    wg.column_dimensions["A"].width = 36
    wg.column_dimensions["B"].width = 110
    wg.freeze_panes = "A2"
    wb.save(path)


# ---------------------------------------------------------------- main

def find_chrome(explicit):
    for c in [explicit, "google-chrome", "chromium", "chromium-browser", "google-chrome-stable"]:
        if c and shutil.which(c):
            return shutil.which(c)
    return None


# Reproducible outputs: the same statements.json must give byte-identical files on a rerun, so
# the render time stamps (xlsx core properties + zip entry times, PDF CreationDate/ModDate) are
# set to the statements' as-of date instead of the wall clock.
def reproducible_xlsx(path, as_of):
    import re
    import zipfile
    y, m, d = (int(x) for x in as_of.split("-"))
    stamp = f"{as_of}T00:00:00Z"
    with zipfile.ZipFile(path) as z:
        items = [(i, z.read(i.filename)) for i in z.infolist()]
    tmp = path + ".tmp"
    with zipfile.ZipFile(tmp, "w") as out:
        for info, data in items:
            if info.filename == "docProps/core.xml":
                data = re.sub(rb"(<dcterms:(?:created|modified)[^>]*>)[^<]*(</dcterms:)", lambda mt: mt.group(1) + stamp.encode() + mt.group(2), data)
            zi = zipfile.ZipInfo(info.filename, date_time=(y, m, d, 0, 0, 0))
            zi.compress_type, zi.external_attr = info.compress_type, info.external_attr
            out.writestr(zi, data)
    os.replace(tmp, path)


def reproducible_pdf(path, as_of):
    import re
    b = open(path, "rb").read()
    fixed = as_of.replace("-", "").encode() + b"000000"
    # same length as Chrome's D:YYYYMMDDHHMMSS, so the xref offsets stay valid
    b2 = re.sub(rb"(/(?:CreationDate|ModDate) \(D:)\d{14}", lambda mt: mt.group(1) + fixed, b)
    if b2 != b:
        open(path, "wb").write(b2)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("json")
    ap.add_argument("outdir")
    ap.add_argument("--notes", default="")
    ap.add_argument("--chrome", default="")
    ap.add_argument("--db", default="", help="ledger SQLite for cash-flow highlights (default: FINANCE_DB or ../finance-data/finance.db)")
    a = ap.parse_args()
    os.makedirs(a.outdir, exist_ok=True)
    as_of, BS, IS, CF = load(a.json)
    notes = json.load(open(a.notes)) if a.notes and os.path.exists(a.notes) else {}
    iy = col_index(IS, key="ytd")
    ytd_from = (IS["columns"][iy] or {}).get("from") if iy is not None else None
    ytd_to = (IS["columns"][iy] or {}).get("to") if iy is not None else as_of
    highlights = cash_flow_highlights(find_db(a.db), ytd_from, ytd_to)
    page = build_html(as_of, BS, IS, CF, notes, highlights=highlights)
    hp = os.path.join(a.outdir, "statements.html")
    open(hp, "w").write(page)

    iy, it = col_index(IS, key="ytd"), col_index(IS, key="ttm")
    nb = len(BS["columns"])

    def totals(i):
        inc, opx, oth = is_lines(IS, i)
        t_inc = sum(v for _, v in inc)
        return t_inc, sum(v for _, v in opx + oth), (row_val(IS, "Net income", i) / t_inc if t_inc else 0)

    (inc_y, sp_y, rate_y), (inc_t, sp_t, rate_t) = totals(iy), totals(it)
    cf = lambda lbl: [lbl, row_val(CF, lbl, iy), row_val(CF, lbl, it)]
    summ = [["Net worth (equity)", row_val(BS, "Net worth (assets minus liabilities)", nb - 1)],
            ["Total assets", row_val(BS, "Total assets", nb - 1)],
            ["Total liabilities", row_val(BS, "Total liabilities", nb - 1)],
            [],
            ["", IS["columns"][iy]["label"], "TTM (trailing 12 months)"],
            ["Total income (revenue and gains)", inc_y, inc_t],
            ["Total expenses (incl. taxes)", sp_y, sp_t],
            ["Net income", row_val(IS, "Net income", iy), row_val(IS, "Net income", it)],
            ["Savings rate (net income / total income)", rate_y, rate_t],
            cf("Net cash from operating activities"),
            cf("Net cash from investing activities"),
            cf("Net cash from financing activities"),
            cf("Net change in cash")]
    try:
        xp = os.path.join(a.outdir, "statements.xlsx")
        build_xlsx(xp, as_of, [BS, IS, CF], summ, glossary(notes, IS))
        reproducible_xlsx(xp, as_of)
    except ImportError:
        print("statements: openpyxl not installed; skipped the workbook", file=sys.stderr)

    chrome = find_chrome(a.chrome)
    if not chrome:
        print("statements: no Chrome/Chromium found; skipped the PDFs", file=sys.stderr)
        return 0
    rc = 0
    for theme, name in (("light", "statements.pdf"), ("dark", "statements-dark.pdf")):
        pdf = os.path.join(a.outdir, name)
        # pinned-theme copy of the page, printed then removed (statements.html itself follows the system)
        fd, tmp = tempfile.mkstemp(prefix=f".statements-{theme}-", suffix=".html", dir=a.outdir)
        with os.fdopen(fd, "w") as f:
            f.write(build_html(as_of, BS, IS, CF, notes, theme=theme, highlights=highlights))
        try:
            if os.path.exists(pdf):
                os.remove(pdf)
            cmd = [chrome, "--headless=new", "--disable-gpu", "--no-sandbox", "--no-pdf-header-footer",
                   f"--print-to-pdf={pdf}", "file://" + os.path.abspath(tmp)]
            r = subprocess.run(cmd, capture_output=True, text=True, timeout=120)
            if r.returncode != 0 or not os.path.exists(pdf):
                print(f"statements: {name} render failed: " + r.stderr[-400:], file=sys.stderr)
                rc = 1
            else:
                reproducible_pdf(pdf, as_of)
        finally:
            os.remove(tmp)
    return rc


if __name__ == "__main__":
    sys.exit(main())
