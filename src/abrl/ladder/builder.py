"""Constrained ladder search, monotonic repair and infeasibility diagnosis."""
from __future__ import annotations

from dataclasses import dataclass

REDUNDANCY_DB = 0.4
_QUALITY_EPS = 1e-9


@dataclass
class LadderResult:
    feasible: bool
    rungs: list[dict]
    reason: str | None
    closest_feasible_count: int
    checks: dict
    constraints: dict | None = None

    def as_dict(self) -> dict:
        return {
            "feasible": self.feasible,
            "constraints": self.constraints or {},
            "rungs": self.rungs,
            "reason": self.reason,
            "closest_feasible_count": self.closest_feasible_count,
            "checks": self.checks,
        }


def _ratio_ok(low: dict, high: dict, min_ratio: float, max_ratio: float) -> bool:
    ratio = high["actual_kbps"] / low["actual_kbps"]
    return min_ratio <= ratio <= max_ratio


def _pair_valid(low: dict, high: dict, min_ratio: float, max_ratio: float) -> bool:
    return (
        high["actual_kbps"] > low["actual_kbps"]
        and high["psnr_db"] - low["psnr_db"] >= REDUNDANCY_DB - _QUALITY_EPS
        and _ratio_ok(low, high, min_ratio, max_ratio)
    )


def _search(candidates: list[dict], rungs: int, min_ratio: float, max_ratio: float,
            max_kbps: float) -> list[dict] | None:
    """DP over bitrate-sorted candidates.

    Adjacent rungs must gain at least REDUNDANCY_DB of PSNR, so a feasible
    chain never contains a redundant pair. Among feasible chains prefer the
    highest top-rung quality, then the highest quality sum (each rung as
    good as possible), then the cheapest top rung.
    """
    usable = sorted(
        (row for row in candidates if row["actual_kbps"] <= max_kbps),
        key=lambda row: row["actual_kbps"],
    )
    n = len(usable)
    if n < rungs:
        return None
    best: dict[tuple[int, int], tuple[float, list]] = {}
    for index, row in enumerate(usable):
        best[(1, index)] = (row["psnr_db"], [index])
    for length in range(2, rungs + 1):
        for j in range(length - 1, n):
            high = usable[j]
            best_state = None
            for i in range(length - 2, j):
                prior = best.get((length - 1, i))
                if prior is None or not _pair_valid(usable[i], high, min_ratio, max_ratio):
                    continue
                state = (prior[0] + high["psnr_db"], prior[1] + [j])
                if best_state is None or state[0] > best_state[0]:
                    best_state = state
            if best_state is not None:
                best[(length, j)] = best_state
    finals = [
        (j, state) for (length, j), state in best.items() if length == rungs
    ]
    if not finals:
        return None

    def _rank(entry):
        j, state = entry
        top = usable[j]
        return (top["psnr_db"], state[0], -top["actual_kbps"])

    indices = max(finals, key=_rank)[1][1]
    return [usable[i] for i in indices]


def _merge_redundant(rungs: list[dict]) -> list[dict]:
    """Drop a rung whose PSNR gain over its predecessor is below 0.4 dB."""
    if len(rungs) <= 1:
        return list(rungs)
    merged = [rungs[0]]
    for rung in rungs[1:]:
        if rung["psnr_db"] - merged[-1]["psnr_db"] < REDUNDANCY_DB - _QUALITY_EPS:
            continue
        merged.append(rung)
    return merged


def evaluate(rungs: list[dict], min_ratio: float, max_ratio: float, max_kbps: float) -> dict:
    bitrate_ok = all(hi["actual_kbps"] > lo["actual_kbps"] for lo, hi in zip(rungs, rungs[1:]))
    quality_ok = all(hi["psnr_db"] + _QUALITY_EPS >= lo["psnr_db"] for lo, hi in zip(rungs, rungs[1:]))
    ratio_ok = all(_ratio_ok(lo, hi, min_ratio, max_ratio) for lo, hi in zip(rungs, rungs[1:]))
    cap_ok = all(rung["actual_kbps"] <= max_kbps for rung in rungs)
    return {
        "bitrate_strictly_increasing": bool(bitrate_ok),
        "quality_nondecreasing": bool(quality_ok),
        "ratio_in_range": bool(ratio_ok),
        "under_max_kbps": bool(cap_ok),
    }


def _closest_count(candidates, min_ratio, max_ratio, max_kbps, wanted):
    for count in range(wanted - 1, 0, -1):
        if _search(candidates, count, min_ratio, max_ratio, max_kbps) is not None:
            return count
    return 0


def build_ladder(candidates: list[dict], rungs: int, max_kbps: float,
                 min_ratio: float, max_ratio: float) -> LadderResult:
    if rungs < 2:
        raise ValueError("ladder needs at least 2 rungs")
    if min_ratio <= 1 or max_ratio < min_ratio:
        raise ValueError("require 1 < min_ratio <= max_ratio")
    constraints = {
        "rungs": rungs,
        "max_kbps": max_kbps,
        "min_ratio": min_ratio,
        "max_ratio": max_ratio,
    }
    ordered = sorted(candidates, key=lambda row: row["actual_kbps"])
    chosen = _search(ordered, rungs, min_ratio, max_ratio, max_kbps)
    if chosen is None:
        closest = _closest_count(ordered, min_ratio, max_ratio, max_kbps, rungs)
        reason = (
            f"no {rungs}-rung ladder satisfies ratio [{min_ratio}, {max_ratio}], "
            f"max_kbps={max_kbps}; closest feasible rung count={closest}"
        )
        return LadderResult(False, [], reason, closest,
                            evaluate([], min_ratio, max_ratio, max_kbps), constraints)
    merged = _merge_redundant(chosen)
    checks = evaluate(merged, min_ratio, max_ratio, max_kbps)
    if all(checks.values()):
        return LadderResult(True, merged, None, len(merged), checks, constraints)
    closest = _closest_count(ordered, min_ratio, max_ratio, max_kbps, rungs)
    reason = (
        f"merge/repair left an invalid ladder under ratio [{min_ratio}, {max_ratio}] "
        f"and max_kbps={max_kbps}; closest feasible rung count={closest}"
    )
    return LadderResult(False, merged, reason, closest, checks, constraints)
