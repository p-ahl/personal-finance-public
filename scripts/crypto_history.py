#!/usr/bin/env python3
"""Self-custody crypto history: lots, value over time vs cost basis, realized sales, and a
retrospective "best time to sell" per asset. Every figure is a query over the ledger tables
(v_ledger, crypto_valuations, crypto_price_history, wallet_events); nothing is typed in.

  python3 scripts/crypto_history.py [--db ../finance-data/finance.db] [--as-of 2026-10-17]
      [--out ../finance-data/exports/crypto-2026-10-17] [--exclude-note TEXT]

Outputs (no wallet addresses): crypto.html (one self-contained phone-width page, inline SVG, follows
prefers-color-scheme), crypto_summary.csv, crypto_lots.csv, crypto_daily.csv, crypto_realized.csv,
crypto_best_time_to_sell.csv. Re-running rewrites the same files.

Method
  * Books period (from books_start): quantity, price, value and cost basis per day come from
    crypto_valuations (the nightly build; quantity reconciled to on-chain balances, price = the
    pull's Kraken ticker, else the Kraken daily close in crypto_price_history). Cost basis is the
    ledger cost, mark-to-market adjustments excluded.
  * Before books_start: quantity and an average-cost estimate are replayed from wallet_events
    (swaps, exchange withdrawals, unmatched items) and valued at the daily close. Estimate only.
  * Pass-through money (someone else's purchase that crossed the wallets) is never booked, so it is
    not in any figure here.
  * "Best time to sell" is hindsight: the day your holding was worth the most, and what selling then
    would have netted (value minus cost basis) versus holding to today. It ignores fees, taxes,
    slippage and the fact that nobody can sell at the exact peak.
"""
import argparse
import csv
import html
import os
import sqlite3
from collections import defaultdict
from datetime import date, timedelta

HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.path.normpath(os.path.join(HERE, "..", "..", "finance-data"))
SCALE = 1e10
SELF = "Assets:Crypto:SelfCustody"
STABLE = {"USDC", "USDT", "PYUSD"}  # pass-through cash equivalents: kept in the CSVs, left out of the asset cards


def usd(x, dec=0):
    s = f"${abs(x):,.{dec}f}"
    return f"-{s}" if x < 0 else s


def connect(db):
    c = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    c.row_factory = sqlite3.Row
    return c


