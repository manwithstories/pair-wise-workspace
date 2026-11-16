"""Export detector results to JSONL, CSV and SQLite.

Exports consume the very same :class:`~.model.DetectionResult` objects the
detector produced (or rows re-read from the store); no stage here re-implements
any detection logic.
"""

from __future__ import annotations

import csv
import json
import os
import sqlite3
from dataclasses import dataclass
from typing import Any, Iterable, Iterator, Optional, Sequence

from .model import FileAnalysis
from .store import SCHEMA

SEGMENT_FIELDS = [
    "path",
    "sha256",
    "index",
    "start_sample",
    "end_sample",
    "sample_count",
    "start_seconds",
    "end_seconds",
    "duration_seconds",
    "peak",
    "rms",
]

SILENCE_FIELDS = [
    "path",
    "sha256",
    "index",
    "start_sample",
    "end_sample",
    "sample_count",
    "start_seconds",
    "end_seconds",
    "duration_seconds",
]


class ExportError(Exception):
    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


def _result_rows(analysis: FileAnalysis) -> tuple[list[dict], list[dict]]:
    if analysis.result is None:
        return [], []
    segments = [
        {
            "path": analysis.path,
            "sha256": analysis.sha256,
            "index": seg.index,
            "start_sample": seg.start_sample,
            "end_sample": seg.end_sample,
            "sample_count": seg.sample_count,
            "start_seconds": seg.start_seconds,
            "end_seconds": seg.end_seconds,
            "duration_seconds": seg.duration_seconds,
            "peak": seg.peak,
            "rms": seg.rms,
        }
        for seg in analysis.result.segments
    ]
    silences = [
        {
            "path": analysis.path,
            "sha256": analysis.sha256,
            "index": sil.index,
            "start_sample": sil.start_sample,
            "end_sample": sil.end_sample,
            "sample_count": sil.sample_count,
            "start_seconds": sil.start_seconds,
            "end_seconds": sil.end_seconds,
            "duration_seconds": sil.duration_seconds,
        }
        for sil in analysis.result.silences
    ]
    return segments, silences


@dataclass
class ExportReport:
    jsonl_path: Optional[str] = None
    csv_path: Optional[str] = None
    sqlite_path: Optional[str] = None
    segment_count: int = 0
    silence_count: int = 0
    records_written: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "jsonl_path": self.jsonl_path,
            "csv_path": self.csv_path,
            "sqlite_path": self.sqlite_path,
            "segment_count": self.segment_count,
            "silence_count": self.silence_count,
            "records_written": self.records_written,
        }


def _ensure_parent(path: str) -> None:
    parent = os.path.dirname(os.path.abspath(path))
    if parent and not os.path.isdir(parent):
        raise ExportError(
            "export_dir_missing", f"output directory does not exist: {parent}"
        )


def write_jsonl(analyses: Iterable[FileAnalysis], path: str) -> int:
    """One JSON object per segment, then per silence, streamed line by line."""

    _ensure_parent(path)
    written = 0
    try:
        with open(path, "w", encoding="utf-8") as fh:
            for analysis in analyses:
                segments, silences = _result_rows(analysis)
                for row in segments:
                    record = {"kind": "segment", **row}
                    fh.write(json.dumps(record, sort_keys=True) + "\n")
                    written += 1
                for row in silences:
                    record = {"kind": "silence", **row}
                    fh.write(json.dumps(record, sort_keys=True) + "\n")
                    written += 1
    except OSError as exc:
        raise ExportError("export_write_failed", str(exc)) from exc
    return written


def write_csv(analyses: Iterable[FileAnalysis], path: str) -> int:
    """Two-section CSV (segments, then silences) with a header per section."""

    _ensure_parent(path)
    rows = list(analyses)
    written = 0
    try:
        with open(path, "w", encoding="utf-8", newline="") as fh:
            writer = csv.DictWriter(fh, fieldnames=SEGMENT_FIELDS, extrasaction="ignore")
            writer.writeheader()
            for analysis in rows:
                segments, _ = _result_rows(analysis)
                for row in segments:
                    writer.writerow(row)
                    written += 1
            fh.write("\n")
            writer = csv.DictWriter(fh, fieldnames=SILENCE_FIELDS, extrasaction="ignore")
            writer.writeheader()
            for analysis in rows:
                _, silences = _result_rows(analysis)
                for row in silences:
                    writer.writerow(row)
                    written += 1
    except OSError as exc:
        raise ExportError("export_write_failed", str(exc)) from exc
    return written


