#!/usr/bin/env python3
"""Infer self-custody wallet events from on-chain history (detective work, estimates).

    scripts/wallet_events.py <wallet pull dir> [--db FINANCE_DB] [--config wallets.yaml]
                             [--timeline-md PATH] [--timeline-csv PATH]

Reads a `finance download wallets --history` pull (balances.json, history/, the Cake Wallet CSV,
daily OHLC), the exchange ledgers staged in the books (raw_exchange_ledger: Kraken API rows and
Coinbase events) and the local wallets.yaml (labels, pass-through windows). Writes
<pull>/events.csv, the input `finance import wallets` books from:

  exchange_out   exchange withdrawal received by a wallet (ref = <exchange>:<refid>, or
                 coinbase-prebooks for Coinbase sends before the books: market value on receipt)
  exchange_in    wallet send credited by an exchange (deposit; ref = <exchange>:<refid>)
  transfer       between own wallets (same transaction on both sides)
  swap           coin X out of a wallet, coin Y into a wallet shortly after, USD values close
                 (swap service / in-wallet exchange); valued at the daily VWAP of each coin
  roundtrip_out / roundtrip_in   sent to an outside address and back from that same address
                 within 90 days (ref pairs them): cost basis carried, not a disposal
  unmatched_in / unmatched_out   anything else above the dust threshold (listed; left to the
                 at-cost adjustment)
  passthrough    inside a configured pass-through window (excluded from the books)

Matching is heuristic (timing + amounts); confidence high/medium/low is recorded per row.
Addresses are never printed; evidence carries truncated transaction ids only. No personal data
lives in this script.
"""
import argparse, csv, datetime as dt, glob, json, os, sqlite3, sys
from decimal import Decimal as D, ROUND_HALF_UP

UTC = dt.timezone.utc
DEN = dt.timezone(dt.timedelta(hours=-7))  # display only; dates for booking use America/Denver below
try:
    from zoneinfo import ZoneInfo
    DEN = ZoneInfo("America/Denver")
except Exception:
    pass
STABLE = {"USDC", "USDT", "PYUSD"}
DUST_USD = D("1.00")
MINTS = {"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": "USDC", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": "USDT",
         "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo": "PYUSD"}


def load_yaml(path):
    try:
        import yaml
    except ImportError:
        sys.exit("wallet_events: needs PyYAML (pip install pyyaml)")
    with open(path) as f:
        return yaml.safe_load(f) or {}


def short(x, n=10):
    return (x or "")[:n]


class Prices:
    def __init__(self, pull):
        self.d = {}
        for f in glob.glob(os.path.join(pull, "history", "ohlc_*.json")):
            coin = os.path.basename(f)[5:-5]
            res = json.load(open(f)).get("result", {})
            for k, rows in res.items():
                if k == "last":
                    continue
                for r in rows:  # [time, open, high, low, close, vwap, volume, count]
                    day = dt.datetime.fromtimestamp(r[0], UTC).date()
                    v = D(r[5]) if D(r[5]) > 0 else D(r[4])
                    self.d.setdefault(coin, {})[day] = v

    def usd(self, coin, t):
        if coin in STABLE:
            return D(1)
        days = self.d.get(coin)
        if not days:
            return None
        day = t.astimezone(UTC).date()
        for i in range(8):
            p = days.get(day - dt.timedelta(days=i))
            if p is not None:
                return p
        return None


