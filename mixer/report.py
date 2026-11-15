"""JSON audit reports and terminal summaries."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any, TextIO

import numpy as np

from mixer.automation import automation_times
from mixer.engine import MixSession
from mixer.io import hash_file, resolve_input_path, resolve_workspace_path


SCHEMA = "mix-report/1"


def _relative_path(path: Path, workspace: Path) -> str:
    try:
        return path.resolve().relative_to(workspace.resolve()).as_posix()
    except ValueError:
        return path.as_posix()


def _input_hashes(
    session: MixSession, graph_path: str | Path | None
) -> tuple[dict[str, str], str | None]:
    files: dict[str, Path] = {}
    workspace = session.workspace
    if graph_path is not None:
        graph = resolve_workspace_path(graph_path, workspace)
        files[_relative_path(graph, workspace)] = graph
    for track in session.tracks:
        files[_relative_path(track.source.path, workspace)] = track.source.path
    for node in session.graph.nodes.values():
        if node.type != "gain":
            continue
        value = node.get("automation", node.get("curve"))
        if isinstance(value, str):
            path = resolve_input_path(value, workspace, session.graph.base_dir)
            files[_relative_path(path, workspace)] = path
    hashes = {name: hash_file(path) for name, path in sorted(files.items())}
    aggregate = hashlib.sha256()
    for name, digest in sorted(hashes.items()):
        aggregate.update(name.encode("utf-8"))
        aggregate.update(b"\0")
        aggregate.update(digest.encode("ascii"))
        aggregate.update(b"\0")
    return hashes, aggregate.hexdigest()


def _clean_number(value: float | None) -> float | None:
    if value is None:
        return None
    if not np.isfinite(value):
        return None
    return float(value)


def build_report(
    session: MixSession,
    *,
    block_size: int,
    graph_path: str | Path | None = None,
    output_path: str | Path | None = None,
    automation_step: float = 0.01,
) -> dict[str, Any]:
    input_hashes, aggregate_hash = _input_hashes(session, graph_path)
    start_sec = session.seek_frame / float(session.master_sr)
    duration_sec = session.render_frames / float(session.master_sr)
    times = start_sec + automation_times(duration_sec, automation_step)
    track_rows: list[dict[str, Any]] = []
    automation_samples: dict[str, list[float]] = {}
    for track in session.tracks:
        snapshot = track.metrics.snapshot()
        info = track.source.reader.info
        ratio = track.source.ratio
        track_rows.append(
            {
                "name": track.name,
                "source_file": _relative_path(track.source.path, session.workspace),
                "source_sr": track.source.source_sr,
                "resample_ratio": f"{ratio.numerator}/{ratio.denominator}",
                "peak_dbfs": _clean_number(snapshot.peak_dbfs),
                "rms_dbfs": _clean_number(snapshot.rms_dbfs),
                "clip_count": int(snapshot.clip_count),
                "clip_runs": int(snapshot.clip_runs),
                "clip_times": list(snapshot.clip_times),
                "channels": int(info.selected_channels),
            }
        )
        automation_samples[track.name] = [
            float(value) for value in track.automation_db(times)
        ]
    bus_snapshot = session.bus.snapshot()
    bus = {
        "name": "bus",
        "peak_dbfs": _clean_number(bus_snapshot.peak_dbfs),
        "rms_dbfs": _clean_number(bus_snapshot.rms_dbfs),
        "clip_count": int(bus_snapshot.clip_count),
        "clip_runs": int(bus_snapshot.clip_runs),
        "clip_times": list(bus_snapshot.clip_times),
    }
    all_clip_times = sorted(
        set(bus_snapshot.clip_times).union(
            *[set(track["clip_times"]) for track in track_rows]
        )
    )[:5]
    return {
        "schema": SCHEMA,
        "master_sr": int(session.master_sr),
        "block_size": int(block_size),
        "channels": int(session.output.channels),
        "duration_sec": float(duration_sec),
        "seek_sec": float(start_sec),
        "input_sha256": aggregate_hash,
        "input_hashes": input_hashes,
        "output_file": None if output_path is None else str(output_path),
        "tracks": track_rows,
        "bus": bus,
        "first_clip_times": all_clip_times,
        "clip_times": list(all_clip_times),
        "automation_step_sec": float(automation_step),
        "automation_times": [float(value) for value in times],
        "automation_samples": automation_samples,
    }


def write_report(path: str | Path, report: dict[str, Any]) -> None:
    path = Path(path)
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("w", encoding="utf-8") as handle:
            json.dump(report, handle, indent=2, sort_keys=True, allow_nan=False)
            handle.write("\n")
    except (OSError, TypeError, ValueError) as exc:
        raise OSError(f"cannot write report {path}: {exc}") from exc


def load_report(path: str | Path) -> dict[str, Any]:
    with Path(path).open("r", encoding="utf-8") as handle:
        return json.load(handle)


def _fmt_db(value: object) -> str:
    if value is None:
        return "-"
    return f"{float(value):7.2f}"


def format_table(report: dict[str, Any]) -> str:
    lines = [
        "track          source_sr   ratio     peak    rms   clips  runs",
        "-------------  ---------  --------  ------  -----  -----  ----",
    ]
    for track in report.get("tracks", []):
        lines.append(
            f"{str(track.get('name', ''))[:13]:13}  "
            f"{int(track.get('source_sr', 0)):9d}  "
            f"{str(track.get('resample_ratio', '-')):8}  "
            f"{_fmt_db(track.get('peak_dbfs')):>6}  "
            f"{_fmt_db(track.get('rms_dbfs')):>5}  "
            f"{int(track.get('clip_count', 0)):5d}  "
            f"{int(track.get('clip_runs', 0)):4d}"
        )
    bus = report.get("bus", {})
    lines.append(
        f"{'BUS':13}  {'':9}  {'':8}  "
        f"{_fmt_db(bus.get('peak_dbfs')):>6}  "
        f"{_fmt_db(bus.get('rms_dbfs')):>5}  "
        f"{int(bus.get('clip_count', 0)):5d}  "
        f"{int(bus.get('clip_runs', 0)):4d}"
    )
    return "\n".join(lines)


def print_table(report: dict[str, Any], stream: TextIO | None = None) -> None:
    print(format_table(report), file=stream)
