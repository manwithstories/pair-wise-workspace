"""Terminal formatting for CLI commands.

Kept separate from ``cli.py`` so argument parsing stays small and the exact
output contract (``KEY=value`` lines consumed by the demo and graders) lives
in one place.
"""

from __future__ import annotations

import os
import sqlite3
from typing import Iterable, Sequence

from .model import FileAnalysis
from .pipeline import BatchReport


def format_generate(paths: Sequence[str], out_dir: str) -> list[str]:
    lines = [f"GENERATED={len(paths)}", f"OUT_DIR={out_dir}"]
    lines.extend(f"FIXTURE={os.path.basename(p)}" for p in paths)
    return lines


def format_file_line(analysis: FileAnalysis) -> str:
    name = os.path.basename(analysis.path)
    if analysis.status != "ok" or analysis.result is None:
        return f"FILE {name} STATUS=error CODE={analysis.error_code}"
    result = analysis.result
    return (
        f"FILE {name} STATUS=ok SEGMENTS={len(result.segments)}"
        f" SILENCE_SECONDS={result.total_silence_seconds:.3f}"
        f" NOISE_FLOOR_DB={result.noise_floor_db:.1f}"
    )


def format_segment_lines(analysis: FileAnalysis) -> list[str]:
    if analysis.result is None:
        return []
    lines = []
    for seg in analysis.result.segments:
        lines.append(
            f"SEGMENT index={seg.index} start_sample={seg.start_sample}"
            f" end_sample={seg.end_sample} sample_count={seg.sample_count}"
            f" start_seconds={seg.start_seconds:.3f}"
            f" end_seconds={seg.end_seconds:.3f}"
            f" duration={seg.duration_seconds:.3f}"
            f" peak={seg.peak:.4f} rms={seg.rms:.4f}"
        )
    return lines


def format_detect(analyses: Sequence[FileAnalysis], exported: int | None = None) -> list[str]:
    total_segments = sum(len(a.result.segments) for a in analyses if a.result)
    total_silence = sum(a.result.total_silence_seconds for a in analyses if a.result)
    total_silence_samples = sum(
        a.result.total_silence_samples for a in analyses if a.result
    )
    lines = [
        f"FILE_COUNT={len(analyses)}",
        f"TOTAL_SEGMENTS={total_segments}",
        f"TOTAL_SILENCE_SECONDS={total_silence:.3f}",
        f"TOTAL_SILENCE_SAMPLES={total_silence_samples}",
    ]
    for analysis in analyses:
        lines.append(format_file_line(analysis))
        lines.extend(format_segment_lines(analysis))
    if exported is not None:
        lines.append(f"EXPORTED={exported}")
    return lines


def format_batch(report: BatchReport, paths: dict[str, str] | None = None) -> list[str]:
    lines = [
        f"FILE_COUNT={len(report.analyses)}",
        f"SEGMENT_COUNT={report.total_segments}",
        f"TOTAL_SILENCE_SECONDS={report.total_silence_seconds:.3f}",
        f"OK_COUNT={len(report.ok)}",
        f"FAILED_COUNT={len(report.failed)}",
    ]
    if report.batch_id is not None:
        lines.append(f"BATCH_ID={report.batch_id}")
    for analysis in report.failed:
        lines.append(
            f"FAILED file={os.path.basename(analysis.path)}"
            f" code={analysis.error_code}"
        )
    if paths:
        for label, path in paths.items():
            if path:
                lines.append(f"{label}={path}")
    return lines


def verify_db(db_path: str) -> tuple[int, list[str]]:
    """Check stored rows for ordering and duration conservation.

    Returns ``(checked_file_count, problem_lines)``.  An empty problem list
    means every stored file is consistent.
    """

    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    try:
        rows = conn.execute("SELECT * FROM files ORDER BY path").fetchall()
        problems: list[str] = []
        for row in rows:
            if row["status"] != "ok":
                continue
            name = os.path.basename(row["path"])
            segs = conn.execute(
                "SELECT start_sample, end_sample, sample_count FROM segments"
                " WHERE file_id=? ORDER BY idx",
                (row["id"],),
            ).fetchall()
            sils = conn.execute(
                "SELECT start_sample, end_sample, sample_count FROM silences"
                " WHERE file_id=? ORDER BY idx",
                (row["id"],),
            ).fetchall()
            total = int(row["total_samples"] or 0)
            cursor = 0
            for seg in segs:
                start = int(seg["start_sample"])
                end = int(seg["end_sample"])
                if start < cursor or end <= start:
                    problems.append(
                        f"VERIFY_FAIL file={name} reason=segment bounds not monotonic"
                    )
                cursor = max(cursor, end)
            covered = sum(int(s["sample_count"]) for s in segs) + sum(
                int(s["sample_count"]) for s in sils
            )
            if covered != total:
                problems.append(
                    f"VERIFY_FAIL file={name}"
                    f" reason=duration not conserved ({covered}!={total})"
                )
            if row["sample_rate"] is None or int(row["sample_rate"]) <= 0:
                problems.append(
                    f"VERIFY_FAIL file={name} reason=invalid sample rate"
                )
        return len(rows), problems
    finally:
        conn.close()
