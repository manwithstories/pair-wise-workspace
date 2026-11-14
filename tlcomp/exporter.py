"""Frame loop, deterministic frame summaries, multiprocessing, and resume."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

import numpy as np

from .compositor import Compositor
from .errors import AssetError, ManifestError, ResumeError
from .timeline import Timeline, load_timeline

_WORKER_TIMELINE: Timeline | None = None
_WORKER_COMPOSITOR: Compositor | None = None


def _round(value: float, digits: int) -> float:
    rounded = round(float(value), digits)
    return 0.0 if rounded == 0.0 else rounded


def summarize_frame(compositor: Compositor, frame: int) -> dict[str, Any]:
    rgba = compositor.render(frame)
    digest = hashlib.sha256(rgba.tobytes(order="C")).hexdigest()
    alpha = rgba[..., 3]
    coverage = float(np.count_nonzero(alpha)) / float(alpha.size)
    mean_rgb = rgba[..., :3].mean(axis=(0, 1), dtype=np.float64)
    return {
        "frame": frame,
        "pts_ms": _round(compositor.timeline.pts_ms(frame), 6),
        "sha256": digest,
        "coverage": _round(coverage, 6),
        "mean_rgb": [_round(float(channel), 3) for channel in mean_rgb],
    }


def summary_line(summary: dict[str, Any]) -> bytes:
    return (
        json.dumps(summary, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        + "\n"
    ).encode("utf-8")


def _worker_init(project_path: str) -> None:
    global _WORKER_TIMELINE, _WORKER_COMPOSITOR
    _WORKER_TIMELINE = load_timeline(project_path)
    _WORKER_COMPOSITOR = Compositor(_WORKER_TIMELINE)
    for layer in _WORKER_TIMELINE.layers:
        _WORKER_COMPOSITOR.cache.get(layer.path)


def _worker_summary(frame: int) -> tuple[int, dict[str, Any]]:
    if _WORKER_COMPOSITOR is None:
        raise RuntimeError("worker was not initialized")
    return frame, summarize_frame(_WORKER_COMPOSITOR, frame)


def compute_summaries(
    timeline: Timeline,
    frames: Iterable[int],
    workers: int = 1,
) -> list[dict[str, Any]]:
    for layer in timeline.layers:
        if not layer.path.is_file():
            raise AssetError(f"missing layer asset: {layer.path}")
    pending = list(frames)
    if not pending:
        return []
    if workers == 1:
        compositor = Compositor(timeline)
        for layer in timeline.layers:
            compositor.cache.get(layer.path)
        return [summarize_frame(compositor, frame) for frame in pending]
    import multiprocessing

    context = multiprocessing.get_context()
    with context.Pool(
        processes=workers,
        initializer=_worker_init,
        initargs=(str(timeline.project_path),),
    ) as pool:
        results = list(
            pool.imap_unordered(_worker_summary, pending, chunksize=max(1, len(pending) // (workers * 8)))
        )
    results.sort(key=lambda item: item[0])
    return [summary for _, summary in results]


def _validate_summary(summary: Any, expected_frame: int) -> dict[str, Any]:
    if not isinstance(summary, dict):
        raise ResumeError(f"manifest row {expected_frame} is not an object")
    required = {"frame", "pts_ms", "sha256", "coverage", "mean_rgb"}
    if set(summary) != required or summary.get("frame") != expected_frame:
        raise ResumeError(f"manifest row {expected_frame} is incomplete or out of order")
    digest = summary.get("sha256")
    if not isinstance(digest, str) or len(digest) != 64:
        raise ResumeError(f"manifest row {expected_frame} has an invalid sha256")
    try:
        int(digest, 16)
        pts = float(summary["pts_ms"])
        coverage = float(summary["coverage"])
        mean_rgb = [float(value) for value in summary["mean_rgb"]]
    except (KeyError, TypeError, ValueError) as exc:
        raise ResumeError(f"manifest row {expected_frame} has invalid values") from exc
    if not np.isfinite(pts) or not 0.0 <= coverage <= 1.0 or len(mean_rgb) != 3:
        raise ResumeError(f"manifest row {expected_frame} has invalid values")
    return summary


def read_resume_prefix(path: Path) -> list[dict[str, Any]]:
    try:
        data = path.read_bytes()
    except FileNotFoundError:
        return []
    except OSError as exc:
        raise ResumeError(f"cannot read resume manifest: {exc}") from exc
    if not data:
        return []
    chunks = data.splitlines(keepends=True)
    complete = chunks
    if chunks and not chunks[-1].endswith(b"\n"):
        complete = chunks[:-1]
    rows: list[dict[str, Any]] = []
    for index, chunk in enumerate(complete):
        try:
            summary = json.loads(chunk)
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            raise ResumeError(f"manifest row {index} is corrupted") from exc
        rows.append(_validate_summary(summary, index))
    return rows


def _atomic_write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except BaseException:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
        raise


def render_project(
    project_path: str | Path,
    out_dir: str | Path,
    *,
    workers: int = 1,
    resume: bool = False,
) -> dict[str, Any]:
    if workers <= 0:
        raise ValueError("workers must be greater than zero")
    started = time.monotonic()
    timeline = load_timeline(project_path)
    output = Path(out_dir)
    output.mkdir(parents=True, exist_ok=True)
    manifest = output / "manifest.jsonl"
    existing = read_resume_prefix(manifest) if resume else []
    if len(existing) > timeline.frame_count:
        raise ResumeError("resume manifest has more rows than the timeline")
    summaries = existing + compute_summaries(
        timeline,
        range(len(existing), timeline.frame_count),
        workers,
    )
    data = b"".join(summary_line(summary) for summary in summaries)
    _atomic_write(manifest, data)
    elapsed_ms = int(round((time.monotonic() - started) * 1000))
    return {
        "frames": len(summaries),
        "manifest": str(manifest),
        "elapsed_ms": elapsed_ms,
    }


def parse_manifest(path: str | Path) -> list[dict[str, Any]]:
    try:
        with Path(path).open("r", encoding="utf-8") as handle:
            lines = handle.readlines()
    except OSError as exc:
        raise ManifestError(f"cannot read manifest {path}: {exc}") from exc
    rows: list[dict[str, Any]] = []
    for index, line in enumerate(lines):
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError as exc:
            raise ManifestError(f"manifest row {index} is invalid JSON") from exc
        rows.append(_validate_manifest_row(row, index))
    return rows


def _validate_manifest_row(row: Any, index: int) -> dict[str, Any]:
    if not isinstance(row, dict):
        raise ManifestError(f"manifest row {index} is not an object")
    if row.get("frame") != index:
        raise ManifestError(f"manifest row {index} is out of order")
    required = {"frame", "pts_ms", "sha256", "coverage", "mean_rgb"}
    if not required.issubset(row) or len(str(row.get("sha256", ""))) != 64:
        raise ManifestError(f"manifest row {index} is incomplete")
    return row


def verify_project(
    project_path: str | Path,
    manifest_path: str | Path,
    *,
    workers: int = 1,
) -> dict[str, Any]:
    timeline = load_timeline(project_path)
    rows = parse_manifest(manifest_path)
    expected = compute_summaries(timeline, range(timeline.frame_count), workers)
    mismatches: list[int] = []
    for frame, wanted in enumerate(expected):
        actual = rows[frame] if frame < len(rows) else None
        if actual is None or any(actual.get(key) != value for key, value in wanted.items()):
            mismatches.append(frame)
    if len(rows) > timeline.frame_count:
        mismatches.extend(range(timeline.frame_count, len(rows)))
    first_bad = mismatches[0] if mismatches else None
    return {
        "checked": min(len(rows), timeline.frame_count),
        "mismatch": len(mismatches),
        "first_bad_frame": first_bad,
    }


def diff_manifests(first: str | Path, second: str | Path) -> dict[str, Any]:
    left = parse_manifest(first)
    right = parse_manifest(second)
    differences: list[int] = []
    for frame in range(max(len(left), len(right))):
        left_row = left[frame] if frame < len(left) else None
        right_row = right[frame] if frame < len(right) else None
        if left_row != right_row:
            differences.append(frame)
    intervals: list[dict[str, int]] = []
    for frame in differences:
        if intervals and intervals[-1]["end"] + 1 == frame:
            intervals[-1]["end"] = frame
        else:
            intervals.append({"start": frame, "end": frame})
    return {"diff_frames": len(differences), "intervals": intervals}