def movements(pull, bal):
    own = {}
    for w in bal["wallets"]:
        for i in w["identifiers"]:
            if i["kind"] in ("address", "derived"):
                own[i["value"]] = (w["wallet"], i["chain"])
    mv = []
    hist = os.path.join(pull, "history")
    # kaspa: net per wallet per transaction (change between a wallet's own addresses nets out)
    wallets_kas = sorted({w for (w, c) in own.values() if c == "kaspa"})
    for wallet in wallets_kas:
        txs = {}
        for f in sorted(glob.glob(os.path.join(hist, f"kaspa_{wallet}_kaspa_*_page*.json"))):
            for t in json.load(open(f)):
                txs[t["transaction_id"]] = t
        for tid, t in txs.items():
            if not t.get("is_accepted"):
                continue
            mine = lambda a: own.get(a, ("", ""))[0] == wallet
            ins = sum(i["previous_outpoint_amount"] or 0 for i in t["inputs"] if mine(i["previous_outpoint_address"]))
            outs = sum(o["amount"] for o in t["outputs"] if mine(o["script_public_key_address"]))
            totin = sum(i["previous_outpoint_amount"] or 0 for i in t["inputs"])
            totout = sum(o["amount"] for o in t["outputs"])
            fee = totin - totout if ins > 0 else 0
            if ins > 0:
                cps = [o["script_public_key_address"] for o in t["outputs"] if not mine(o["script_public_key_address"])]
            else:
                cps = sorted({i["previous_outpoint_address"] for i in t["inputs"]})
            mv.append(dict(t=dt.datetime.fromtimestamp(t["block_time"] / 1000, UTC), wallet=wallet, coin="KAS",
                           delta=D(outs - ins) / D(10**8), fee=D(fee) / D(10**8), tx=tid, cps=cps))
    # bitcoin / litecoin (Esplora)
    for addr, (wallet, chain) in own.items():
        if chain not in ("bitcoin", "litecoin"):
            continue
        coin = "BTC" if chain == "bitcoin" else "LTC"
        txs = {}
        for f in sorted(glob.glob(os.path.join(hist, f"{wallet}_{chain}_page*.json"))):
            for t in json.load(open(f)):
                txs[t["txid"]] = t
        for tid, t in txs.items():
            if not t["status"].get("confirmed"):
                continue
            ins = sum(v["prevout"]["value"] for v in t["vin"] if v.get("prevout") and v["prevout"].get("scriptpubkey_address") == addr)
            outs = sum(o["value"] for o in t["vout"] if o.get("scriptpubkey_address") == addr)
            fee = t["fee"] if ins > 0 else 0
            if ins > 0:
                cps = [o.get("scriptpubkey_address") for o in t["vout"] if o.get("scriptpubkey_address") != addr]
            else:
                cps = sorted({v["prevout"].get("scriptpubkey_address") for v in t["vin"] if v.get("prevout")})
            mv.append(dict(t=dt.datetime.fromtimestamp(t["status"]["block_time"], UTC), wallet=wallet, coin=coin,
                           delta=D(outs - ins) / D(10**8), fee=D(fee) / D(10**8), tx=tid, cps=[c for c in cps if c]))
    # solana: native SOL and SPL token deltas of the owner per transaction
    for addr, (wallet, chain) in own.items():
        if chain != "solana":
            continue
        seen = set()
        for f in sorted(glob.glob(os.path.join(hist, f"solana_{wallet}_solana_tx_*.json"))):
            r = json.load(open(f)).get("result")
            if not r or r["meta"].get("err"):
                continue
            sig = r["transaction"]["signatures"][0]
            if sig in seen:
                continue
            seen.add(sig)
            keys = [k["pubkey"] if isinstance(k, dict) else k for k in r["transaction"]["message"]["accountKeys"]]
            m = r["meta"]
            t = dt.datetime.fromtimestamp(r["blockTime"], UTC)
            others = sorted({b.get("owner") for b in m.get("postTokenBalances", []) + m.get("preTokenBalances", []) if b.get("owner") and b.get("owner") != addr})
            if addr in keys:
                i = keys.index(addr)
                d = m["postBalances"][i] - m["preBalances"][i]
                fee = m["fee"] if keys[0] == addr else 0
                if d:
                    opp = sorted((abs(m["postBalances"][j] - m["preBalances"][j]), keys[j]) for j in range(len(keys))
                                 if j != i and (m["postBalances"][j] - m["preBalances"][j]) * d < 0)
                    sol_cps = others or [k for _, k in reversed(opp)]
                    mv.append(dict(t=t, wallet=wallet, coin="SOL", delta=D(d) / D(10**9), fee=D(fee) / D(10**9), tx=sig, cps=sol_cps[:3]))
            pre = {b["accountIndex"]: b for b in m.get("preTokenBalances", []) if b.get("owner") == addr}
            post = {b["accountIndex"]: b for b in m.get("postTokenBalances", []) if b.get("owner") == addr}
            for idx in set(pre) | set(post):
                b = post.get(idx) or pre.get(idx)
                coin = MINTS.get(b["mint"], "SPL-" + b["mint"][:6])
                a1 = D(post[idx]["uiTokenAmount"]["amount"]) if idx in post else D(0)
                a0 = D(pre[idx]["uiTokenAmount"]["amount"]) if idx in pre else D(0)
                if a1 != a0:
                    dec = b["uiTokenAmount"]["decimals"]
                    mv.append(dict(t=t, wallet=wallet, coin=coin, delta=(a1 - a0) / D(10**dec), fee=D(0), tx=sig, cps=others[:3]))
    # monero: Cake Wallet export(s)
    for w in bal["wallets"]:
        for i in w["identifiers"]:
            if i["kind"] != "file":
                continue
            with open(os.path.join(pull, i["value"])) as f:
                for r in csv.DictReader(f):
                    if r.get("record_type") != "transaction" or r.get("currency") != "XMR" or r.get("status") != "confirmed":
                        continue
                    a, fee = D(r["amount"]), D(r["fee"] or 0)
                    inc = r["type"] == "incoming"
                    mv.append(dict(t=dt.datetime.fromisoformat(r["date_time"].replace("Z", "+00:00")).astimezone(UTC), wallet=w["wallet"], coin="XMR",
                                   delta=a if inc else -(a + fee), fee=D(0) if inc else fee, tx=r["tx_id"].split("_")[0],
                                   cps=[], note=r.get("note", "")))
    mv.sort(key=lambda m: (m["t"], m["wallet"], m["coin"], m["tx"]))
    for k, m in enumerate(mv):
        m["i"] = k
    return mv


