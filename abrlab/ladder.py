"""Measured-point Pareto ladders and independent verification."""

from __future__ import annotations

import json
import math
from dataclasses import asdict, dataclass
from typing import Any

from .codec import decode
from .metrics import evaluate
from .plan import write_json_atomic
from .source import generate_reference, generate_sequence, resize_sequence
from .store import Store


class LadderError(ValueError):
    """Raised when a ladder cannot be built or is infeasible."""


@dataclass
class Point:
    task_key: str
    name: str
    resolution: str
    bitrate_kbps: float
    ssim: float
    psnr_db: float
    source_json: str
    params_json: str

    def level(self, monotone: bool) -> dict[str, Any]:
        return {
            "task_key": self.task_key,
            "name": self.name,
            "resolution": self.resolution,
            "bitrate_kbps": round(self.bitrate_kbps, 3),
            "ssim": round(self.ssim, 6),
            "psnr_db": round(self.psnr_db, 6),
            "monotone": monotone,
        }


def _point_from_row(row: Any) -> Point:
    source = json.loads(row["source_json"])
    return Point(
        task_key=str(row["task_key"]),
        name=str(row["name"]),
        resolution=f"{int(source['width'])}x{int(source['height'])}",
        bitrate_kbps=float(row["bitrate_kbps"]),
        ssim=float(row["ssim"]),
        psnr_db=float(row["psnr_db"]),
        source_json=str(row["source_json"]),
        params_json=str(row["params_json"]),
    )


def _dominated(point: Point, pool: list[Point]) -> bool:
    for other in pool:
        if other.task_key == point.task_key:
            continue
        if (other.bitrate_kbps <= point.bitrate_kbps and other.ssim >= point.ssim and
                (other.bitrate_kbps < point.bitrate_kbps or other.ssim > point.ssim)):
            return True
    return False


def _merge_frontier(frontier: list[Point]) -> tuple[list[Point], list[dict[str, Any]]]:
    kept: list[Point] = []
    dropped: list[dict[str, Any]] = []
    for point in frontier:
        if not kept:
            kept.append(point)
            continue
        previous = kept[-1]
        quality_delta = point.ssim - previous.ssim
        relative_bitrate = (point.bitrate_kbps - previous.bitrate_kbps) / max(previous.bitrate_kbps, 1e-9)
        reason = None
        if quality_delta < 0.005:
            dropped.append({"task_key": point.task_key, "name": point.name,
                            "reason": "quality_delta_below_0.005", "merged_into": previous.task_key})
            continue
        if relative_bitrate < 0.02:
            reason = "bitrate_delta_below_2pct"
            kept.pop()
            dropped.append({"task_key": previous.task_key, "name": previous.name,
                            "reason": reason, "merged_into": point.task_key})
            kept.append(point)
            continue
        kept.append(point)
    return kept, dropped


def _adaptive_select(points: list[Point], max_rungs: int) -> tuple[list[Point], list[dict[str, Any]]]:
    if len(points) <= max_rungs:
        return points, []
    if max_rungs == 1:
        selected = [points[-1]]
        dropped = [
            {"task_key": point.task_key, "name": point.name, "reason": "max_rungs_limit"}
            for point in points[:-1]
        ]
        return selected, dropped
    selected_indices = {0, len(points) - 1}
    if max_rungs > 2:
        quality_low = points[0].ssim
        quality_high = points[-1].ssim
        for slot in range(1, max_rungs - 1):
            target = quality_low + (quality_high - quality_low) * slot / (max_rungs - 1)
            available = [index for index in range(len(points)) if index not in selected_indices]
            best = min(available, key=lambda index: (abs(points[index].ssim - target),
                                                     points[index].bitrate_kbps))
            selected_indices.add(best)
    selected = [points[index] for index in sorted(selected_indices)]
    dropped = [
        {"task_key": point.task_key, "name": point.name, "reason": "max_rungs_limit"}
        for index, point in enumerate(points) if index not in selected_indices
    ]
    return selected, dropped


def build_ladder(
    rows: list[Any],
    *,
    max_rungs: int = 4,
    max_bitrate: float = 1800.0,
    min_quality: float = 0.86,
) -> dict[str, Any]:
    if not 1 <= max_rungs <= 32:
        raise LadderError("max_rungs must be between 1 and 32")
    if not math.isfinite(max_bitrate) or max_bitrate <= 0:
        raise LadderError("max_bitrate must be positive")
    if not math.isfinite(min_quality) or not 0.0 <= min_quality <= 1.0:
        raise LadderError("min_quality must be between 0 and 1")
    all_points = [_point_from_row(row) for row in rows]
    feasible = [
        point for point in all_points
        if point.bitrate_kbps <= max_bitrate and point.ssim >= min_quality
    ]
    dropped: list[dict[str, Any]] = [
        {"task_key": point.task_key, "name": point.name, "reason": "constraint_filter"}
        for point in all_points if point not in feasible
    ]
    dropped.extend(
        {"task_key": point.task_key, "name": point.name, "reason": "pareto_dominated"}
        for point in feasible if _dominated(point, feasible)
    )
    frontier = sorted(
        [point for point in feasible if not _dominated(point, feasible)],
        key=lambda point: (point.bitrate_kbps, point.ssim),
    )
    if not frontier:
        raise LadderError("no measured point satisfies max-bitrate and min-quality")
    frontier, merge_dropped = _merge_frontier(frontier)
    dropped.extend(merge_dropped)
    selected, limit_dropped = _adaptive_select(frontier, max_rungs)
    dropped.extend(limit_dropped)
    monotone = all(
        selected[index].bitrate_kbps > selected[index - 1].bitrate_kbps and
        selected[index].ssim >= selected[index - 1].ssim - 0.002
        for index in range(1, len(selected))
    )
    if not monotone:
        raise LadderError("frontier merging produced a non-monotone ladder")
    return {
        "version": 1,
        "levels": [point.level(monotone) for point in selected],
        "dropped": dropped,
        "monotone": monotone,
        "constraints": {
            "max_rungs": max_rungs,
            "max_bitrate": max_bitrate,
            "min_quality": min_quality,
        },
        "measured_points": len(all_points),
        "feasible_points": len(feasible),
        "pareto_points": len(frontier),
    }


