"""Transaction detail behind the statement lines (Profit and Loss Statement, Balance Sheet,
Statement of Cash Flows), read from the ledger view v_ledger, for the expanding sections of
statements.html.

The statement JSON (`finance report statements`) holds one row per account group and account
key; this module re-reads the ledger lines with the same classification as
internal/report/classified.go (ported, not re-invented) and assigns every line to the statement
row it landed in, so each expanding row's subtotal equals the statement figure (verified in
Detail.verify; mismatches are reported on stderr). Nothing personal lives in this file.

Safety: only the posted description and counterparty of a line are shown (never memo, raw
reference or external id), and scrub() masks anything that looks like an SSN, a long account
or card number (kept: the last 4) or a wallet address.
"""
import html
import re
import sqlite3
import sys

# ---------------------------------------------------------------- text helpers

_SSN = re.compile(r"\b\d{3}-\d{2}-\d{4}\b")
_DIGITS = re.compile(r"\d{5,}")
_HEX = re.compile(r"\b0x[0-9a-fA-F]{8,}\b")
_ADDR = re.compile(r"\b(?:[a-z][a-z0-9]{2,12}:)?[A-Za-z0-9]{26,}\b")


def scrub(s):
    s = _SSN.sub("[ssn]", s or "")
    s = _HEX.sub("[address]", s)
    s = _ADDR.sub("[address]", s)
    return _DIGITS.sub(lambda m: "…" + m.group(0)[-4:], s)


def _split_camel(s):
    r = list(s)
    cuts = []
    for i in range(1, len(r)):
        l2u = (r[i - 1].islower() or r[i - 1].isdigit()) and r[i].isupper()
        acr = r[i - 1].isupper() and r[i].isupper() and i + 1 < len(r) and r[i + 1].islower()
        if l2u or acr:
            cuts.append(i)
    out, start = [], 0
    for k, c in enumerate(cuts):
        nxt = cuts[k + 1] if k + 1 < len(cuts) else len(r)
        left, right = r[start:c], r[c:nxt]
        all_caps = "".join(left).upper() == "".join(left)
        if (len(left) >= 3 or all_caps) and len(right) >= 3:
            out.append("".join(left))
            start = c
    out.append("".join(r[start:]))
    return out


def humanize(seg):
    words = []
    for part in re.split(r"[-_ ]+", seg):
        if part:
            words += _split_camel(part)
    return " ".join(words)


def human_join(segs):
    return " ".join(humanize(s) for s in segs)


# ---------------------------------------------------------------- ledger classification (port of classified.go)

_RETIRE = re.compile(r"(?i)\b(ira|roth|401k|403b|retire|hsa)|IRA|Roth|401k|403b")