def exchange_moves(db):
    con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    rows = con.execute("""SELECT exchange, refid, time, type, commodity, amount, fee FROM raw_exchange_ledger
        WHERE type IN ('withdrawal','deposit') AND commodity <> 'USD' ORDER BY CAST(time AS REAL)""").fetchall()
    out = []
    for ex, ref, tm, typ, com, amt, fee in rows:
        try:
            t = dt.datetime.fromtimestamp(float(tm), UTC)
        except ValueError:
            t = dt.datetime.fromisoformat(tm.replace("Z", "+00:00"))
        out.append(dict(ex=ex, ref=ref, t=t, typ=typ, coin=com, qty=abs(D(amt)), fee=D(fee or 0)))
    return out


def q(x, n=8):
    if x is None:
        return ""
    v = format(D(x).quantize(D(1).scaleb(-n), rounding=ROUND_HALF_UP), "f")
    return v.rstrip("0").rstrip(".") if "." in v else v


def usd(x):
    return str(D(x).quantize(D("0.01"), rounding=ROUND_HALF_UP)) if x is not None else ""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("pull")
    ap.add_argument("--db", default=os.environ.get("FINANCE_DB", os.path.join(os.environ.get("FINANCE_DATA", "../finance-data"), "finance.db")))
    ap.add_argument("--config", help="wallets.yaml instead of <pull>/labels.json (needs PyYAML)")
    ap.add_argument("--timeline-md", help="after `finance import wallets`: write the swap/transfer timeline (markdown)")
    ap.add_argument("--timeline-csv", help="... and as CSV")
    a = ap.parse_args()
    pull = os.path.abspath(a.pull)
    if a.timeline_md or a.timeline_csv:
        return timeline(pull, a.timeline_md, a.timeline_csv)
    if a.config:
        cfg = load_yaml(a.config)
    else:  # written by `finance download wallets` from wallets.yaml (labels + pass-through only)
        cfg = json.load(open(os.path.join(pull, "labels.json")))
    labels = cfg.get("labels") or {}
    passthrough = [(dt.datetime.fromisoformat(p["from"].replace("Z", "+00:00")), dt.datetime.fromisoformat(p["to"].replace("Z", "+00:00")), p.get("note", ""))
                   for p in cfg.get("passthrough") or []]
    bal = json.load(open(os.path.join(pull, "balances.json")))
    px = Prices(pull)
    mv = movements(pull, bal)
    xm = exchange_moves(a.db)
    for m in mv:
        p = px.usd(m["coin"], m["t"])
        m["px"] = p
        m["usd"] = (abs(m["delta"]) * p) if p is not None else None
        m["used"] = False
        m["label"] = next((labels[c] for c in m["cps"] if c in labels), "")
    events = []

    def ev(kind, t, out=None, inn=None, ref="", conf="", memo=""):
        e = dict(kind=kind, t=t, out=out, inn=inn, ref=ref, conf=conf, memo=memo)
        for m in (out, inn):
            if m is not None:
                m["used"] = True
        events.append(e)
        return e

    # 1. pass-through windows
    for m in mv:
        for f, to, note in passthrough:
            if f <= m["t"] <= to:
                ev("passthrough", m["t"], out=m if m["delta"] < 0 else None, inn=m if m["delta"] > 0 else None, conf="high", memo="pass-through: " + note.split(":")[0])
    # 2. own transfers: same transaction in two wallets
    bytx = {}
    for m in mv:
        if not m["used"]:
            bytx.setdefault((m["coin"], m["tx"]), []).append(m)
    for (coin, tx), ms in bytx.items():
        outs = [m for m in ms if m["delta"] < 0]
        ins = [m for m in ms if m["delta"] > 0]
        if len(outs) == 1 and len(ins) == 1 and outs[0]["wallet"] != ins[0]["wallet"]:
            ev("transfer", outs[0]["t"], out=outs[0], inn=ins[0], conf="high", memo="own wallets, same transaction")
    # 3. exchange withdrawals -> wallet inflows; wallet outflows -> exchange deposits
    cands = []
    for x in xm:
        for m in mv:
            if m["used"] or m["coin"] != x["coin"]:
                continue
            if x["typ"] == "withdrawal" and m["delta"] > 0:
                gap = (m["t"] - x["t"]).total_seconds() / 3600
                if -2 <= gap <= 72 and abs(m["delta"] - x["qty"]) <= max(x["qty"] * D("0.005"), D("0.00002")):
                    cands.append((abs(gap), x, m))
            if x["typ"] == "deposit" and m["delta"] < 0:
                gap = (x["t"] - m["t"]).total_seconds() / 3600
                sent = -m["delta"] - m["fee"]
                if -2 <= gap <= 72 and abs(sent - x["qty"]) <= max(x["qty"] * D("0.005"), D("0.00002")):
                    cands.append((abs(gap), x, m))
    cands.sort(key=lambda c: (c[0], c[1]["t"]))
    xused = set()
    for gap, x, m in cands:
        if m["used"] or (x["ex"], x["ref"]) in xused:
            continue
        xused.add((x["ex"], x["ref"]))
        conf = "high" if gap <= 6 else "medium"
        if x["typ"] == "withdrawal":
            ev("exchange_out", m["t"], inn=m, ref=f"{x['ex']}:{x['ref']}", conf=conf, memo=f"{x['ex']} withdrawal {x['t'].astimezone(DEN):%Y-%m-%d}")
        else:
            ev("exchange_in", m["t"], out=m, ref=f"{x['ex']}:{x['ref']}", conf=conf, memo=f"{x['ex']} deposit {x['t'].astimezone(DEN):%Y-%m-%d}")
    unmatched_x = [x for x in xm if (x["ex"], x["ref"]) not in xused]
    # 4/5. swaps: an outflow then an inflow of another coin within hours, USD close. Strict
    #      (high-confidence) pairs first: swap services may pay out from exchange hot wallets,
    #      so a tight swap pair beats the Coinbase hot-wallet label.
    def swaps(strict):
        outs = [m for m in mv if not m["used"] and m["delta"] < 0 and (m["usd"] or 0) >= DUST_USD]
        ins = [m for m in mv if not m["used"] and m["delta"] > 0 and (m["usd"] or 0) >= DUST_USD]
        sc = []
        for o in outs:
            for i in ins:
                if i["coin"] == o["coin"] or o["usd"] is None or i["usd"] is None:
                    continue
                gap = (i["t"] - o["t"]).total_seconds() / 3600
                if not (-0.05 <= gap <= (0.75 if strict else 24)):
                    continue
                sent_usd = (-o["delta"] - o["fee"]) * o["px"]
                ratio = i["usd"] / sent_usd if sent_usd else D(0)
                if not ((D("0.93") <= ratio <= D("1.02")) if strict else (D("0.80") <= ratio <= D("1.06"))):
                    continue
                score = D(abs(gap)) + abs(D(1) - ratio) * 20
                sc.append((score, gap, ratio, o, i))
        sc.sort(key=lambda s: (s[0], s[3]["t"]))
        for score, gap, ratio, o, i in sc:
            if o["used"] or i["used"]:
                continue
            if gap <= 0.75 and D("0.93") <= ratio <= D("1.02"):
                conf = "high"
            elif gap <= 4 and D("0.85") <= ratio <= D("1.04"):
                conf = "medium"
            else:
                conf = "low"
            ev("swap", o["t"], out=o, inn=i, conf=conf, memo=f"gap {gap * 60:.0f} min, received/sent {ratio:.3f}" + (f"; via {i['label'] or o['label']}" if (i["label"] or o["label"]) else ""))

    swaps(True)
    # Coinbase sends outside the staged Coinbase history (before the books): labeled hot wallets
    for m in mv:
        if not m["used"] and m["delta"] > 0 and m["label"] == "coinbase":
            ev("exchange_out", m["t"], inn=m, ref="coinbase-prebooks", conf="medium", memo="Coinbase send (hot wallet label; not in the staged Coinbase history): cost = market value on receipt (estimate)")
    swaps(False)
    # 6. round trips: sent to an outside address and (nearly) all of it back from that address
    #    later (another own wallet / staking account not tracked here): cost basis carried
    rt = 0
    for o in mv:
        if o["used"] or o["delta"] >= 0 or (o["usd"] or 0) < DUST_USD or not o["cps"]:
            continue
        for i in mv:
            if i["used"] or i["delta"] <= 0 or i["coin"] != o["coin"] or i["wallet"] != o["wallet"] or i["t"] <= o["t"]:
                continue
            if (i["t"] - o["t"]).days > 90 or not set(i["cps"]) & set(o["cps"]):
                continue
            r = i["delta"] / (-o["delta"] - o["fee"])
            if D("0.97") <= r <= D("1.05"):
                rt += 1
                ref = f"rt{rt:03d}"
                ev("roundtrip_out", o["t"], out=o, ref=ref, conf="medium", memo="sent to an outside address that later sent it back (untracked own account?): cost carried")
                ev("roundtrip_in", i["t"], inn=i, ref=ref, conf="medium", memo=f"back from the same address after {(i['t'] - o['t']).days} days, received/sent {r:.4f}")
                break
    # 7. leftovers
    dust = {}
    for m in mv:
        if m["used"]:
            continue
        if (m["usd"] or 0) < DUST_USD:
            k = (m["wallet"], m["coin"])
            dust[k] = dust.get(k, D(0)) + m["delta"]
            continue
        kind = "unmatched_in" if m["delta"] > 0 else "unmatched_out"
        memo = m["label"] or (m.get("note") or "")
        ev(kind, m["t"], out=m if m["delta"] < 0 else None, inn=m if m["delta"] > 0 else None, conf="", memo=memo or "unknown counterparty")
    events.sort(key=lambda e: (e["t"], e["kind"]))

    # ---- write events.csv ----
    cols = ["id", "time", "date", "kind", "wallet_out", "coin_out", "qty_out", "fee_out", "wallet_in", "coin_in", "qty_in",
            "ref", "price_out", "price_in", "usd_out", "usd_in", "confidence", "evidence", "memo"]
    rows = []
    for n, e in enumerate(events, 1):
        o, i = e["out"], e["inn"]
        r = dict(id=f"w{n:04d}", time=e["t"].strftime("%Y-%m-%dT%H:%M:%SZ"), date=e["t"].astimezone(DEN).strftime("%Y-%m-%d"), kind=e["kind"],
                 ref=e["ref"], confidence=e["conf"], memo=e["memo"])
        ev_ids = []
        if o:
            r.update(wallet_out=o["wallet"], coin_out=o["coin"], qty_out=q(-o["delta"] - o["fee"], 12), fee_out=q(o["fee"], 12),
                     price_out=q(o["px"], 8) if o["px"] is not None else "", usd_out=usd((-o["delta"] - o["fee"]) * o["px"]) if o["px"] is not None else "")
            ev_ids.append(short(o["tx"]))
        if i:
            r.update(wallet_in=i["wallet"], coin_in=i["coin"], qty_in=q(i["delta"], 12), price_in=q(i["px"], 8) if i["px"] is not None else "",
                     usd_in=usd(i["usd"]) if i["usd"] is not None else "")
            if not o or i["tx"] != o["tx"]:
                ev_ids.append(short(i["tx"]))
        r["evidence"] = " ".join(ev_ids)
        rows.append(r)
    out = os.path.join(pull, "events.csv")
    with open(out, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=cols)
        w.writeheader()
        for r in rows:
            w.writerow({k: r.get(k, "") for k in cols})
    os.chmod(out, 0o600)
    # summary (counts only)
    kinds = {}
    for r in rows:
        kinds[r["kind"]] = kinds.get(r["kind"], 0) + 1
    print(f"events: {len(rows)} -> {out}")
    for k in sorted(kinds):
        print(f"  {k}: {kinds[k]}")
    print(f"  dust (< ${DUST_USD}, left to the adjustment): " + ", ".join(f"{w} {c} {q(v, 9)}" for (w, c), v in sorted(dust.items())))
    print(f"  exchange crypto withdrawals/deposits not found in these wallets: {len(unmatched_x)}")
    for x in unmatched_x:
        print(f"    {x['t'].astimezone(DEN):%Y-%m-%d} {x['ex']} {x['typ']} {x['coin']} {q(x['qty'])}")
    json.dump([dict(ex=x["ex"], ref=x["ref"], date=x["t"].astimezone(DEN).strftime("%Y-%m-%d"), typ=x["typ"], coin=x["coin"], qty=str(x["qty"])) for x in unmatched_x],
              open(os.path.join(pull, "events-unmatched-exchange.json"), "w"), indent=1)
    os.chmod(os.path.join(pull, "events-unmatched-exchange.json"), 0o600)