def write_ladder(path: str, ladder: dict[str, Any]) -> None:
    write_json_atomic(path, ladder)


def _finite_number(value: Any) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    number = float(value)
    return number if math.isfinite(number) else None


def verify_ladder(store: Store, ladder: dict[str, Any]) -> dict[str, Any]:
    levels = ladder.get("levels") if isinstance(ladder, dict) else None
    if not isinstance(levels, list) or not levels:
        raise LadderError("ladder must contain a non-empty levels list")
    violations: list[dict[str, Any]] = []
    checked = 0
    previous: dict[str, Any] | None = None
    constraints = ladder.get("constraints", {})
    if not isinstance(constraints, dict):
        constraints = {}
    max_bitrate = _finite_number(constraints.get("max_bitrate", float("inf")))
    min_quality = _finite_number(constraints.get("min_quality", -1.0))
    max_bitrate = float("inf") if max_bitrate is None else max_bitrate
    min_quality = -1.0 if min_quality is None else min_quality
    for index, level in enumerate(levels):
        if not isinstance(level, dict):
            violations.append({"level": index, "code": "E_LEVEL_TYPE"})
            continue
        key = level.get("task_key")
        if not isinstance(key, str):
            violations.append({"level": index, "code": "E_TASK_KEY"})
            continue
        row = store.get_result(key)
        if row is None or row["status"] != "done" or row["payload"] is None:
            violations.append({"level": index, "task_key": key, "code": "E_NOT_DONE"})
            continue
        checked += 1
        bitrate = _finite_number(level.get("bitrate_kbps"))
        ssim = _finite_number(level.get("ssim"))
        psnr_db = _finite_number(level.get("psnr_db"))
        if bitrate is None or ssim is None or psnr_db is None:
            violations.append({"level": index, "task_key": key, "code": "E_INVALID_METRIC"})
            continue
        try:
            source = json.loads(row["source_json"])
            params = json.loads(row["params_json"])
            payload = bytes(row["payload"])
            duration = float(source["frames"]) / float(source["fps"])
            recomputed_bitrate = len(payload) * 8.0 / duration / 1000.0
            reconstructed = decode(payload)
            reference = generate_reference(source, str(params["chroma"]))
            compared = resize_sequence(
                reconstructed, int(reference.shape[2]), int(reference.shape[1])
            )
            recomputed_metrics = evaluate(reference, compared)
        except Exception as exc:
            violations.append({"level": index, "task_key": key,
                               "code": "E_RECOMPUTE_FAILED", "message": str(exc)[:300]})
            continue
        if abs(bitrate - recomputed_bitrate) > max(0.05, recomputed_bitrate * 0.005):
            violations.append({"level": index, "task_key": key, "code": "E_BITRATE_MISMATCH",
                               "expected": round(recomputed_bitrate, 6), "actual": bitrate})
        if abs(bitrate - float(row["bitrate_kbps"])) > max(0.05, recomputed_bitrate * 0.005):
            violations.append({"level": index, "task_key": key, "code": "E_DB_BITRATE_MISMATCH"})
        if abs(ssim - recomputed_metrics["ssim"]) > 0.002:
            violations.append({"level": index, "task_key": key, "code": "E_SSIM_MISMATCH",
                               "expected": recomputed_metrics["ssim"], "actual": ssim})
        if abs(psnr_db - recomputed_metrics["psnr_db"]) > 0.05:
            violations.append({"level": index, "task_key": key, "code": "E_PSNR_MISMATCH",
                               "expected": recomputed_metrics["psnr_db"], "actual": psnr_db})
        if bitrate > max_bitrate:
            violations.append({"level": index, "task_key": key, "code": "E_MAX_BITRATE"})
        if ssim < min_quality:
            violations.append({"level": index, "task_key": key, "code": "E_MIN_QUALITY"})
        if previous is not None:
            if bitrate <= float(previous["bitrate_kbps"]):
                violations.append({"level": index, "task_key": key, "code": "E_BITRATE_NOT_STRICT"})
            if ssim < float(previous["ssim"]) - 0.002:
                violations.append({"level": index, "task_key": key, "code": "E_QUALITY_DECREASE"})
        previous = {"bitrate_kbps": bitrate, "ssim": ssim}
    monotone = not any(
        violation["code"] in {"E_BITRATE_NOT_STRICT", "E_QUALITY_DECREASE"}
        for violation in violations
    )
    passed = monotone and not violations
    return {"pass": passed, "monotone": monotone, "violations": violations, "checked": checked}