def classify_bs(account, atype, rline, cfclass):
    """-> (section, group, key, item) as internal/report/classified.go classifyBS."""
    s = account.split(":")
    has = account.startswith

    def rest(i):
        return human_join(s[i:]) if len(s) > i else humanize(s[-1])
    if atype == "asset":
        if has("Assets:Bank:") or has("Assets:Cash") or cfclass == "cash":
            if len(s) > 2:
                return "Current assets", "Cash", ":".join(s[:3]), humanize(s[2])
            return "Current assets", "Cash", account, humanize(s[-1])
        if has("Assets:Receivable") or rline == "Receivables":
            return "Current assets", "Money owed to us", account, rest(2)
        if has("Assets:Prepaid") or rline == "Prepaid expenses":
            return "Current assets", "Prepaid expenses", account, rest(2)
        if has("Assets:Escrow"):
            return "Current assets", "Escrow", account, rest(2)
        if has("Assets:Transfers"):
            return "Current assets", "Transfers in transit", account, rest(2)
        if has("Assets:Investments"):
            if _RETIRE.search(account):
                return "Investments", "Retirement accounts", account, rest(2)
            return "Investments", "Brokerage and private shares", account, rest(2)
        if has("Assets:Crypto"):
            return "Investments", "Crypto", account, rest(2)
        if has("Assets:RealEstate"):
            if ":CIP" in account or ":Construction" in account:
                return "Property and equipment", "Construction in progress", account, rest(2)
            if ":Land" in account or ":Lot" in account:
                return "Property and equipment", "Land", account, rest(2)
            return "Property and equipment", "Home (at cost)", account, rest(2)
        if has("Assets:Land"):
            return "Property and equipment", "Land", account, rest(1)
        if has("Assets:Vehicles"):
            return "Property and equipment", "Vehicles", account, rest(2)
        return "Other assets", "Other assets", account, rest(1)
    if atype == "liability":
        if has("Liabilities:CreditCard") or rline == "Credit cards":
            return "Current liabilities", "Credit cards", account, rest(2)
        if has("Liabilities:Payable") or has("Liabilities:Vendor") or rline == "Payables":
            return "Current liabilities", "Bills and payables", account, rest(2)
        if has("Liabilities:Accrued:Taxes"):
            return "Current liabilities", "Accrued taxes", account, rest(3)
        if has("Liabilities:Accrued"):
            return "Current liabilities", "Accrued interest and other", account, rest(2)
        if has("Liabilities:Pending"):
            return "Current liabilities", "Pending funding", account, rest(2)
        if has("Liabilities:PassThrough"):
            return "Current liabilities", "Owed to others (pass-through)", account, rest(2)
        if has("Liabilities:Mortgage") or rline == "Mortgages":
            return "Long-term liabilities", "Mortgages", account, rest(2)
        if has("Liabilities:Loan") or rline == "Loans":
            return "Long-term liabilities", "Loans", account, rest(2)
        return "Current liabilities", "Other liabilities", account, rest(1)
    return "Equity", "Equity", "opening-equity", "Opening equity (balances at books start)"


_IS_CAT = {
    "Salary": "Wages and salaries", "Business income": "Business income", "Other income": "Other income",
    "Uncategorized income": "Other income", "Interest": "Interest and dividends",
    "Investment income": "Interest and dividends", "Realized gains": "Investment gains",
    "Unrealized gains": "Investment gains", "Retirement distributions": "Retirement distributions",
    "Housing": "Housing", "Utilities": "Utilities", "Food": "Food", "Transport": "Transportation",
    "Healthcare": "Healthcare", "Baby": "Baby and childcare", "Childcare": "Baby and childcare",
    "Insurance": "Insurance", "Shopping": "Shopping", "Subscriptions": "Subscriptions and software",
    "Software": "Subscriptions and software", "Education": "Education", "Entertainment": "Entertainment and travel",
    "Travel": "Entertainment and travel", "Personal care": "Personal care, gifts and other",
    "Gifts": "Personal care, gifts and other", "Fees": "Personal care, gifts and other",
    "Other expenses": "Personal care, gifts and other", "Uncategorized expenses": "Personal care, gifts and other",
    "Charity": "Giving", "Taxes": "Taxes", "Interest expense": "Interest on loans",
}
_IS_SECTION = {
    "Wages and salaries": "Income", "Business income": "Income", "Other income": "Income",
    "Interest and dividends": "Non-operating", "Investment gains": "Non-operating",
    "Retirement distributions": "Non-operating", "Interest on loans": "Non-operating", "Taxes": "Taxes",
}


def classify_is(account, atype, rline):
    """-> (section, group)"""
    g = _IS_CAT.get(rline)
    if g is None:
        g = "Other income" if atype == "income" else "Personal care, gifts and other"
    return _IS_SECTION.get(g, "Living expenses"), g


def linkable(account):
    return account.startswith(("Assets:Receivable", "Liabilities:Payable", "Liabilities:Vendor"))


