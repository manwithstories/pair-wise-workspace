#!/usr/bin/env python3
"""Validate a transition_bake JSON report without third-party dependencies."""

from __future__ import annotations

import argparse
import json
import re
import statistics
from pathlib import Path

KERNELS = {
    "linear_blend",
    "threshold_mask",
    "channel_key",
    "geometry_resample",
    "block_permute",
}
HASH_RE = re.compile(r"^[0-9a-f]{16}$")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Check a bake report")
    parser.add_argument("--report", required=True)
    parser.add_argument("--strict", action="store_true")
    return parser.parse_args()


def is_int(value: object) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def check_smooth(class_name: str, curve: list[object], errors: list[str]) -> None:
    if len(curve) < 8 or not all(is_int(v) for v in curve):
        errors.append(f"{class_name}: invalid diff_curve")
        return
    values = [int(v) for v in curve]
    if values[0] != 0 or values[-1] != 0:
        errors.append(f"{class_name}: mixed curve endpoints are not zero")
    peak = max(range(len(values)), key=lambda index: values[index])
    if not len(values) // 4 <= peak <= (len(values) * 3) // 4:
        errors.append(f"{class_name}: mixed curve peak is not central")
    if values[peak] <= 1:
        errors.append(f"{class_name}: mixed curve has no visible peak")
    left = values[: peak + 1]
    right = values[peak:]
    if any(right[i] < right[i + 1] for i in range(len(right) - 1)):
        errors.append(f"{class_name}: mixed curve does not fall after its peak")
    if sum(1 for i in range(len(left) - 1) if left[i] > left[i + 1] + 1) > 2:
        errors.append(f"{class_name}: mixed curve is not broadly rising")


def check_hard(class_name: str, curve: list[object], errors: list[str]) -> None:
    if len(curve) < 8 or not all(is_int(v) for v in curve):
        errors.append(f"{class_name}: invalid diff_curve")
        return
    values = [int(v) for v in curve]
    if values[0] != 0 or values[-1] != 0:
        errors.append(f"{class_name}: hard curve endpoints are not zero")
    interior = values[2:-2]
    if not interior:
        errors.append(f"{class_name}: hard curve has no interior")
        return
    median = statistics.median(interior)
    tolerance = max(3, int(median * 2) + 1)
    stable = sum(1 for value in interior if abs(value - median) <= tolerance)
    if stable < int(len(interior) * 0.6):
        errors.append(f"{class_name}: hard curve is not plateau-like")


def validate(report: dict[str, object], strict: bool) -> list[str]:
    errors: list[str] = []
    width = report.get("width")
    height = report.get("height")
    if not is_int(width) or not is_int(height) or width <= 0 or height <= 0:
        errors.append("invalid width/height")
        pixel_limit = 1
    else:
        pixel_limit = int(width) * int(height)
    total = report.get("total_frames")
    command = report.get("command")
    if command == "selftest":
        total_ok = is_int(total) and total >= 0
    else:
        total_ok = is_int(total) and 100 <= total <= 10000
    if not total_ok:
        errors.append("total_frames is outside 100..10000")
    if (
        strict
        and command != "selftest"
        and not 120 <= int(total if is_int(total) else 0) <= 150
    ):
        errors.append("strict total_frames is not near 132")
    classes = report.get("classes")
    if not isinstance(classes, list) or len(classes) != 5:
        errors.append("classes must contain exactly five entries")
        classes = []
    seen = set()
    for item in classes:
        if not isinstance(item, dict):
            errors.append("class entry is not an object")
            continue
        name = item.get("class")
        if name not in KERNELS or name in seen:
            errors.append(f"invalid or duplicate class: {name}")
            continue
        seen.add(name)
        count = item.get("frame_count")
        if not is_int(count) or count <= 0:
            errors.append(f"{name}: invalid frame_count")
            count = 0
        elif strict and not 20 <= count <= 28:
            errors.append(f"{name}: frame_count is not near 24")
        for key in ("first_frame_hash", "last_frame_hash"):
            if not isinstance(item.get(key), str) or not HASH_RE.fullmatch(item[key]):
                errors.append(f"{name}: {key} is not a 16-digit lowercase hash")
        mean_diff = item.get("mean_abs_diff")
        if not is_int(mean_diff) or not 0 <= mean_diff <= 60:
            errors.append(f"{name}: mean_abs_diff is outside 0..60")
        max_diff = item.get("max_abs_diff")
        if not is_int(max_diff) or not 0 <= max_diff <= 255:
            errors.append(f"{name}: max_abs_diff is outside 0..255")
        changed = item.get("changed_pixels")
        if not is_int(changed) or not 0 <= changed <= pixel_limit:
            errors.append(f"{name}: changed_pixels is outside the frame")
        histogram = item.get("diff_histogram")
        if (
            not isinstance(histogram, list)
            or len(histogram) != 16
            or not all(is_int(value) and value >= 0 for value in histogram)
            or sum(histogram) <= 0
        ):
            errors.append(f"{name}: diff_histogram is invalid")
        peak = item.get("diff_peak_frame")
        if not is_int(peak) or not 0 <= peak < max(1, count):
            errors.append(f"{name}: diff_peak_frame is outside the class")
        curve = item.get("diff_curve")
        if not isinstance(curve, list):
            errors.append(f"{name}: diff_curve is missing")
            continue
        if count and len(curve) != count - 1:
            errors.append(f"{name}: diff_curve length does not match frame_count")
        kind = item.get("curve_kind")
        if name == "linear_blend":
            if kind != "single_peak":
                errors.append(f"{name}: curve_kind must be single_peak")
            check_smooth(name, curve, errors)
        else:
            if kind != "plateau":
                errors.append(f"{name}: curve_kind must be plateau")
            check_hard(name, curve, errors)
    if seen != KERNELS:
        errors.append("class set does not match the five registered kernels")
    if report.get("reproducible") is not True:
        errors.append("reproducible is not true")
    speed = report.get("speed_segments")
    if not is_int(speed) or not 6 <= speed <= 8:
        errors.append("speed_segments is outside 6..8")
    if report.get("duplicate_frames") != 0:
        errors.append("duplicate_frames is not zero")
    skipped = report.get("skipped_frames")
    if not is_int(skipped) or skipped != 0:
        errors.append("skipped_frames is not zero")
    if report.get("errors") != []:
        errors.append("errors is not an empty array")
    if strict:
        if report.get("ok") is not True:
            errors.append("strict report is not ok")
        if command == "verify":
            if report.get("observed_frames") != total:
                errors.append("observed_frames does not match total_frames")
    return errors


def main() -> int:
    args = parse_args()
    try:
        report = json.loads(Path(args.report).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        print(json.dumps({"ok": False, "errors": [str(error)]}, separators=(",", ":")))
        return 1
    if not isinstance(report, dict):
        errors = ["report root is not an object"]
    else:
        errors = validate(report, args.strict)
    print(
        json.dumps(
            {"ok": not errors, "strict": args.strict, "errors": errors},
            separators=(",", ":"),
        )
    )
    return 0 if not errors else 1


if __name__ == "__main__":
    raise SystemExit(main())
