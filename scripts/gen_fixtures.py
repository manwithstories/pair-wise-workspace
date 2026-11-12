"""Generate deterministic source fixtures (CSV / JSONL / SQLite).

Layout (seed-stable):
  fixtures/supplier_a.csv   500 rows, ';' delimited, cents amounts,
                            slash dates, thousands separators, full-width
                            digits, parenthesised negatives, sci-notation
  fixtures/biz_line.jsonl   400 rows, nested order.*/meta.* paths,
                            English-month dates, yuan amounts
  fixtures/settlement.db    300 rows, epoch timestamps, yuan amounts

Row budget: 1200 rows in, 1138 unique pks (62 duplicate rows resolved),
11 bad rows producing 14 conversion failures
(invalid_date=5 enum_unmapped=4 overflow=2 not_a_number=2 array_rejected=1).
"""
from __future__ import annotations

import argparse
import csv
import json
import os
import random
import sqlite3
from datetime import datetime, timedelta

REGIONS = ["East", "South", "North", "West"]
REPS = ["Alice", "Bob", "Carol", "David", "NA"]       # 'NA' -> null token
WAREHOUSES = ["WH-1", "WH-2", ""]                     # '' -> empty null
CATEGORIES = ["electronics", "grocery", "apparel", "home"]
CURRENCIES = ["CNY", "CNY", "RMB", "USD", "EUR"]
STATUSES = ["P", "PAID", "paid", "PEND", "pending", "R", "refunded",
            "C", "cancelled"]
CHANNELS = ["ON", "online", "OFF", "offline", "POS", "pos"]
PAYMENTS = ["CARD", "card", "CASH", "cash", "TR", "transfer"]

_FULLWIDTH = str.maketrans("0123456789", "０１２３４５６７８９")

CSV_COLUMNS = ["order_id", "customer_id", "sku", "category", "qty",
               "amt_cents", "unit_price", "tax", "currency", "status",
               "channel", "tdate", "settle_date", "ts", "region",
               "sales_rep", "warehouse", "payment_method", "is_gift",
               "weight_kg", "supplier_code", "legacy_code"]

SQLITE_DDL = """
CREATE TABLE settlement (
  order_id TEXT, customer_id TEXT, sku TEXT, category TEXT,
  quantity TEXT, amount_yuan REAL, unit_price REAL, tax REAL,
  currency TEXT, status TEXT, channel TEXT,
  trade_date INTEGER, settle_date TEXT, upd_ts INTEGER,
  region TEXT, sales_rep TEXT, warehouse TEXT, payment_method TEXT,
  is_gift INTEGER, supplier_code TEXT
)
"""


def _pk(i: int) -> str:
    return f"ORD-{i:06d}"


