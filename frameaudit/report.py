"""Terminal summaries and JSON statistics derived from one timeline."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .decode import DecodeResult
from .repair import RepairResult
from .reorder import ReorderResult
from .timeline import TimelineFrame


def build_stats(
    decode: DecodeResult,
    reorder: ReorderResult,
    repair: RepairResult,
    timeline: list[TimelineFrame],
    *,
    nominal_fps: str,
) -> dict[str, Any]:
    pict_type = {"I": 0, "P": 0, "B": 0, "other": 0}
    for frame in timeline:
        pict_type[frame.pict_type] = pict_type.get(frame.pict_type, 0) + 1
    return {
        "decoded_frames": len(timeline),
        "pict_type": pict_type,
        "keyframes": sum(frame.keyframe for frame in timeline),
        "packets": decode.packets,
        "packet_frame_delta": decode.packet_frame_delta,
        "decode_errors": decode.decode_errors,
        "reordered_frames": reorder.reordered_frames,
        "max_reorder_depth": reorder.max_reorder_depth,
        "repaired_timestamps": repair.anomaly_count,
        "max_jump_ms": repair.max_jump_ms,
        "first_pts_ms": repair.first_pts_ms,
        "monotonic": (
            repair.monotonic if repair.strategy != "scan" else repair.source_monotonic
        ),
        "residual_max_jump_ms": repair.residual_max_jump_ms,
        "strategy": repair.strategy,
        "synthetic": bool(timeline) and all(frame.synthetic for frame in timeline),
        "nominal_fps": nominal_fps,
        "time_base": f"{decode.time_base.numerator}/{decode.time_base.denominator}",
        "codec": decode.codec_name,
        "width": decode.width,
        "height": decode.height,
        "duplicates": repair.duplicate_count,
        "rollbacks": repair.rollback_count,
        "jumps": repair.jump_count,
        "rounding_duplicates": repair.rounding_duplicate_count,
    }


def write_json(path: str | Path, payload: dict[str, Any]) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )


def terminal_summary(stats: dict[str, Any], *, title: str = "FrameAudit") -> str:
    types = stats["pict_type"]
    lines = [
        f"{title}",
        f"frames={stats['decoded_frames']} packets={stats['packets']} "
        f"packet_frame_delta={stats['packet_frame_delta']} "
        f"decode_errors={stats['decode_errors']}",
        f"I={types['I']} P={types['P']} B={types['B']} other={types['other']} "
        f"keyframes={stats['keyframes']}",
        f"reordered={stats['reordered_frames']} "
        f"max_reorder_depth={stats['max_reorder_depth']}",
        f"anomalies={stats['repaired_timestamps']} max_jump_ms={stats['max_jump_ms']} "
        f"first_pts_ms={stats['first_pts_ms']} monotonic={str(stats['monotonic']).lower()}",
        f"strategy={stats['strategy']} residual_max_jump_ms="
        f"{stats['residual_max_jump_ms']}",
        f"synthetic={str(stats['synthetic']).lower()} fps={stats['nominal_fps']} "
        f"time_base={stats['time_base']}",
    ]
    return "\n".join(lines)


def json_text(payload: dict[str, Any]) -> str:
    return json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
