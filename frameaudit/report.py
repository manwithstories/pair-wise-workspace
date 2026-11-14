"""Terminal renderers for command reports."""

from __future__ import annotations

from typing import Any


def scan_text(report: dict[str, Any]) -> str:
    return "\n".join(
        [
            f"input: {report['input']}",
            f"container: {report['container']['format']} / {report['container']['codec']}",
            f"fps: nominal={report['container']['nominal_fps']} actual={report['container']['actual_fps']} vfr={report['vfr']}",
            f"frames: {report['decoded_frames']} (I={report['i_frames']} P={report['p_frames']} B={report['b_frames']})",
            f"keyframes: {report['keyframe_count']}",
            f"frame interval: mean={report['frame_interval_mean_ms']}ms peak={report['frame_interval_peak_ms']}ms",
            f"bad regions: {len(report['bad_regions'])}",
        ]
    )


def repair_text(report: dict[str, Any]) -> str:
    categories = report["timestamp_categories"]
    category_text = ", ".join(f"{key}={value}" for key, value in categories.items())
    return "\n".join(
        [
            f"input: {report['input']}",
            f"frames: {report['decoded_frames']} index_rows={report['index_rows']} proxy_frames={report['proxy_frames']}",
            f"repaired: {report['repaired_count']} ({category_text})",
            f"jump: before={report['max_jump_before_ms']}ms after={report['max_jump_after_ms']}ms",
            f"monotonic: {report['fixed_monotonic']} clock={report['clock_ms']}ms",
            f"bad regions: {len(report['bad_regions'])}",
        ]
    )


def fixtures_text(report: dict[str, Any]) -> str:
    files = report["files"]
    return "\n".join(
        [
            f"seed: {report['seed']} frames: {report['frames']}",
            f"sample: {files['vfr_bpyramid']}",
            f"truncated: {files['truncated_packet']}",
            f"injected: {report['injected']}",
        ]
    )


def verify_text(report: dict[str, Any]) -> str:
    state = "OK" if report["ok"] else "MISMATCH"
    return "\n".join(
        [
            f"verify: {state}",
            f"frames: index={report['index_rows']} proxy={report['proxy_frames']}",
            f"pts: first={report['first_pts_us']} last={report['last_pts_us']}",
            f"monotonic: index={report['index_monotonic']} proxy={report['proxy_monotonic']}",
            f"hash mismatches: {report['hash_mismatches']}",
            *[f"error: {message}" for message in report["errors"][:10]],
        ]
    )

