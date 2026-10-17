"""Report configuration shared by weekly.py, charts.py and statements.py.

Everything personal (names, account labels, goal amounts, narrative notes) lives in
$FINANCE_DATA/config/report.local.yaml (never committed; see config/report.example.yaml for the
format). Without that file the reports use the generic defaults below.

Path: $FINANCE_REPORT_CONFIG, else $FINANCE_DATA/config/report.local.yaml, else
<repo>/../finance-data/config/report.local.yaml.
"""
import copy
import os

HERE = os.path.dirname(os.path.abspath(__file__))


def data_dir():
    return os.environ.get("FINANCE_DATA") or os.path.normpath(os.path.join(HERE, "..", "..", "finance-data"))


DEFAULTS = {
    # who the reports are for (page subtitle)
    "household": "Household",
    # weekly report / charts: cash goal in whole dollars (set your own in report.local.yaml)
    "goal": 10000,
    # accounts whose cash is shown but excluded from the goal (e.g. a trips/holiday fund)
    "trips_accounts": [],
    "trips_receivable_account": "",
    "trips_fund_suffix": " (trips fund)",
    "trips_receivable_label": "Trips: receivable held",
    "trips_chart_label": "Trips fund",
    # income tax jurisdictions in accrual accounts Liabilities:Accrued:Taxes:<Jurisdiction>-<year>
    "tax_jurisdictions": ["Federal", "State"],
    # payable accounts containing these substrings are listed as "owed later", not in the 2-week window
    "later_account_substrings": [],
    "brokerage_name": "Brokerage",
    "large_charge_dollars": 200,   # weekly report flags charges at or above this
    "annual_min_dollars": 100,     # yearly-renewal reminders for charges at or above this
    "plan_items_phrase": "cash-plan dates",
    "later_note": "",
    # message templates ({name} placeholders are filled by the report; empty = omit)
    "notes": {
        "crypto_first_valuation": "",
        "inv_real": "",
        "crypto_real": "",
        "no_income": "No pay or distribution deposits were booked this week.",
        "trips_held": "† Trips money, not counted: trips savings {trips_cash} plus held pay "
                      "{trips_receivable} (not yet deposited).",
        "trips_deposit": "Deposit the remaining held pay ({held}) into the trips account.",
        "travel_skew": "",
    },
    # open receivables by balance account: html template with {amount}
    "receivable_notes": {},
    # charts
    "cash_series": [
        {"key": "bank", "label": "Bank accounts", "prefixes": ["Assets:Bank:"], "color": "#4e79a7"},
    ],
    "loan_group": {"label": "", "account_prefixes": []},
    "income_series": [{"key": "salary", "label": "Salary", "prefix": "Income:Salary"}],
    "income_notes": [],            # [{pattern: "%SQL like%", label: "..."}]: annotate big expense months
    "real_estate_note": "Real estate is carried at cost; vehicles are net of depreciation.",
    # statements.py cash-flow highlights (lower-case substrings of counterparties/descriptions/accounts)
    "highlights": {
        "tax_year_labels": [],     # [{acct: "-2025", desc: "2025", label: "..."}] checked in order
        "land": {"accts": [], "cps": [], "label": "Land purchase payments"},
        "medical": {"cps": [], "desc": [], "label": "Medical bills"},
        "sale_proceeds": {"cps": [], "desc": [], "accts": [], "brokerage_desc": [],
                          "label": "[[inv_proceeds|Proceeds from sales of investments]]"},
        "business_account_token": "",
    },
}


def _merge(base, over):
    for k, v in over.items():
        if isinstance(v, dict) and isinstance(base.get(k), dict):
            _merge(base[k], v)
        else:
            base[k] = v
    return base


_cache = {}


def path():
    return os.environ.get("FINANCE_REPORT_CONFIG") or os.path.join(data_dir(), "config", "report.local.yaml")


def load(p=None):
    p = p or path()
    if p in _cache:
        return _cache[p]
    cfg = copy.deepcopy(DEFAULTS)
    if os.path.exists(p):
        import yaml
        with open(p) as f:
            _merge(cfg, yaml.safe_load(f) or {})
    _cache[p] = cfg
    return cfg


def fill(tpl, **kw):
    """Fill {placeholders}; unknown placeholders are left as written."""
    class _D(dict):
        def __missing__(self, k):
            return "{" + k + "}"
    return tpl.format_map(_D(**kw))
