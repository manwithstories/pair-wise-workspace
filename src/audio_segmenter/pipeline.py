"""Batch orchestration: per-file isolation, parallel analysis, one store write.

Every file is analysed independently.  One broken file is captured as a
:class:`FileAnalysis` with ``status="error"`` and never aborts the batch, and
database writes happen in the calling thread so a transaction covers the
whole run without concurrent writers.
"""

from __future__ import annotations

import os
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from typing import Iterable, Optional, Sequence

from .detector import detect_stream
from .model import DetectionConfig, FileAnalysis
from .store import Store, StoreError
from .wavio import WavError, WavReader, file_sha256


def discover_wavs(root: str) -> list[str]:
    """Sorted *.wav paths under ``root`` (a file or a directory, non-recursive)."""

    if os.path.isfile(root):
        return [root]
    if not os.path.isdir(root):
        raise FileNotFoundError(root)
    names = [
        os.path.join(root, name)
        for name in sorted(os.listdir(root))
        if name.lower().endswith(".wav")
        and os.path.isfile(os.path.join(root, name))
    ]
    return names


def _fallback_digest(path: str) -> str:
    """Stable key for files whose bytes cannot be read at all."""

    import hashlib

    marker = f"unreadable:{os.path.abspath(path)}".encode("utf-8")
    return hashlib.sha256(marker).hexdigest()


def analyze_file(path: str, config: DetectionConfig) -> FileAnalysis:
    """Analyse one file; never raises for bad audio, only for programming errors."""

    try:
        digest = file_sha256(path)
    except WavError as exc:
        return FileAnalysis(
            path=path,
            sha256=_fallback_digest(path),
            status="error",
            error_code=exc.code,
            error_message=exc.message,
        )
    try:
        reader = WavReader(path)
        result = detect_stream(
            lambda: reader.iter_blocks(),
            reader.info.sample_rate,
            reader.info.nframes,
            config,
            path=path,
        )
        return FileAnalysis(
            path=path,
            sha256=digest,
            status="ok",
            info=reader.info,
            result=result,
        )
    except WavError as exc:
        return FileAnalysis(
            path=path,
            sha256=digest,
            status="error",
            error_code=exc.code,
            error_message=exc.message,
        )


@dataclass
class BatchReport:
    analyses: list[FileAnalysis] = field(default_factory=list)
    batch_id: Optional[int] = None
    db_error: Optional[str] = None

    @property
    def ok(self) -> list[FileAnalysis]:
        return [a for a in self.analyses if a.status == "ok"]

    @property
    def failed(self) -> list[FileAnalysis]:
        return [a for a in self.analyses if a.status != "ok"]

    @property
    def total_segments(self) -> int:
        return sum(len(a.result.segments) for a in self.ok if a.result)

    @property
    def total_silence_seconds(self) -> float:
        return sum(
            a.result.total_silence_seconds for a in self.ok if a.result
        )

    def to_dict(self) -> dict:
        return {
            "batch_id": self.batch_id,
            "file_count": len(self.analyses),
            "ok_count": len(self.ok),
            "failed_count": len(self.failed),
            "segment_count": self.total_segments,
            "silence_total_seconds": self.total_silence_seconds,
            "files": [a.to_dict() for a in self.analyses],
            "db_error": self.db_error,
        }


def _worker_count(requested: Optional[int], file_count: int) -> int:
    if requested is not None and requested > 0:
        return min(requested, max(1, file_count))
    cpu = os.cpu_count() or 1
    return min(max(1, cpu), max(1, file_count))


def run_batch(
    paths: Sequence[str],
    config: DetectionConfig,
    workers: Optional[int] = None,
) -> list[FileAnalysis]:
    """Analyse ``paths`` in parallel; results preserve input order."""

    paths = list(paths)
    if not paths:
        return []
    config.validate(48000)  # shape check; per-file rates are validated in detect
    count = _worker_count(workers, len(paths))
    if count == 1:
        return [analyze_file(path, config) for path in paths]
    with ThreadPoolExecutor(max_workers=count) as pool:
        return list(pool.map(lambda p: analyze_file(p, config), paths))


def persist_batch(
    store: Store,
    analyses: Iterable[FileAnalysis],
    config: DetectionConfig,
    root: str,
) -> int:
    """Write a batch in one transaction; returns the batch id."""

    batch_id = store.begin_batch(root, config)
    try:
        with store.transaction() as conn:
            for analysis in analyses:
                store._save_analysis(conn, analysis)
                row = conn.execute(
                    "SELECT id FROM files WHERE sha256=?", (analysis.sha256,)
                ).fetchone()
                if row is not None:
                    conn.execute(
                        "INSERT OR IGNORE INTO batch_files(batch_id, file_id)"
                        " VALUES(?, ?)",
                        (batch_id, int(row["id"])),
                    )
    except Exception:
        store.finish_batch(batch_id, "failed")
        raise
    store.finish_batch(batch_id, "ok")
    return batch_id


def execute_batch(
    root: str,
    config: DetectionConfig,
    store: Optional[Store] = None,
    workers: Optional[int] = None,
) -> BatchReport:
    """Discover, analyse and (optionally) persist one batch."""

    paths = discover_wavs(root)
    analyses = run_batch(paths, config, workers=workers)
    report = BatchReport(analyses=analyses)
    if store is not None:
        try:
            report.batch_id = persist_batch(store, analyses, config, root)
        except StoreError as exc:
            report.db_error = f"{exc.code}: {exc.message}"
    return report
