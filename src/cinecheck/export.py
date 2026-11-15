"""Terminal/CSV/JSON renderers sharing one analysis payload.

Both the ``analyze`` and ``repair`` commands build the same payload via
:func:`build_payload`, so field names and meanings can never drift between
the two output paths. Every document carries a ``schema_version``.
"""

from __future__ import annotations

import csv
import io
import json
from pathlib import Path

from . import SCHEMA_VERSION
from .stats import FrameStats
from .timeline import RepairResult, TimelineReport


def _round(value, digits=3):
    return None if value is None else round(float(value), digits)


def build_payload(
    source: str,
    stats: FrameStats,
    report: TimelineReport,
    repair: RepairResult | None,
    truncated: bool,
    exit_code: int,
    fps_override: float | None,
) -> dict:
    """Assemble the canonical, schema-versioned result document."""

    max_jump_ms = (
        repair.max_jump_ticks * _tick_ms(stats.time_base)
        if repair is not None
        else None
    )
    payload = {
        "schema_version": SCHEMA_VERSION,
        "source": str(source),
        "status": _status_name(exit_code),
        "exit_code": exit_code,
        "truncated": truncated,
        "fps_override": fps_override,
        "stats": stats.as_dict(),
        "anomalies": {
            "counts": report.counts,
            "total": report.total,
            "nominal_step_ticks": report.nominal_step_ticks,
            "tolerance_ticks": report.tolerance_ticks,
            "events": [
                {
                    "kind": event.kind,
                    "display_index": event.display_index,
                    "observed_pts": event.observed_pts,
                    "detail": event.detail,
                    "value": event.value,
                }
                for event in report.events
            ],
        },
    }
    if repair is not None:
        payload["repair"] = {
            "repaired": repair.repaired_count,
            "max_jump_ticks": repair.max_jump_ticks,
            "max_jump_ms": _round(max_jump_ms),
            "max_jump_index": repair.max_jump_index,
            # idempotent_pass reports how many repairs a second pass over
            # the rebuilt axis would still perform; 0 proves idempotency.
            "idempotent_pass": repair.second_pass_repairs,
            "idempotent": bool(repair.idempotent_pass),
            "second_pass_repairs": repair.second_pass_repairs,
        }
    return payload


def _tick_ms(time_base_text: str) -> float:
    numerator, denominator = time_base_text.split("/")
    return float(numerator) / float(denominator) * 1000.0


def _status_name(exit_code: int) -> str:
    return {
        0: "clean",
        2: "repaired",
        3: "unrecoverable",
        4: "usage_error",
    }.get(exit_code, "unknown")


def render_summary(payload: dict) -> str:
    """One-line-per-concern terminal summary, no GUI dependencies."""

    stats = payload["stats"]
    anomalies = payload["anomalies"]
    lines = [
        f"source={payload['source']}",
        (
            f"decoded_frames={stats['decoded_frames']} "
            f"I={stats['i_frames']} P={stats['p_frames']} B={stats['b_frames']} "
            f"gops={stats['gop_count']} fps={_fmt(stats['nominal_fps'])} "
            f"tb={stats['time_base']}"
        ),
        (
            f"max_gop={stats['max_gop_size']} "
            f"dropped={stats['dropped_packets']} "
            f"measured_fps={_fmt(stats['measured_fps'])}"
        ),
        (
            f"first={_fmt(stats['first_time_ms'])}ms "
            f"last={_fmt(stats['last_time_ms'])}ms"
        ),
        f"anomaly_total={anomalies['total']} counts={anomalies['counts']}",
        f"order_violations={stats['order_violations']} "
        f"inferred_pts={stats['inferred_pts']} "
        f"truncated={payload['truncated']}",
    ]
    if "repair" in payload:
        repair = payload["repair"]
        lines.append(
            f"repaired={repair['repaired']} "
            f"max_jump_ms={_fmt(repair['max_jump_ms'])} "
            f"max_jump_ticks={repair['max_jump_ticks']} "
            f"idempotent_pass={repair['idempotent_pass']}"
        )
    lines.append(f"status={payload['status']} exit_code={payload['exit_code']}")
    return "\n".join(lines)


def _fmt(value) -> str:
    if value is None:
        return "n/a"
    if isinstance(value, float):
        return f"{value:.3f}"
    return str(value)


def render_json(payload: dict) -> str:
    return json.dumps(payload, indent=2, sort_keys=False, ensure_ascii=False)


def write_json(payload: dict, path: str | Path) -> None:
    Path(path).write_text(render_json(payload) + "\n", encoding="utf-8")


def write_repair_csv(
    path: str | Path,
    display_pts: list[int],
    repaired_pts: list[int],
    pict_types: list[str],
    time_base_text: str,
) -> int:
    """Write per-frame CSV; returns the number of data rows."""

    tick_ms = _tick_ms(time_base_text)
    output = io.StringIO()
    writer = csv.writer(output, lineterminator="\n")
    writer.writerow([
        "display_index", "pict_type", "original_pts", "repaired_pts",
        "original_ms", "repaired_ms", "delta_ticks",
    ])
    for index, (original, fixed, pict_type) in enumerate(
        zip(display_pts, repaired_pts, pict_types)
    ):
        writer.writerow([
            index, pict_type, original, fixed,
            _round(original * tick_ms), _round(fixed * tick_ms),
            fixed - original,
        ])
    Path(path).write_text(output.getvalue(), encoding="utf-8")
    return len(display_pts)
