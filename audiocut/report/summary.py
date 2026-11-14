"""Run summary report; reads the database only, never the audio."""
from __future__ import annotations

import json

from ..store import db as store


def build_report(db_path: str) -> dict | None:
    run = store.latest_run(db_path)
    if run is None:
        return None
    return {
        "run_id": run["id"],
        "input": run["input_path"],
        "sample_rate": run["sample_rate"],
        "segments": run["n_segments"],
        "silence_total_samples": run["silence_total_samples"],
        "silence_total_sec": run["silence_total_sec"],
        "items": [
            {"index": s["idx"], "start_sample": s["start_sample"],
             "end_sample": s["end_sample"]}
            for s in run["segments"]
        ],
        "silences": [
            {"index": s["idx"], "start_sample": s["start_sample"],
             "end_sample": s["end_sample"]}
            for s in run["silences"]
        ],
        "params": run["params"],
    }


def report_json(db_path: str) -> str | None:
    report = build_report(db_path)
    if report is None:
        return None
    return json.dumps(report, ensure_ascii=False)