def load(db, as_of):
    c = connect(db)
    D = {}
    D["books_start"] = c.execute("SELECT MIN(date) FROM v_ledger WHERE account LIKE ?", (SELF + "%",)).fetchone()[0]
    # daily books-period series per coin (all self-custody wallets together)
    daily = defaultdict(dict)  # coin -> date -> [qty, price, value, cost]
    for r in c.execute("""SELECT val_date, commodity, SUM(CAST(quantity AS REAL)) q, MAX(CAST(price_usd AS REAL)) p,
                          SUM(value_cents) v, SUM(cost_cents) k FROM crypto_valuations WHERE val_date <= ?
                          GROUP BY 1, 2 ORDER BY 1""", (as_of,)):
        daily[r["commodity"]][r["val_date"]] = [r["q"], r["p"], r["v"] / 100, r["k"] / 100]
    D["daily"] = daily
    hist = defaultdict(dict)
    for r in c.execute("SELECT commodity, price_date, price FROM crypto_price_history WHERE price_date <= ?", (as_of,)):
        hist[r["commodity"]][r["price_date"]] = float(r["price"])
    D["hist"] = hist
    # pre-books replay from wallet_events (estimate)
    pre = defaultdict(list)  # coin -> [(date, qty_after, cost_after)]
    q = defaultdict(float)
    k = defaultdict(float)
    first_seen = {}
    cut = D["books_start"] or as_of
    for r in c.execute("""SELECT date, kind, coin_out, qty_out, fee_out, usd_out, coin_in, qty_in, usd_in FROM wallet_events
                          WHERE date <= ? ORDER BY time, id""", (cut,)):
        if r["kind"] == "passthrough":
            continue
        touched = set()

        def f(x):
            try:
                return float(x or 0)
            except ValueError:
                return 0.0
        if r["coin_out"] and f(r["qty_out"]):
            co, qo = r["coin_out"], f(r["qty_out"]) + f(r["fee_out"])
            if q[co] > 0:
                k[co] -= k[co] * min(1.0, qo / q[co])
            q[co] -= qo
            touched.add(co)
        if r["coin_in"] and f(r["qty_in"]):
            ci = r["coin_in"]
            q[ci] += f(r["qty_in"])
            k[ci] += f(r["usd_in"]) or f(r["usd_out"])
            first_seen.setdefault(ci, r["date"])
            touched.add(ci)
        for co in touched:
            pre[co].append((r["date"], q[co], k[co]))
    D["pre"], D["first_seen"] = pre, first_seen
    # lots: positive-quantity self-custody lines that are not moves between own wallets
    lots = []
    for r in c.execute("""SELECT date, entry_type, description, account, commodity, quantity, amount_minor FROM v_ledger
                          WHERE account LIKE ? AND commodity <> '' AND quantity > 0 AND entry_type NOT IN ('transfer','valuation')
                          AND date <= ? ORDER BY date, line_id""", (SELF + "%", as_of)):
        d = r["description"]
        kind = ("opening holdings (cost replayed)" if r["entry_type"] == "opening" else
                "swap in" if d.startswith("Swap") else
                "exchange withdrawal" if "withdrawal" in d else
                "on-chain true-up" if r["entry_type"] == "adjustment" else
                "receipt (market value)" if ("receipt" in d or "Unmatched" in d) else
                "round trip back" if "round trip" in d else "other")
        lots.append({"date": r["date"], "asset": r["commodity"], "wallet_account": r["account"].replace(SELF + ":", ""),
                     "qty": r["quantity"] / SCALE, "cost": r["amount_minor"] / 100, "kind": kind})
    D["lots"] = lots
    # pre-books lots (estimates)
    plots = []
    for r in c.execute("""SELECT date, kind, coin_in, qty_in, usd_in, usd_out FROM wallet_events WHERE date <= ? AND coin_in <> ''
                          AND kind IN ('exchange_out','swap','unmatched_in') ORDER BY time""", (cut,)):
        try:
            plots.append({"date": r["date"], "asset": r["coin_in"], "wallet_account": "(before books)", "qty": float(r["qty_in"] or 0),
                          "cost": float(r["usd_in"] or 0) or float(r["usd_out"] or 0),
                          "kind": {"exchange_out": "exchange withdrawal (est.)", "swap": "swap in (est.)", "unmatched_in": "receipt (est.)"}[r["kind"]]})
        except ValueError:
            pass
    D["prelots"] = plots
    # realized disposals of crypto: entries with a realized-gain line and a crypto leg out
    rows = c.execute("""SELECT entry_id, date, description, account, commodity, quantity, amount_minor FROM v_ledger
                        WHERE entry_id IN (SELECT entry_id FROM v_ledger WHERE account LIKE 'Income:Investments:Realized-Gains%')
                        AND date <= ? ORDER BY entry_id, line_id""", (as_of,)).fetchall()
    by = defaultdict(list)
    for r in rows:
        by[r["entry_id"]].append(r)
    sales = []
    for eid, ls in by.items():
        legs = [l for l in ls if l["account"].startswith("Assets:Crypto:") and l["commodity"] and l["quantity"] is not None and l["quantity"] < 0]
        if not legs:
            continue
        leg = max(legs, key=lambda l: abs(l["amount_minor"]))
        gain = -sum(l["amount_minor"] for l in ls if l["account"].startswith("Income:Investments:Realized-Gains")) / 100
        cost = -leg["amount_minor"] / 100
        qty = -leg["quantity"] / SCALE
        where = "self-custody" if leg["account"].startswith(SELF) else leg["account"].split(":")[-1]
        sales.append({"date": leg["date"], "asset": leg["commodity"], "venue": where, "qty": qty, "cost": cost,
                      "proceeds": cost + gain, "gain": gain, "price": (cost + gain) / qty if qty else 0,
                      "what": "sold for cash" if " -> USD" in leg["description"] else "swapped"})
    D["sales"] = sorted(sales, key=lambda s: (s["date"], s["asset"]))
    D["dec"] = {}
    D["mark_total"] = c.execute("SELECT COALESCE(SUM(amount_minor),0) FROM v_ledger WHERE account='Income:Investments:Unrealized-Gains' AND date<=?", (as_of,)).fetchone()[0] / 100
    c.close()
    return D


