"""Single-file analysis and restart-safe multiprocessing batch runner."""

from __future__ import annotations

import os
import sqlite3
from concurrent.futures import ProcessPoolExecutor, as_completed
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .detector import detect_envelope
from .framing import scan_envelopes
from .ledger import (
    JobRow,
    complete_job,
    connect,
    init_db,
    mark_failed,
    mark_running,
    pending_jobs,
    reset_running_jobs,
)
from .refine import DEFAULT_MAX_FADE_MS, DEFAULT_MERGE_GAP_MS, refine_segments
from .report import write_reports
from .riff import RiffError, WavReader


@dataclass(frozen=True)
class AnalysisOptions:
    noise_margin_db: float = 12.0
    hangover_ms: int = 250
    max_fade_ms: int = DEFAULT_MAX_FADE_MS
    merge_gap_ms: int = DEFAULT_MERGE_GAP_MS


@dataclass
class BatchRunResult:
    total: int
    done: int
    failed: int
    interrupted: bool
    summary: dict[str, Any]


def analyze_file(path: str | os.PathLike[str], options: AnalysisOptions | None = None) -> dict[str, Any]:
    """Analyze one WAV without loading the file into memory."""

    opts = options or AnalysisOptions()
    with WavReader(path) as reader:
        info = reader.info
        assert info is not None
        frame_samples = max(1, int(round(info.sample_rate * 0.010)))
        if info.total_frames < frame_samples:
            raise RiffError("audio shorter than one frame")
        envelope = scan_envelopes(reader)
    detection = detect_envelope(
        envelope,
        noise_margin_db=opts.noise_margin_db,
        hangover_ms=opts.hangover_ms,
    )
    refined = refine_segments(
        os.fspath(path),
        envelope,
        detection,
        max_fade_ms=opts.max_fade_ms,
        merge_gap_ms=opts.merge_gap_ms,
    )
    segments = [
        {
            "start_sample": int(item.start_sample),
            "end_sample": int(item.end_sample),
            "fade_in_ms": int(item.fade_in_ms),
            "fade_out_ms": int(item.fade_out_ms),
        }
        for item in refined
    ]
    voiced = sum(item.end_sample - item.start_sample for item in refined)
    silence = max(0, envelope.total_samples - voiced)
    return {
        "sample_rate": info.sample_rate,
        "channels": info.channels,
        "encoding": info.encoding,
        "channel_mode": envelope.channel_mode,
        "total_samples": envelope.total_samples,
        "segment_count": len(segments),
        "segments": segments,
        "silence_total_samples": silence,
        "silence_total_ms": round(silence * 1000.0 / info.sample_rate),
    }


def _worker(path: str, options: AnalysisOptions) -> tuple[str, dict[str, Any] | None, str | None]:
    try:
        return "ok", analyze_file(path, options), None
    except RiffError:
        return "error", None, "riff_invalid"
    except Exception as exc:
        return "error", None, f"{type(exc).__name__}: {exc}"


def _store_result(conn: sqlite3.Connection, job: JobRow, result: dict[str, Any]) -> None:
    segments = [
        (
            item["start_sample"],
            item["end_sample"],
            item["fade_in_ms"],
            item["fade_out_ms"],
        )
        for item in result["segments"]
    ]
    complete_job(
        conn,
        job.job_id,
        int(result["sample_rate"]),
        int(result["channels"]),
        int(result["total_samples"]),
        str(result["encoding"]),
        str(result["channel_mode"]),
        segments,
    )


def run_batch(
    db_path: str | os.PathLike[str],
    report_dir: str | os.PathLike[str],
    workers: int = 4,
    options: AnalysisOptions | None = None,
) -> BatchRunResult:
    opts = options or AnalysisOptions()
    conn = connect(db_path)
    init_db(conn)
    reset_running_jobs(conn)
    jobs = pending_jobs(conn)
    if not jobs:
        data = write_reports(conn, report_dir)
        conn.close()
        return BatchRunResult(0, 0, 0, False, data)

    done = 0
    failed = 0
    interrupted = False
    executor = ProcessPoolExecutor(max_workers=max(1, int(workers)))
    futures: dict[Any, JobRow] = {}
    try:
        for job in jobs:
            mark_running(conn, job.job_id)
            future = executor.submit(_worker, job.path, opts)
            futures[future] = job
        for future in as_completed(futures):
            job = futures[future]
            try:
                status, result, error = future.result()
            except Exception as exc:
                status, result, error = "error", None, f"worker failure: {exc}"
            if status == "ok" and result is not None:
                _store_result(conn, job, result)
                done += 1
            else:
                mark_failed(conn, job.job_id, error or "unknown worker error")
                failed += 1
    except KeyboardInterrupt:
        interrupted = True
        for future in futures:
            future.cancel()
        reset_running_jobs(conn)
    finally:
        executor.shutdown(wait=not interrupted, cancel_futures=interrupted)
    data = write_reports(conn, report_dir)
    conn.close()
    return BatchRunResult(len(jobs), done, failed, interrupted, data)