KIND_LABEL = {"swap": "swap", "transfer": "own-wallet transfer", "exchange_out": "exchange -> wallet", "exchange_in": "wallet -> exchange",
              "roundtrip_out": "round trip out", "roundtrip_in": "round trip back", "unmatched_in": "UNMATCHED in", "unmatched_out": "UNMATCHED out",
              "passthrough": "pass-through (excluded)"}


def timeline(pull, md_path, csv_path):
    """Timeline of the booked wallet events (events.csv + events-booked.csv from the import)."""
    ev = list(csv.DictReader(open(os.path.join(pull, "events.csv"))))
    bk_path = os.path.join(pull, "events-booked.csv")
    if not os.path.exists(bk_path):
        sys.exit("wallet_events: run `finance import wallets --raw <pull>` first (events-booked.csv)")
    bk = {r["id"]: r for r in csv.DictReader(open(bk_path))}
    rows = []
    for e in ev:
        b = bk.get(e["id"], {})
        gain = D(b.get("gain_short") or 0) + D(b.get("gain_long") or 0)
        rows.append(dict(id=e["id"], date=e["date"], time_utc=e["time"], kind=KIND_LABEL.get(e["kind"], e["kind"]),
                         from_wallet=e["wallet_out"], from_coin=e["coin_out"], from_qty=e["qty_out"], from_usd=e["usd_out"],
                         to_wallet=e["wallet_in"], to_coin=e["coin_in"], to_qty=e["qty_in"], to_usd=e["usd_in"],
                         confidence=e["confidence"], tx=e["evidence"], ref=e["ref"].split(":")[0] if e["ref"] else "",
                         in_books=b.get("booked", ""), cost_basis=b.get("cost", ""), proceeds=b.get("proceeds", "") if e["kind"] == "swap" else "",
                         gain_estimate=str(gain) if e["kind"] == "swap" else "", term=("long" if D(b.get("gain_long") or 0) else "short") if e["kind"] == "swap" else "",
                         memo=e["memo"]))
    cols = list(rows[0].keys()) if rows else []
    if csv_path:
        with open(csv_path, "w", newline="") as f:
            w = csv.DictWriter(f, fieldnames=cols)
            w.writeheader()
            w.writerows(rows)
        os.chmod(csv_path, 0o600)
    if not md_path:
        return
    years = {}
    for r in rows:
        if r["kind"] != "swap":
            continue
        y = years.setdefault(r["date"][:4], dict(n=0, proceeds=D(0), cost=D(0), gain=D(0), booked=0))
        y["n"] += 1
        y["proceeds"] += D(r["proceeds"] or 0)
        y["cost"] += D(r["cost_basis"] or 0)
        y["gain"] += D(r["gain_estimate"] or 0)
        y["booked"] += r["in_books"] == "true"
    L = []
    L.append(f"# Self-custody wallet timeline (pull {os.path.basename(pull)})\n")
    L.append("Generated by `scripts/wallet_events.py --timeline-md` from the on-chain history of every wallet, the Cake Wallet export and the "
             "Kraken/Coinbase ledgers in the books. **Estimates**: swaps are inferred from timing (minutes to hours) and USD amounts at daily "
             "VWAP prices; cost basis is FIFO per wallet, carried from exchange withdrawals; swap proceeds = market value of the coin given up. "
             "Wallet names only; tx ids truncated to 10 characters. Local file (not in git).\n")
    L.append("## Realized gain/loss on wallet swaps by year (estimates for the CPA)\n")
    L.append("| Year | Swaps | In the books | Proceeds | Cost basis | Gain/loss |\n|---|---:|---:|---:|---:|---:|")
    for y in sorted(years):
        v = years[y]
        L.append(f"| {y} | {v['n']} | {v['booked']} | ${v['proceeds']:,.2f} | ${v['cost']:,.2f} | ${v['gain']:,.2f} |")
    lt = any(r["term"] == "long" for r in rows)
    L.append("\n" + ("Some swaps include long-term lots (see the Term column in the CSV). " if lt else "All swap gains are short-term (no lot was held more than a year when swapped). ") +
             "Exchange-side gains (Kraken, Coinbase) are booked from the exchange ledgers and are not included here. Coins that arrived from an "
             "unidentified source (UNMATCHED in) and pre-books Coinbase sends carry their market value on receipt as basis (estimate).\n")
    def table(title, sel):
        L.append(f"## {title}\n")
        L.append("| Date (MT) | Kind | From | Amount | USD | To | Amount | USD | Conf. | Cost basis | Gain (est.) | Tx | Memo |\n|---|---|---|---:|---:|---|---:|---:|---|---:|---:|---|---|")
        for r in rows:
            if sel(r):
                fr = f"{r['from_wallet']} {r['from_coin']}".strip()
                to = f"{r['to_wallet']} {r['to_coin']}".strip()
                if r["ref"] and r["kind"].startswith("exchange"):
                    fr = fr or r["ref"]
                    to = to or r["ref"]
                L.append(f"| {r['date']} | {r['kind']} | {fr} | {r['from_qty']} | {r['from_usd']} | {to} | {r['to_qty']} | {r['to_usd']} | {r['confidence']} | "
                         f"{r['cost_basis']} | {r['gain_estimate']} | {r['tx']} | {r['memo'].replace('|', '/')} |")
        L.append("")
    table("Swaps", lambda r: r["kind"] == "swap")
    table("Own-wallet transfers and round trips", lambda r: r["kind"] in ("own-wallet transfer", "round trip out", "round trip back"))
    table("Exchange <-> wallet moves", lambda r: r["kind"] in ("exchange -> wallet", "wallet -> exchange"))
    table("Unmatched (left in the at-cost adjustment; please identify)", lambda r: r["kind"].startswith("UNMATCHED"))
    pt = [r for r in rows if r["kind"].startswith("pass-through")]
    if pt:
        L.append("## Pass-through (excluded from the books)\n")
        L.append(f"{len(pt)} movements between {pt[0]['time_utc']} and {pt[-1]['time_utc']} (UTC) inside a configured pass-through window "
                 f"({pt[0]['memo']}); someone else's money crossing the wallet: never booked, no gain or loss.\n")
    ux = os.path.join(pull, "events-unmatched-exchange.json")
    if os.path.exists(ux):
        L.append("## Exchange crypto withdrawals/deposits not found in any tracked wallet\n")
        for x in json.load(open(ux)):
            L.append(f"- {x['date']} {x['ex']} {x['typ']} {x['coin']} {x['qty']}")
        L.append("")
    with open(md_path, "w") as f:
        f.write("\n".join(L) + "\n")
    os.chmod(md_path, 0o600)
    print(f"timeline: {len(rows)} events -> {md_path}" + (f", {csv_path}" if csv_path else ""))


if __name__ == "__main__":
    main()