def series_for(D, coin, as_of):
    """Daily [date, qty, price, value, cost, estimated] from the first holding to as_of."""
    out = []
    dly = D["daily"].get(coin, {})
    hist = D["hist"].get(coin, {})
    cut = D["books_start"]
    pre = D["pre"].get(coin, [])
    if pre and cut:
        d0 = date.fromisoformat(pre[0][0])
        d1 = date.fromisoformat(cut) - timedelta(days=1)
        i, qc, kc, last_p = 0, 0.0, 0.0, None
        d = d0
        while d <= d1:
            ds = d.isoformat()
            while i < len(pre) and pre[i][0] <= ds:
                qc, kc = pre[i][1], pre[i][2]
                i += 1
            p = hist.get(ds, last_p)
            last_p = p if p is not None else last_p
            if p is not None and qc > 1e-9:
                out.append([ds, qc, p, qc * p, kc, True])
            d += timedelta(days=1)
    for ds in sorted(dly):
        q, p, v, k = dly[ds]
        out.append([ds, q, p, v, k, False])
    return out


def analyse(D, as_of):
    coins = sorted(set(D["daily"]) | set(D["pre"]))
    res = {}
    for c in coins:
        s = series_for(D, c, as_of)
        if not s:
            continue
        now = s[-1] if s[-1][0] == as_of else None
        held_now = now is not None and now[3] >= 1.0  # dust (under $1) counts as not held
        peak = max(s, key=lambda r: r[3])
        peakp = max(s, key=lambda r: r[2])
        res[c] = {"series": s, "now": now, "held": held_now, "peak": peak, "peak_price": peakp,
                  "first": s[0][0], "first_held_books": next((r[0] for r in s if not r[5]), None)}
    return res


def svg_chart(series, w=420, h=170, dark_ok=True):
    """Value (solid) vs cumulative cost basis (dashed); pre-books estimate shaded lighter."""
    pts = [r for r in series]
    if len(pts) < 2:
        return ""
    x0 = date.fromisoformat(pts[0][0]).toordinal()
    x1 = date.fromisoformat(pts[-1][0]).toordinal()
    ymax = max(max(r[3], r[4]) for r in pts) * 1.08 or 1
    L, R, T, B = 46, 8, 8, 20

    def X(ds):
        return L + (date.fromisoformat(ds).toordinal() - x0) / max(1, x1 - x0) * (w - L - R)

    def Y(v):
        return T + (1 - v / ymax) * (h - T - B)
    val = " ".join(f"{X(r[0]):.1f},{Y(r[3]):.1f}" for r in pts)
    cost = " ".join(f"{X(r[0]):.1f},{Y(r[4]):.1f}" for r in pts)
    g = []
    for i in range(5):
        v = ymax / 1.08 * i / 4
        g.append(f'<line x1="{L}" x2="{w - R}" y1="{Y(v):.1f}" y2="{Y(v):.1f}" class="grid"/><text x="{L - 4}" y="{Y(v) + 3:.1f}" class="ax" text-anchor="end">{usd(v)}</text>')
    pk = max(pts, key=lambda r: r[3])
    est_end = [r for r in pts if r[5]]
    shade = ""
    if est_end:
        shade = f'<rect x="{L}" y="{T}" width="{X(est_end[-1][0]) - L:.1f}" height="{h - T - B}" class="est"/>'
    xl = f'<text x="{L}" y="{h - 5}" class="ax">{pts[0][0][:7]}</text><text x="{w - R}" y="{h - 5}" class="ax" text-anchor="end">{pts[-1][0][:7]}</text>'
    return (f'<svg viewBox="0 0 {w} {h}" role="img" aria-label="Value over time versus cost basis">{shade}{"".join(g)}'
            f'<polyline points="{cost}" class="cost"/><polyline points="{val}" class="val"/>'
            f'<circle cx="{X(pk[0]):.1f}" cy="{Y(pk[3]):.1f}" r="3.5" class="pk"/>{xl}</svg>')