def generate(root: str, seed: int) -> dict:
    rng = random.Random(seed)
    fixtures = os.path.join(root, "fixtures")
    os.makedirs(fixtures, exist_ok=True)

    # ---- primary-key pools ---------------------------------------------
    shared_cj = list(range(1, 31))        # csv ∩ jsonl      (30 dup rows)
    shared_cs = list(range(31, 51))       # csv ∩ sqlite     (20 dup rows)
    shared_js = list(range(51, 61))       # jsonl ∩ sqlite   (10 dup rows)
    csv_only = list(range(101, 549))      # 448
    jsonl_only = list(range(601, 961))    # 360
    sqlite_only = list(range(1001, 1271))  # 270

    def rand_dt() -> datetime:
        return datetime(2024, 1, 1) + timedelta(
            seconds=rng.randrange(0, 365 * 86400))

    def base_values():
        qty = rng.randint(1, 2500)
        unit = round(rng.uniform(5, 500), 2)
        tdate = rand_dt()
        sdate = tdate + timedelta(days=rng.randint(1, 7))
        upd = sdate + timedelta(hours=rng.randint(0, 48))
        return {
            "qty": qty, "unit": unit,
            "amount": round(unit * qty + rng.uniform(-5, 5), 2),
            "tax": round(unit * qty * 0.06, 2),
            "tdate": tdate, "sdate": sdate, "upd": upd,
            "customer_id": f"CUST-{rng.randint(100, 999)}",
            "sku": f"SKU-{rng.randint(1000, 9999)}",
            "category": rng.choice(CATEGORIES),
            "currency": rng.choice(CURRENCIES),
            "status": rng.choice(STATUSES),
            "channel": rng.choice(CHANNELS),
            "region": rng.choice(REGIONS),
            "sales_rep": rng.choice(REPS),
            "warehouse": rng.choice(WAREHOUSES),
            "payment_method": rng.choice(PAYMENTS),
            "is_gift": 1 if rng.random() < 0.1 else 0,
            "weight": round(rng.uniform(0.1, 20), 2),
        }

    # ---- CSV source ------------------------------------------------------
    def fmt_qty(q: int) -> str:
        r = rng.random()
        if q >= 1000 and r < 0.5:
            return f"{q:,}"                       # thousands separator
        if r < 0.15:
            return str(q).translate(_FULLWIDTH)   # full-width digits
        return str(q)

    def csv_row(i: int) -> dict:
        v = base_values()
        cents = int(round(v["amount"] * 100))
        weight = (f"{v['weight']:.1e}" if rng.random() < 0.1
                  else f"{v['weight']}")
        return {
            "order_id": _pk(i),
            "customer_id": v["customer_id"],
            "sku": v["sku"],
            "category": v["category"],
            "qty": fmt_qty(v["qty"]),
            "amt_cents": f"({-cents})" if cents < 0 else str(cents),
            "unit_price": f"{v['unit']:.2f}",
            "tax": f"{v['tax']:.2f}",
            "currency": v["currency"],
            "status": v["status"],
            "channel": v["channel"],
            "tdate": v["tdate"].strftime("%Y/%m/%d"),
            "settle_date": v["sdate"].strftime("%Y/%m/%d"),
            "ts": v["upd"].strftime("%Y/%m/%d %H:%M:%S"),
            "region": v["region"],
            "sales_rep": v["sales_rep"],
            "warehouse": v["warehouse"],
            "payment_method": v["payment_method"],
            "is_gift": str(v["is_gift"]),
            "weight_kg": weight,
            "supplier_code": "SUP-A",
            "legacy_code": f"LG-{rng.randint(10000, 99999)}",
        }

    csv_rows = [csv_row(i) for i in shared_cj + shared_cs + csv_only]
    # injected failures (unique pks, tail of file so schema sniffing is clean)
    bad = {r["order_id"]: r for r in csv_rows}
    bad[_pk(544)]["tdate"] = "2024/13/99"          # invalid_date
    bad[_pk(544)]["status"] = "XX"                # enum_unmapped
    bad[_pk(545)]["tdate"] = "not-a-date"         # invalid_date
    bad[_pk(545)]["qty"] = "99999999999999999999"  # overflow
    bad[_pk(546)]["tdate"] = "2024/02/30"         # invalid_date
    bad[_pk(547)]["status"] = "??"                # enum_unmapped
    bad[_pk(548)]["unit_price"] = "abc"           # not_a_number
    csv_rows.append(dict(csv_rows[0]))            # exact duplicate
    csv_rows.append(dict(csv_rows[1]))            # exact duplicate

    csv_path = os.path.join(fixtures, "supplier_a.csv")
    with open(csv_path, "w", encoding="utf-8", newline="") as fh:
        writer = csv.DictWriter(fh, fieldnames=CSV_COLUMNS, delimiter=";")
        writer.writeheader()
        writer.writerows(csv_rows)

    # ---- JSONL source ------------------------------------------------------
    def jsonl_row(i: int) -> dict:
        v = base_values()
        return {
            "order": {"id": _pk(i), "amt": v["amount"]},
            "customer_id": v["customer_id"],
            "sku": v["sku"],
            "category": v["category"],
            "quantity": v["qty"],
            "unit_price": v["unit"],
            "currency": v["currency"],
            "status": v["status"],
            "channel": v["channel"],
            "trade_date": v["tdate"].strftime("%b %d, %Y"),
            "settle_date": v["sdate"].strftime("%b %d, %Y"),
            "meta": {"ts": int(v["upd"].timestamp())},
            "region": v["region"],
            "sales_rep": v["sales_rep"],
            "warehouse": v["warehouse"],
            "payment_method": v["payment_method"],
            "is_gift": v["is_gift"],
            "weight_kg": v["weight"],
            "supplier_code": "SUP-B",
            "debug": f"trace-{rng.randint(100000, 999999)}",
        }

    jsonl_rows = [jsonl_row(i) for i in shared_cj + shared_js + jsonl_only]
    jbad = {r["order"]["id"]: r for r in jsonl_rows}
    jbad[_pk(957)]["trade_date"] = "Foo 99, 2024"     # invalid_date
    jbad[_pk(957)]["order"]["amt"] = ["x", "y"]       # array_rejected
    jbad[_pk(958)]["trade_date"] = "Smarch 1, 2024"   # invalid_date
    jbad[_pk(959)]["status"] = "BAD"                  # enum_unmapped
    jbad[_pk(960)]["unit_price"] = "abc"              # not_a_number

    jsonl_path = os.path.join(fixtures, "biz_line.jsonl")
    with open(jsonl_path, "w", encoding="utf-8") as fh:
        for row in jsonl_rows:
            fh.write(json.dumps(row, ensure_ascii=False) + "\n")

    # ---- SQLite source -----------------------------------------------------
    def sqlite_row(i: int) -> tuple:
        v = base_values()
        return (
            _pk(i), v["customer_id"], v["sku"], v["category"],
            v["qty"], v["amount"], v["unit"], v["tax"],
            v["currency"], v["status"], v["channel"],
            int(v["tdate"].timestamp()),
            v["sdate"].strftime("%Y-%m-%d %H:%M:%S"),
            int(v["upd"].timestamp()),
            v["region"], v["sales_rep"], v["warehouse"],
            v["payment_method"], v["is_gift"], "SUP-C",
        )

    sqlite_rows = [sqlite_row(i) for i in shared_cs + shared_js + sqlite_only]
    sbad = {r[0]: idx for idx, r in enumerate(sqlite_rows)}

    def replace(i: int, **kw):
        idx = sbad[_pk(i)]
        row = list(sqlite_rows[idx])
        cols = ["order_id", "customer_id", "sku", "category", "quantity",
                "amount_yuan", "unit_price", "tax", "currency", "status",
                "channel", "trade_date", "settle_date", "upd_ts", "region",
                "sales_rep", "warehouse", "payment_method", "is_gift",
                "supplier_code"]
        for key, val in kw.items():
            row[cols.index(key)] = val
        sqlite_rows[idx] = tuple(row)

    replace(1269, status="UNKNOWN")                          # enum_unmapped
    replace(1270, quantity="99999999999999999999999")        # overflow

    db_path = os.path.join(fixtures, "settlement.db")
    if os.path.exists(db_path):
        os.remove(db_path)
    conn = sqlite3.connect(db_path)
    conn.execute(SQLITE_DDL)
    conn.executemany(
        "INSERT INTO settlement VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        sqlite_rows,
    )
    conn.commit()
    conn.close()

    return {
        "csv": len(csv_rows),
        "jsonl": len(jsonl_rows),
        "sqlite": len(sqlite_rows),
        "dir": fixtures,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seed", type=int, default=7)
    parser.add_argument("--root", default=os.path.dirname(
        os.path.dirname(os.path.abspath(__file__))))
    args = parser.parse_args()
    stats = generate(args.root, args.seed)
    print(f"fixtures csv={stats['csv']} jsonl={stats['jsonl']} "
          f"sqlite={stats['sqlite']} dir={stats['dir']}")


if __name__ == "__main__":
    main()
