"""Aggregate ledger results and write CSV/JSON delivery manifests."""

from __future__ import annotations

import csv
import json
import os
import sqlite3
from pathlib import Path
from typing import Any


CUT_COLUMNS = ("file", "index", "start_sample", "end_sample", "start_ms", "end_ms")


def _file_rows(conn: sqlite3.Connection) -> list[dict[str, Any]]:
    rows = conn.execute(
        """
        SELECT f.id, f.path, f.sample_rate, f.channels, f.total_samples, f.encoding,
               f.status, f.error, j.status AS job_status,
               COUNT(s.id) AS segment_count,
               COALESCE(SUM(s.end_sample - s.start_sample), 0) AS voiced_samples
        FROM files f
        LEFT JOIN jobs j ON j.file_id = f.id
        LEFT JOIN segments s ON s.file_id = f.id
        GROUP BY f.id, j.status
        ORDER BY f.id
        """
    ).fetchall()
    result = []
    for row in rows:
        total = int(row["total_samples"] or 0)
        voiced = int(row["voiced_samples"] or 0)
        silence = max(0, total - voiced)
        sample_rate = int(row["sample_rate"] or 0)
        result.append(
            {
                "id": int(row["id"]),
                "file": str(row["path"]),
                "status": str(row["job_status"] or row["status"]),
                "sample_rate": sample_rate,
                "channels": int(row["channels"] or 0),
                "total_samples": total,
                "segment_count": int(row["segment_count"]),
                "silence_total_samples": silence,
                "silence_total_ms": silence * 1000.0 / sample_rate if sample_rate else 0.0,
                "error": row["error"],
            }
        )
    return result


def summary(conn: sqlite3.Connection) -> dict[str, Any]:
    files = _file_rows(conn)
    ok = sum(1 for row in files if row["status"] == "done")
    failed = sum(1 for row in files if row["status"] == "failed")
    segments = sum(int(row["segment_count"]) for row in files)
    silence_samples = sum(int(row["silence_total_samples"]) for row in files)
    silence_ms = sum(float(row["silence_total_ms"]) for row in files)
    return {
        "files": len(files),
        "ok": ok,
        "failed": failed,
        "pending": sum(1 for row in files if row["status"] not in ("done", "failed")),
        "segments": segments,
        "silence_total_samples": silence_samples,
        "silence_total_ms": round(silence_ms),
    }


def _atomic_text(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(text, encoding="utf-8")
    os.replace(temporary, path)


def _write_csv(path: Path, columns: tuple[str, ...], rows: list[dict[str, Any]]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=columns, extrasaction="ignore")
        writer.writeheader()
        writer.writerows(rows)
    os.replace(temporary, path)


def write_reports(conn: sqlite3.Connection, out_dir: str | os.PathLike[str]) -> dict[str, Any]:
    directory = Path(out_dir)
    data = summary(conn)
    files = _file_rows(conn)
    _atomic_text(directory / "summary.json", json.dumps(data, ensure_ascii=False, indent=2) + "\n")
    _atomic_text(directory / "files.json", json.dumps(files, ensure_ascii=False, indent=2) + "\n")
    summary_columns = ("files", "ok", "failed", "pending", "segments", "silence_total_samples", "silence_total_ms")
    _write_csv(directory / "summary.csv", summary_columns, [data])
    file_columns = (
        "file",
        "status",
        "sample_rate",
        "channels",
        "total_samples",
        "segment_count",
        "silence_total_samples",
        "silence_total_ms",
        "error",
    )
    _write_csv(directory / "files.csv", file_columns, files)
    return data


def cut_rows(conn: sqlite3.Connection) -> list[dict[str, Any]]:
    rows = conn.execute(
        """
        SELECT f.path, s.segment_index, s.start_sample, s.end_sample, s.start_ms, s.end_ms
        FROM segments s JOIN files f ON f.id = s.file_id
        JOIN jobs j ON j.file_id = f.id
        WHERE j.status = 'done'
        ORDER BY f.id, s.segment_index
        """
    ).fetchall()
    return [
        {
            "file": str(row["path"]),
            "index": int(row["segment_index"]),
            "start_sample": int(row["start_sample"]),
            "end_sample": int(row["end_sample"]),
            "start_ms": round(float(row["start_ms"]), 3),
            "end_ms": round(float(row["end_ms"]), 3),
        }
        for row in rows
    ]


def export_cuts(conn: sqlite3.Connection, out_dir: str | os.PathLike[str], fmt: str = "csv") -> Path:
    directory = Path(out_dir)
    rows = cut_rows(conn)
    if fmt == "csv":
        path = directory / "cuts.csv"
        _write_csv(path, CUT_COLUMNS, rows)
    elif fmt == "json":
        path = directory / "cuts.json"
        _atomic_text(path, json.dumps(rows, ensure_ascii=False, indent=2) + "\n")
    else:
        raise ValueError("format must be csv or json")
    return path