CSS = """
:root{--bg:#fff;--fg:#1b1f24;--mut:#5b6573;--card:#f4f6f8;--line:#d7dce2;--val:#2f8f5b;--cost:#c9772c;--pk:#c0392b;--est:rgba(120,120,120,.10)}
@media (prefers-color-scheme:dark){:root{--bg:#111418;--fg:#e8ebef;--mut:#9aa5b3;--card:#1b2027;--line:#2d343d;--val:#52c48a;--cost:#e8a35c;--pk:#ff6b5b;--est:rgba(200,200,200,.08)}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.45 -apple-system,system-ui,Segoe UI,Roboto,sans-serif}
main{max-width:480px;margin:0 auto;padding:14px}h1{font-size:20px;margin:4px 0}h2{font-size:17px;margin:0 0 6px}h3{font-size:14px;margin:12px 0 4px;color:var(--mut)}
section{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:12px;margin:12px 0}
.sub{color:var(--mut);font-size:13px}table{width:100%;border-collapse:collapse;font-size:13px;font-variant-numeric:tabular-nums}
td,th{padding:3px 4px;border-bottom:1px solid var(--line);text-align:right}th{color:var(--mut);font-weight:600}td:first-child,th:first-child{text-align:left}
.gain{color:var(--val)}.loss{color:var(--pk)}svg{width:100%;height:auto}.grid{stroke:var(--line);stroke-width:1}.ax{fill:var(--mut);font-size:9px}
.val{fill:none;stroke:var(--val);stroke-width:2}.cost{fill:none;stroke:var(--cost);stroke-width:1.5;stroke-dasharray:4 3}.pk{fill:var(--pk)}.est{fill:var(--est)}
.kv{display:grid;grid-template-columns:1fr auto;gap:2px 8px;font-size:14px}.kv b{text-align:right}details summary{cursor:pointer;color:var(--mut)}
ul{padding-left:18px;margin:6px 0}.legend span{margin-right:10px;font-size:12px;color:var(--mut)}
"""


def cls(x):
    return "gain" if x >= 0 else "loss"


