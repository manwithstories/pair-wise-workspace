import json
import os
import sqlite3

from etl.cli import main as cli_main

CONFIG_TEMPLATE = """
target_table: facts
failure_policy: {policy}
batch_size: 2
run_dir: {run_dir}
target_schema:
  order_id: {{type: string, pk: true}}
  quantity: {{type: int}}
  trade_date: {{type: datetime}}
  status: {{type: string, enum: status}}
  updated_at: {{type: datetime}}
enums:
  status: {{P: paid}}
sources:
  - name: db
    type: sqlite
    path: {db}
    table: t
    priority: 1
    bindings:
      order_id: order_id
      quantity: quantity
      trade_date: trade_date
      status: status
      updated_at: upd
  - name: file
    type: csv
    path: {csv}
    priority: 2
    bindings:
      order_id: order_id
      quantity: qty
      trade_date: tdate
      status: status
      updated_at: upd
"""


def _build(tmp_path, policy="quarantine", extra_db_rows=()):
    csv_path = tmp_path / "a.csv"
    csv_path.write_text(
        "order_id,qty,tdate,status,upd\n"
        "O1,10,2024/01/01,P,2024/01/02 00:00:00\n"
        "O2,20,2024/01/01,P,2024/01/02 00:00:00\n"
        "O2,20,2024/01/01,P,2024/01/02 00:00:00\n"   # exact duplicate
        "O3,30,not-a-date,P,2024/01/02 00:00:00\n"   # invalid_date
    )
    db_path = tmp_path / "b.db"
    conn = sqlite3.connect(db_path)
    conn.execute("CREATE TABLE t (order_id TEXT, quantity INTEGER, "
                 "trade_date INTEGER, status TEXT, upd INTEGER)")
    rows = [
        ("O1", 99, 1704067200, "P", 1704153600),      # wins over csv by priority
        ("O4", 40, 1704067200, "P", 1704153600),
        *extra_db_rows,
    ]
    conn.executemany("INSERT INTO t VALUES (?,?,?,?,?)", rows)
    conn.commit()
    conn.close()
    config_path = tmp_path / "mapping.yaml"
    config_path.write_text(CONFIG_TEMPLATE.format(
        policy=policy, run_dir=tmp_path / "run",
        db=db_path, csv=csv_path))
    return str(config_path)


def test_pipeline_quarantine_and_idempotent(tmp_path):
    config = _build(tmp_path)
    assert cli_main(["align", "--config", config]) == 2
    run_dir = tmp_path / "run"
    report = json.loads((run_dir / "report.json").read_text())
    assert report["load"]["rows_in"] == 6
    assert report["load"]["rejected"] == 1
    assert report["load"]["dup_resolved"] == 2
    assert report["load"]["loaded"] == 3
    assert report["conversion_failures"]["reasons"] == {"invalid_date": 1}
    quarantine = (run_dir / "quarantine.csv").read_text()
    assert "invalid_date" in quarantine and "not-a-date" in quarantine
    first_checksum = report["checksum"]
    assert cli_main(["align", "--config", config]) == 2
    again = json.loads((run_dir / "report.json").read_text())
    assert again["checksum"] == first_checksum
    # staging table is always cleaned up
    conn = sqlite3.connect(run_dir / "facts.db")
    tables = [r[0] for r in conn.execute(
        "SELECT name FROM sqlite_master WHERE type='table'")]
    conn.close()
    assert tables == ["facts"]


def test_pipeline_fail_fast(tmp_path):
    config = _build(tmp_path, policy="fail-fast")
    assert cli_main(["align", "--config", config]) == 1


def test_pipeline_unmerged_exit_3(tmp_path):
    # same pk, same source, same updated_at, different payload -> unmerged
    conflict = [("O9", 1, 1704067200, "P", 1704153600),
                ("O9", 2, 1704067200, "P", 1704153600)]
    config = _build(tmp_path, extra_db_rows=conflict)
    assert cli_main(["align", "--config", config]) == 3
    report = json.loads((tmp_path / "run" / "report.json").read_text())
    assert report["load"]["unmerged"] == 1