def investing_line(grp, amt):
    if amt == 0:
        return ""
    if grp in ("Brokerage and private shares", "Retirement accounts", "Crypto"):
        return "Purchases of investments" if amt < 0 else "Proceeds from sales of investments"
    if grp == "Land":
        return "Land purchase payments" if amt < 0 else "Proceeds from land sales"
    if grp == "Construction in progress":
        return "Construction in progress"
    if grp == "Home (at cost)":
        return "Home improvements"
    if grp == "Vehicles":
        return "Vehicles bought" if amt < 0 else "Vehicles sold"
    return grp


# ---------------------------------------------------------------- tree

class Node:
    """label (HTML), tx [(date, text, cents)], kids, note (HTML, optional). cols: cents per window."""
    __slots__ = ("label", "tx", "kids", "note", "fixed")

    def __init__(self, label, tx=None, kids=None, note="", fixed=None):
        self.label, self.tx, self.kids, self.note, self.fixed = label, tx or [], kids or [], note, fixed

    def all_tx(self):
        yield from self.tx
        for k in self.kids:
            yield from k.all_tx()

    def cols(self, wins):
        if self.fixed is not None:
            return list(self.fixed)
        out = [0] * len(wins)
        for d, _, c in self.all_tx():
            for i, (lo, hi) in enumerate(wins):
                if (lo is None or d >= lo) and (hi is None or d <= hi):
                    out[i] += c
        return out


def _trie(items, esc):
    """items: [(account, tx)] -> list of Nodes nested by account hierarchy (common prefix stripped,
    single-child chains joined with ' › ')."""
    paths = [(a.split(":"), tx) for a, tx in items]
    if not paths:
        return []
    k = 0
    while all(len(p) > k + 1 for p, _ in paths) and len({p[k] for p, _ in paths}) == 1:
        k += 1
    if len(paths) == 1:
        k = max(0, len(paths[0][0]) - 2)  # one account: its last two levels

    def build(ps, depth):
        groups = {}
        own = []
        for p, tx in ps:
            if len(p) == depth:
                own += tx
            else:
                groups.setdefault(p[depth], []).append((p, tx))
        nodes = []
        for seg in sorted(groups):
            chain, cur = [seg], groups[seg]
            d = depth + 1
            while True:
                nxt = {p[d] for p, _ in cur if len(p) > d}
                if len(nxt) == 1 and all(len(p) > d for p, _ in cur):
                    chain.append(next(iter(nxt)))
                    d += 1
                else:
                    break
            label = esc(" › ".join(humanize(c) for c in chain))
            nodes.append(_mk(label, cur, d, build))
        if own:
            nodes.insert(0, Node(esc("(this account)"), own))
        return nodes
    return build(paths, k)


def _mk(label, cur, d, build):
    kids = build(cur, d)
    if len(kids) == 1 and not kids[0].kids:
        n = Node(label, kids[0].tx)  # leaf chain
        return n
    return Node(label, [], kids)


def fmt_cents(c):
    s = f"{abs(c) / 100:,.2f}"
    return f"({s})" if c < 0 else s


def _fmt0(x):
    n = round(x)
    s = f"${abs(n):,}"
    return f"({s})" if n < 0 else s


def _mdy(d):
    return f"{d[5:7]}/{d[8:10]}/{d[2:4]}"


# ---------------------------------------------------------------- the ledger