def build(D, A, as_of, out, note):
    os.makedirs(out, exist_ok=True)
    cur_px = {c: (v["now"][2] if v["now"] else None) for c, v in A.items()}
    # --- CSVs
    with open(os.path.join(out, "crypto_summary.csv"), "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["asset", "quantity", "price_usd", "value_usd", "cost_basis_usd", "unrealized_gain_usd", "first_date", "peak_value_date", "peak_value_usd"])
        for c, v in A.items():
            n = v["now"]
            if n and v["held"]:
                w.writerow([c, f"{n[1]:.8f}", f"{n[2]:.6f}", f"{n[3]:.2f}", f"{n[4]:.2f}", f"{n[3] - n[4]:.2f}", v["first"], v["peak"][0], f"{v['peak'][3]:.2f}"])
    with open(os.path.join(out, "crypto_lots.csv"), "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["asset", "date", "quantity", "cost_usd", "unit_cost_usd", "source", "wallet_account", "estimate"])
        for l in sorted(D["prelots"] + D["lots"], key=lambda l: (l["asset"], l["date"])):
            w.writerow([l["asset"], l["date"], f"{l['qty']:.8f}", f"{l['cost']:.2f}", f"{l['cost'] / l['qty']:.6f}" if l["qty"] else "", l["kind"], l["wallet_account"],
                        "yes" if l["wallet_account"] == "(before books)" else "no"])
    with open(os.path.join(out, "crypto_daily.csv"), "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["date", "asset", "quantity", "price_usd", "value_usd", "cost_basis_usd", "estimate_before_books"])
        for c, v in A.items():
            for r in v["series"]:
                w.writerow([r[0], c, f"{r[1]:.8f}", f"{r[2]:.6f}", f"{r[3]:.2f}", f"{r[4]:.2f}", "yes" if r[5] else "no"])
    # sales vs holding
    for s in D["sales"]:
        px_now = cur_px.get(s["asset"]) or (sorted(D["hist"].get(s["asset"], {}).items())[-1][1] if D["hist"].get(s["asset"]) else None)
        s["now_value"] = s["qty"] * px_now if px_now else None
        after = [(d, p) for d, p in D["hist"].get(s["asset"], {}).items() if d > s["date"]]
        pk = max(after, key=lambda x: x[1]) if after else None
        s["peak_after"] = (pk[0], s["qty"] * pk[1]) if pk else None
    with open(os.path.join(out, "crypto_realized.csv"), "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["date", "asset", "venue", "what", "quantity", "price_usd", "proceeds_usd", "cost_basis_usd", "realized_gain_usd", "value_today_if_held_usd",
                    "sold_better_than_holding_usd", "best_later_value_date", "best_later_value_usd"])
        for s in D["sales"]:
            nv = s["now_value"]
            w.writerow([s["date"], s["asset"], s["venue"], s["what"], f"{s['qty']:.6f}", f"{s['price']:.6f}", f"{s['proceeds']:.2f}", f"{s['cost']:.2f}", f"{s['gain']:.2f}",
                        f"{nv:.2f}" if nv is not None else "", f"{s['proceeds'] - nv:.2f}" if nv is not None else "",
                        s["peak_after"][0] if s["peak_after"] else "", f"{s['peak_after'][1]:.2f}" if s["peak_after"] else ""])
    best = []
    with open(os.path.join(out, "crypto_best_time_to_sell.csv"), "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["asset", "peak_value_date", "peak_value_usd", "cost_basis_at_peak_usd", "net_gain_if_sold_at_peak_usd", "value_now_usd", "cost_basis_now_usd", "net_gain_if_held_now_usd",
                    "peak_minus_now_usd", "peak_price_date", "peak_price_usd", "price_now_usd", "price_vs_peak_pct"])
        for c, v in A.items():
            pk, n = v["peak"], v["now"]
            nv, nk = (n[3], n[4]) if n and v["held"] else (0.0, 0.0)
            pp, cp = v["peak_price"], (n[2] if n else None)
            row = [c, pk[0], round(pk[3], 2), round(pk[4], 2), round(pk[3] - pk[4], 2), round(nv, 2), round(nk, 2), round(nv - nk, 2), round(pk[3] - nv, 2), pp[0], pp[2],
                   cp if cp is not None else "", round((cp / pp[2] - 1) * 100, 1) if cp else ""]
            w.writerow(row)
            best.append(row)
    # --- HTML
    h = [f'<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="color-scheme" content="light dark">'
         f'<title>Crypto history</title><style>{CSS}</style></head><body><main>',
         f'<h1>Crypto: value, cost and what-if-sold</h1><div class="sub">Self-custody wallets as of {as_of}. Wallet addresses are not shown.</div>']
    tv = sum(v["now"][3] for v in A.values() if v["now"] and v["held"])
    tk = sum(v["now"][4] for v in A.values() if v["now"] and v["held"])
    h.append(f'<section><h2>Total today</h2><div class="kv"><span>Market value</span><b>{usd(tv)}</b><span>Cost basis (what was paid)</span><b>{usd(tk)}</b>'
             f'<span>Unrealized gain / (loss)</span><b class="{cls(tv - tk)}">{usd(tv - tk)}</b></div>'
             f'<p class="sub">Cost basis is the purchase price including fees where known. Unrealized means not sold yet: it only becomes a real gain or loss when sold.</p></section>')
    for c, v in sorted(A.items(), key=lambda kv: -(kv[1]["now"][3] if kv[1]["now"] and kv[1]["held"] else 0)):
        if c in STABLE:
            continue
        n, pk = v["now"], v["peak"]
        heldtxt = n is not None and v["held"]
        h.append(f'<section><h2>{html.escape(c)}</h2>')
        if heldtxt:
            h.append(f'<div class="kv"><span>Quantity</span><b>{n[1]:,.4f}</b><span>Price now</span><b>{usd(n[2], 4 if n[2] < 1 else 2)}</b><span>Market value</span><b>{usd(n[3])}</b>'
                     f'<span>Cost basis</span><b>{usd(n[4])}</b><span>Unrealized gain / (loss)</span><b class="{cls(n[3] - n[4])}">{usd(n[3] - n[4])}</b></div>')
        else:
            h.append('<div class="sub">No longer held in self-custody (swapped or sent to an exchange).</div>')
        h.append(svg_chart(v["series"]))
        h.append('<div class="legend"><span style="color:var(--val)">&#9632; value</span><span style="color:var(--cost)">&#9644; cost basis</span><span style="color:var(--pk)">&#9679; peak</span>'
                 '<span>shaded = before books start (estimate)</span></div>')
        nv = n[3] if heldtxt else 0.0
        nk = n[4] if heldtxt else 0.0
        h.append(f'<h3>Best time to sell (hindsight)</h3><div class="kv"><span>Peak value day</span><b>{pk[0]}</b><span>Value that day</span><b>{usd(pk[3])}</b>'
                 f'<span>Cost basis that day</span><b>{usd(pk[4])}</b><span>Net gain if sold then</span><b class="{cls(pk[3] - pk[4])}">{usd(pk[3] - pk[4])}</b>'
                 f'<span>Net gain holding to today</span><b class="{cls(nv - nk)}">{usd(nv - nk) if heldtxt else "n/a"}</b>'
                 f'<span>Peak vs today</span><b>{usd(pk[3] - nv)} {"higher" if pk[3] >= nv else "lower"}</b><span>Peak price day</span><b>{v["peak_price"][0]} @ {usd(v["peak_price"][2], 4 if v["peak_price"][2] < 1 else 2)}</b></div>')
        lots = [l for l in sorted(D["prelots"] + D["lots"], key=lambda l: l["date"]) if l["asset"] == c]
        if lots:
            body = "".join(f'<tr><td>{l["date"]}{"*" if l["wallet_account"] == "(before books)" else ""}</td><td>{l["qty"]:,.2f}</td><td>{usd(l["cost"])}</td><td>{usd(l["cost"] / l["qty"], 4) if l["qty"] and l["cost"] / l["qty"] < 1 else usd(l["cost"] / l["qty"] if l["qty"] else 0, 2)}</td></tr>' for l in lots)
            h.append(f'<details><summary>Purchase / receipt lots ({len(lots)})</summary><table><tr><th>Date</th><th>Qty</th><th>Cost</th><th>Unit</th></tr>{body}</table>'
                     f'<div class="sub">* before books start; cost = market value on receipt (estimate). Swaps in are valued at the day price.</div></details>')
        sl = [s for s in D["sales"] if s["asset"] == c]
        if sl:
            body = "".join(f'<tr><td>{s["date"]}</td><td>{s["qty"]:,.0f}</td><td>{usd(s["price"], 4 if s["price"] < 1 else 2)}</td><td>{usd(s["proceeds"])}</td>'
                           f'<td class="{cls(s["gain"])}">{usd(s["gain"])}</td><td>{usd(s["now_value"]) if s["now_value"] is not None else ""}</td></tr>' for s in sl)
            h.append(f'<details><summary>Sales already made ({len(sl)})</summary><table><tr><th>Date</th><th>Qty</th><th>Price</th><th>Proceeds</th><th>Gain</th><th>Worth today</th></tr>{body}</table></details>')
        h.append("</section>")
    # Kaspa 2026 sales comparison
    ks = [s for s in D["sales"] if s["asset"] == "KAS" and s["date"].startswith("2026") and s["what"] == "sold for cash"]
    if ks:
        tp = sum(s["proceeds"] for s in ks)
        tn = sum(s["now_value"] for s in ks if s["now_value"] is not None)
        body = "".join(f'<tr><td>{s["date"]}</td><td>{s["qty"]:,.0f}</td><td>{usd(s["price"], 4)}</td><td>{usd(s["proceeds"])}</td><td>{usd(s["now_value"])}</td>'
                       f'<td class="{cls(s["proceeds"] - s["now_value"])}">{usd(s["proceeds"] - s["now_value"])}</td></tr>' for s in ks)
        h.append(f'<section><h2>Kaspa sales in 2026: sold vs held</h2><table><tr><th>Date</th><th>Qty</th><th>Sold at</th><th>Proceeds</th><th>Worth today</th><th>Sold better by</th></tr>{body}'
                 f'<tr><th>Total</th><th></th><th></th><th>{usd(tp)}</th><th>{usd(tn)}</th><th class="{cls(tp - tn)}">{usd(tp - tn)}</th></tr></table>'
                 f'<p class="sub">Positive = selling beat holding to today. Proceeds are net of the cost basis relieved plus gain; fees are expensed separately.</p></section>')
    h.append('<section><h2>Notes in plain English</h2><ul>'
             '<li><b>Value</b> = quantity x market price. <b>Cost basis</b> = what you paid (FIFO lots). <b>Unrealized</b> = value minus cost for coins you still hold.</li>'
             '<li>Month-end and nightly marks book the change in value to Unrealized Gains in the period it happened, so each month&rsquo;s Profit and Loss reflects that month&rsquo;s market move only.</li>'
             '<li>Prices: the nightly Kraken ticker; earlier days use the Kraken daily close (public API), cross-checked against CoinGecko. Quantities match the on-chain balances.</li>'
             f'<li>{html.escape(note)}</li>'
             '<li>&ldquo;Best time to sell&rdquo; is hindsight only: it ignores fees, taxes and the price impact of selling, and no one can sell at the exact peak.</li></ul></section>')
    h.append("</main></body></html>")
    with open(os.path.join(out, "crypto.html"), "w") as f:
        f.write("\n".join(h))
    return {"total_value": tv, "total_cost": tk}


def weekly_rows(db, as_of, prior):
    """Per asset value now vs a week earlier (for the weekly report): [(asset, qty_now, value_now, value_prior, price_chg_pct)]."""
    c = connect(db)
    out = []
    for r in c.execute("""SELECT commodity, SUM(CAST(quantity AS REAL)) q, SUM(value_cents) v, MAX(CAST(price_usd AS REAL)) p FROM crypto_valuations
                          WHERE val_date = ? GROUP BY 1 HAVING SUM(value_cents) >= 100 ORDER BY v DESC""", (as_of,)).fetchall():
        p = c.execute("""SELECT SUM(value_cents), MAX(CAST(price_usd AS REAL)), SUM(CAST(quantity AS REAL)) FROM crypto_valuations WHERE val_date = ? AND commodity = ?""", (prior, r["commodity"])).fetchone()
        pv = (p[0] or 0) / 100
        out.append({"asset": r["commodity"], "qty": r["q"], "value": r["v"] / 100, "prior_value": pv, "price": r["p"], "prior_price": p[1] or 0, "prior_qty": p[2] or 0})
    c.close()
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", default=os.path.join(DATA, "finance.db"))
    ap.add_argument("--as-of", default=date.today().isoformat())
    ap.add_argument("--out")
    ap.add_argument("--exclude-note", default="Money that only passed through the wallets for someone else is never booked, so it is not counted here.")
    a = ap.parse_args()
    out = a.out or os.path.join(DATA, "exports", f"crypto-{a.as_of}")
    D = load(a.db, a.as_of)
    A = analyse(D, a.as_of)
    r = build(D, A, a.as_of, out, a.exclude_note)
    print(f"crypto history -> {out}  (value {usd(r['total_value'])}, cost {usd(r['total_cost'])}, {len(A)} assets)")


if __name__ == "__main__":
    main()