def write_sqlite(
    analyses: Iterable[FileAnalysis], path: str, overwrite: bool = False
) -> int:
    """Materialise a standalone sqlite database in the store schema.

    An existing file is never clobbered silently: ``overwrite`` must be set
    explicitly, otherwise the caller gets ``export_exists``.
    """

    _ensure_parent(path)
    if os.path.exists(path):
        if not overwrite:
            raise ExportError(
                "export_exists", f"refusing to overwrite existing database: {path}"
            )
        os.remove(path)
    try:
        conn = sqlite3.connect(path)
    except sqlite3.Error as exc:
        raise ExportError("export_sqlite_failed", str(exc)) from exc
    written = 0
    try:
        conn.executescript(SCHEMA)
        batch_id = conn.execute(
            "INSERT INTO batches(created_at, root, config_json, status)"
            " VALUES(datetime('now'), ?, '{}', 'exported')",
            ("<export>",),
        ).lastrowid
        for analysis in analyses:
            segments, silences = _result_rows(analysis)
            info = analysis.info
            cur = conn.execute(
                "INSERT OR REPLACE INTO files(sha256, path, status, sample_rate,"
                " channels, sample_width, total_samples, duration_seconds,"
                " updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))",
                (
                    analysis.sha256,
                    analysis.path,
                    analysis.status,
                    info.sample_rate if info else None,
                    info.channels if info else None,
                    info.sample_width if info else None,
                    info.nframes if info else None,
                    info.duration_seconds if info else None,
                ),
            )
            file_id = cur.lastrowid
            conn.execute(
                "INSERT OR IGNORE INTO batch_files(batch_id, file_id) VALUES(?, ?)",
                (batch_id, file_id),
            )
            conn.executemany(
                "INSERT INTO segments(file_id, idx, start_sample, end_sample,"
                " sample_count, peak, rms) VALUES(?, ?, ?, ?, ?, ?, ?)",
                [
                    (file_id, r["index"], r["start_sample"], r["end_sample"],
                     r["sample_count"], r["peak"], r["rms"])
                    for r in segments
                ],
            )
            conn.executemany(
                "INSERT INTO silences(file_id, idx, start_sample, end_sample,"
                " sample_count) VALUES(?, ?, ?, ?, ?)",
                [
                    (file_id, r["index"], r["start_sample"], r["end_sample"],
                     r["sample_count"])
                    for r in silences
                ],
            )
            written += len(segments) + len(silences)
        conn.commit()
    except sqlite3.Error as exc:
        conn.rollback()
        raise ExportError("export_sqlite_failed", str(exc)) from exc
    finally:
        conn.close()
    return written


def export_all(
    analyses: Sequence[FileAnalysis],
    jsonl_path: Optional[str] = None,
    csv_path: Optional[str] = None,
    sqlite_path: Optional[str] = None,
    overwrite: bool = False,
) -> ExportReport:
    """Export every requested format from the same result set."""

    report = ExportReport()
    report.segment_count = sum(
        len(a.result.segments) for a in analyses if a.result is not None
    )
    report.silence_count = sum(
        len(a.result.silences) for a in analyses if a.result is not None
    )
    if jsonl_path:
        report.records_written += write_jsonl(analyses, jsonl_path)
        report.jsonl_path = jsonl_path
    if csv_path:
        report.records_written += write_csv(analyses, csv_path)
        report.csv_path = csv_path
    if sqlite_path:
        report.records_written += write_sqlite(analyses, sqlite_path, overwrite)
        report.sqlite_path = sqlite_path
    return report


def read_jsonl(path: str) -> Iterator[dict[str, Any]]:
    """Reusable reader for downstream verification and tests."""

    try:
        with open(path, "r", encoding="utf-8") as fh:
            for lineno, line in enumerate(fh, start=1):
                line = line.strip()
                if not line:
                    continue
                try:
                    yield json.loads(line)
                except json.JSONDecodeError as exc:
                    raise ExportError(
                        "export_jsonl_corrupt", f"line {lineno}: {exc}"
                    ) from exc
    except OSError as exc:
        raise ExportError("export_read_failed", str(exc)) from exc