class Detail:
    def __init__(self, db_path, as_of, BS, IS, CF, labels=None):
        self.as_of = as_of
        self.BS, self.IS, self.CF = BS, IS, CF
        self.esc = lambda s: html.escape(s, quote=False)
        self.problems = []
        db = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
        cur = db.execute(
            "SELECT entry_id, date, entry_type, COALESCE(description,''), COALESCE(counterparty,''), account,"
            " account_type, COALESCE(cash_flow_class,''), statement, COALESCE(report_line,''), amount_minor"
            " FROM v_ledger WHERE date <= ? ORDER BY entry_id, line_id", (as_of,))
        self.lines = cur.fetchall()
        db.close()
        # per-entry text; equity (opening) legs name the account they opened
        self.entries = []
        i = 0
        L = self.lines
        while i < len(L):
            j = i
            while j < len(L) and L[j][0] == L[i][0]:
                j += 1
            self.entries.append(L[i:j])
            i = j
        self.bs_cols = [(None, BS["columns"][0]["to"]), (None, BS["columns"][-1]["to"])]
        iy = next(i for i, c in enumerate(IS["columns"]) if c["key"] == "ytd")
        it = next(i for i, c in enumerate(IS["columns"]) if c["key"] == "ttm")
        self.iy, self.it = iy, it
        self.pl_cols = [(IS["columns"][iy]["from"], IS["columns"][iy]["to"]),
                        (IS["columns"][it]["from"], IS["columns"][it]["to"])]
        self.lo = {"bs": None, "pl": self.pl_cols[1][0]}
        self._index()

    # text of one ledger line
    def text(self, e, l):
        desc, cp = scrub(l[3]).strip(), scrub(l[4]).strip()
        t = desc
        if cp and cp.lower() not in desc.lower():
            t = f"{cp} · {desc}" if desc else cp
        if l[5].startswith("Equity:"):
            others = [x[5] for x in e if not x[5].startswith("Equity:")]
            if others:
                t += " · " + human_join(others[0].split(":")[-1:])
        t = t.strip() or "(no description)"
        return t if len(t) <= 90 else t[:89] + "…"

    def _index(self):
        """Assign every line to statement targets."""
        BSk, ISk = {}, {}
        self.bs_tx = {}   # key -> [(date, text, cents natural)]
        self.bs_accts = {}  # key -> {account: [tx]}
        self.is_tx = {}   # key -> {account: [tx]}
        self.is_group = {}  # key -> (section, group)
        self.is_ni = {}   # (section, group) -> {date-bucket} for breakdowns: list of (date, cents income-positive)
        self.bs_ni = []   # (date, section, group, cents net-income contribution)
        self.cf = {}      # (kind, key) -> {account: [tx]}
        # ---- balance sheet / income statement
        for e in self.entries:
            for l in e:
                date, account, atype, cf, stmt, rline, amt = l[1], l[5], l[6], l[7], l[8], l[9], l[10]
                if stmt == "income_statement":
                    sec, g = classify_is(account, atype, rline)
                    v = amt if atype != "income" else -amt
                    if sec == "Non-operating" and atype == "expense":
                        v = -v
                    self.is_tx.setdefault(account, {}).setdefault(account, []).append((date, self.text(e, l), v))
                    self.is_group[account] = (sec, g)
                    self.bs_ni.append((date, sec, g, -amt))
                else:
                    sec, g, key, item = classify_bs(account, atype, rline, cf)
                    v = amt if atype == "asset" else -amt
                    self.bs_accts.setdefault(key, {}).setdefault(account, []).append((date, self.text(e, l), v))
        self._cashflow()

    def _cashflow(self):
        lo = self.pl_cols[1][0]
        hi = self.pl_cols[1][1]
        meta = []
        for e in self.entries:
            has_cash = other = invfin = False
            first_inv = first_fin = ""
            for l in e:
                if l[8] == "income_statement":
                    continue
                cfc = l[7]
                if cfc == "cash":
                    has_cash = True
                elif cfc == "investing":
                    invfin = True
                    if not first_inv:
                        first_inv = classify_bs(l[5], l[6], l[9], cfc)[1]
                elif cfc == "financing":
                    invfin = True
                    if not first_fin:
                        first_fin = classify_bs(l[5], l[6], l[9], cfc)[1]
                elif linkable(l[5]):
                    pass
                else:
                    other = True
            non_cash = (not has_cash) and (not other) and invfin and e[0][2] != "opening"
            meta.append((e, non_cash, first_inv, first_fin))
        linked = {}
        for e, nc, fi, ff in meta:
            if not nc:
                continue
            for l in e:
                if l[8] != "income_statement" and linkable(l[5]) and l[5] not in linked:
                    linked[l[5]] = ("investing", fi) if fi else ("financing", ff)

        def add(kind, key, e, l, c):
            self.cf.setdefault((kind, key), {}).setdefault(l[5], []).append((l[1], self.text(e, l), c))
        for e, nc, fi, ff in meta:
            date, etype = e[0][1], e[0][2]
            if date < lo or date > hi or etype == "opening":
                continue
            for l in e:
                if l[8] == "income_statement" and l[7] == "investing":
                    add("adj", "gain", e, l, l[10])
            if nc:
                for l in e:
                    if l[8] == "income_statement":
                        if l[7] != "investing":
                            add("adj", "noncash", e, l, l[10])
                    elif l[10] < 0 and (l[7] == "financing" or linkable(l[5])) and fi:
                        what = "new " + classify_bs(l[5], l[6], l[9], l[7])[1].lower()
                        if linkable(l[5]):
                            what = "amounts owed (paid later or still owed)"
                        add("memo", f"{fi}|{what}", e, l, -l[10])
                continue
            for l in e:
                if l[7] == "cash" and l[8] != "income_statement":
                    continue
                if l[8] == "income_statement":
                    if l[7] == "investing":
                        k = investing_line(fi or "Brokerage and private shares", -l[10])
                        if k:
                            add("inv", k, e, l, -l[10])
                    continue
                grp = classify_bs(l[5], l[6], l[9], l[7])[1]
                cls = l[7]
                if l[5] in linked:
                    cls, grp = linked[l[5]]
                if cls == "investing":
                    k = investing_line(grp, -l[10])
                    if k:
                        add("inv", k, e, l, -l[10])
                elif cls == "financing":
                    add("fin", grp, e, l, -l[10])
                elif cls in ("operating", "unclassified"):
                    add("wc", grp, e, l, -l[10])
                else:
                    add("fin", "Other equity", e, l, -l[10])

    # ------------------------------------------------------------ node builders

    def _leafs(self, accts, root=None):
        """{account: tx} -> nodes nested by hierarchy below `root` (an account prefix)."""
        items = []
        for a, tx in accts.items():
            rel = a[len(root) + 1:] if root and a.startswith(root + ":") else ("" if a == root else a)
            items.append((rel, sorted(tx, key=lambda t: t[0], reverse=True)))
        own = [t for r, t in items if r == ""]
        sub = [(r, t) for r, t in items if r != ""]
        kids = _trie(sub, self.esc)
        return [t for o in own for t in o], kids

    def _item(self, label, accts, root, wins=None):
        tx, kids = self._leafs(accts, root)
        n = Node(label, tx, kids)
        return n

    def _nonzero(self, n, wins):
        return any(n.cols(wins))

    def bs_group(self, groups):
        """Balance sheet: list of Nodes (one per group when several, else its accounts)."""
        out = []
        for g in groups:
            items = []
            for r in self.BS["rows"]:
                if r["kind"] == "item" and r.get("group") == g and r.get("key") in self.bs_accts:
                    n = self._item(self.esc(r["label"]), self.bs_accts[r["key"]], r["key"], self.bs_cols)
                    if self._nonzero(n, self.bs_cols):
                        items.append(n)
            if len(groups) > 1:
                if items:
                    out.append(Node(self.esc(g), kids=items))
            else:
                out += items
        return out

    def equity_opening(self):
        tx = [t for v in self.bs_accts.get("opening-equity", {}).values() for t in v]
        return Node("", tx)

    def ni_breakdown(self, mode):
        """Net income by P&L group (no transactions: they are listed on the Profit and Loss Statement)."""
        wins = self.bs_cols if mode == "bs" else self.pl_cols
        order = ["Income", "Living expenses", "Non-operating", "Taxes"]
        agg = {}
        for d, sec, g, c in self.bs_ni:
            if mode != "bs" and d < self.pl_cols[1][0]:
                continue
            agg.setdefault((sec, g), []).append((d, "", c))
        kids = []
        for sec in order:
            gs = sorted(k for k in agg if k[0] == sec)
            if not gs:
                continue
            sub = [Node(self.esc(g), fixed=Node("", agg[(s, g)]).cols(wins)) for s, g in gs]
            tot = [sum(k.fixed[i] for k in sub) for i in range(2)]
            if any(tot):
                kids.append(Node(self.esc(sec), kids=sub, fixed=tot))
        return Node("", kids=kids, note="Transactions for these lines are under the Profit and Loss Statement.",
                    fixed=[sum(k.fixed[i] for k in kids) for i in range(2)])

    def pl_items(self, groups, keys=None, sign=1):
        """Profit and Loss: nodes (one per group when several) for the account items of `groups`;
        `keys` restricts to some item keys. sign flips amounts for lines shown the other way round."""
        out = []
        for g in groups:
            items = []
            for r in self.IS["rows"]:
                if r["kind"] != "item" or r.get("group") != g or r.get("key") not in self.is_tx:
                    continue
                if keys is not None and r["key"] not in keys:
                    continue
                accts = {a: [(d, t, c * sign) for d, t, c in tx] for a, tx in self.is_tx[r["key"]].items()}
                n = self._item(self.esc(r["label"]), accts, r["key"], self.pl_cols)
                if any(n.cols(self.pl_cols)):
                    items.append(n)
            if len(groups) > 1:
                if items:
                    out.append(Node(self.esc(g), kids=items))
            else:
                out += items
        return out

    def cf_target(self, kind, key, label=None, sign=1):
        accts = self.cf.get((kind, key))
        if not accts:
            return None
        if sign != 1:
            accts = {a: [(d, t, c * sign) for d, t, c in tx] for a, tx in accts.items()}
        items = [(a, sorted(tx, key=lambda t: t[0], reverse=True)) for a, tx in accts.items()]
        kids = _trie(items, self.esc)
        if len(kids) == 1 and not kids[0].kids:
            kids[0].label = label if label else kids[0].label
            return kids[0]
        return Node(label or "", [], kids)

    # ------------------------------------------------------------ rendering

    def render_tx(self, tx, dim_le):
        rows = []
        for d, t, c in sorted(tx, key=lambda x: x[0], reverse=True):
            cls = " class=p" if dim_le and d <= dim_le else ""
            neg = " class=n" if c < 0 else ""
            rows.append(f'<p{cls}><i>{_mdy(d)}</i><span>{self.esc(t)}</span><b{neg}>{fmt_cents(c)}</b></p>')
        return "".join(rows)

    def render(self, n, label_html, wins, dim_le, vals=None, tag="x"):
        """One expanding row. The summary shows the label and two numbers: `vals` (dollars, the
        statement's own figures) at the top level, computed from the transactions below it."""
        d = list(vals) if vals is not None else [x / 100 for x in n.cols(wins)]
        nums = "".join(f'<b{" class=n" if round(x) < 0 else ""}>{_fmt0(x)}</b>' for x in d)
        body = "".join(self.render(k, k.label, wins, dim_le) for k in n.kids)
        if n.tx:
            body += self.render_tx(n.tx, dim_le)
        note = f'<p class="nt">{n.note}</p>' if n.note else ""
        return f'<details class="{tag}"><summary><span>{label_html}</span>{nums}</summary><div class="d">{note}{body}</div></details>'

    def verify(self, what, nodes, wins, expect):
        """The nodes' computed column totals (cents) must equal the statement's figures (dollars)."""
        got = [0] * len(wins)
        for n in nodes:
            for i, c in enumerate(n.cols(wins)):
                got[i] += c
        for g, x in zip(got, expect):
            if abs(g - round(x * 100)) > 1:
                self.problems.append(f"{what}: detail {g / 100:,.2f} vs statement {x:,.2f}")
