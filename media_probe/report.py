"""Assemble the JSON manifest and print the terminal summary."""

from __future__ import annotations

import json
import os
import sys

from .framewalk import WalkResult
from .probe import ProbeResult
from .repair import RepairResult


def build_manifest(
    mode: str,
    probe: ProbeResult,
    walk: WalkResult,
    repair: RepairResult,
    type_counts: dict,
    gops: list[int],
    rate: dict,
) -> dict:
    """One manifest shape for both scan and repair (identical timeline)."""
    return {
        "mode": mode,
        "input": probe.path,
        "probe": {
            "codec": probe.codec,
            "width": probe.width,
            "height": probe.height,
            "time_base": f"{probe.time_base.numerator}/{probe.time_base.denominator}",
            "stream_index": probe.stream_index,
            "nominal_fps_hint": probe.nominal_fps,
            "duration_sec": probe.duration_sec,
        },
        "frames": {
            "decoded_frames": walk.frames_decoded,
            "packets": walk.packets_seen,
            **type_counts,
            "gop_lengths": gops,
            "gop_max": max(gops) if gops else 0,
        },
        "timeline": {
            "interval_ticks": repair.interval_ticks,
            "nominal_fps": rate["nominal_fps"],
            "vfr": rate["vfr"],
            "vfr_score": rate["vfr_score"],
            "confidence": rate["confidence"],
            "repaired_count": repair.repaired_count,
            "repair_events": repair.events,
            "jumps": {
                "threshold_intervals": repair.jump_threshold,
                "count": len(repair.jumps),
                "max_jump_ms": round(repair.max_jump_ms, 3),
                "positions": [j.frame_index for j in repair.jumps],
            },
        },
        "warnings": list(walk.warnings),
        "missing_intervals": [list(m) for m in walk.missing_intervals],
    }


def write_json(manifest: dict, path: str) -> None:
    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, exist_ok=True)
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(manifest, handle, indent=2, ensure_ascii=False)
        handle.write("\n")


def print_summary(manifest: dict, stream=None) -> None:
    """Compact aligned summary table on the terminal."""
    stream = stream or sys.stdout
    frames = manifest["frames"]
    timeline = manifest["timeline"]
    jumps = timeline["jumps"]
    rows = [
        ("input", manifest["input"]),
        ("mode", manifest["mode"]),
        ("codec", f"{manifest['probe']['codec']}  tb={manifest['probe']['time_base']}"),
        ("decoded_frames", frames["decoded_frames"]),
        (
            "I/P/B",
            f"{frames['i_frames']}/{frames['p_frames']}/{frames['b_frames']}"
            f"  (inferred={frames['pict_type_inferred']})",
        ),
        ("gop_max", frames["gop_max"]),
        ("nominal_fps", timeline["nominal_fps"]),
        ("vfr", f"{timeline['vfr']}  (score={timeline['vfr_score']})"),
        ("repaired_count", timeline["repaired_count"]),
        ("repair_events", timeline["repair_events"]),
        ("jumps", f"{jumps['count']}  max={jumps['max_jump_ms']}ms"),
        ("warnings", len(manifest["warnings"])),
    ]
    width = max(len(key) for key, _ in rows)
    print("-" * 48, file=stream)
    for key, value in rows:
        print(f"  {key:<{width}} : {value}", file=stream)
    print("-" * 48, file=stream)
