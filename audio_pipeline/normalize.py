"""Loudness gain planning with true-peak backoff."""

from __future__ import annotations

from dataclasses import asdict, dataclass
import math

import numpy as np

from .loudness import DB_FLOOR


DEFAULT_TARGET_LUFS = -16.0
DEFAULT_MAX_GAIN_DB = 12.0
DEFAULT_TRUE_PEAK_DBTP = -1.0
MAX_BACKOFF_ROUNDS = 5


@dataclass(frozen=True)
class GainRound:
    round: int
    gain_db: float
    predicted_true_peak_dbtp: float
    action: str


@dataclass(frozen=True)
class GainPlan:
    gain_db: float
    target_lufs: float
    requested_gain_db: float
    limited_by_gain_cap: bool
    limited_by_true_peak: bool
    rounds: tuple[GainRound, ...]

    def to_dict(self) -> dict:
        result = asdict(self)
        result["rounds"] = [asdict(round_) for round_ in self.rounds]
        return result


def _round(value: float) -> float:
    return round(float(value), 9)


def compute_gain(
    loudness_before: float,
    true_peak_dbtp: float,
    target_lufs: float = DEFAULT_TARGET_LUFS,
    max_gain_db: float = DEFAULT_MAX_GAIN_DB,
    true_peak_limit: float = DEFAULT_TRUE_PEAK_DBTP,
    max_rounds: int = MAX_BACKOFF_ROUNDS,
) -> GainPlan:
    """Return a deterministic gain plan; silence remains untouched."""

    if not math.isfinite(target_lufs) or not math.isfinite(max_gain_db):
        raise ValueError("normalization parameters must be finite")
    if max_gain_db < 0.0:
        raise ValueError("max_gain_db must be non-negative")
    if max_rounds < 1 or max_rounds > MAX_BACKOFF_ROUNDS:
        raise ValueError(f"max_rounds must be in 1..{MAX_BACKOFF_ROUNDS}")
    if true_peak_limit >= 0.0:
        raise ValueError("true_peak_limit must be negative")

    if loudness_before <= DB_FLOOR + 1e-9:
        plan = GainPlan(0.0, target_lufs, 0.0, False, False, ())
        return plan

    requested = target_lufs - loudness_before
    capped = requested > max_gain_db
    gain = min(requested, max_gain_db)
    rounds: list[GainRound] = []
    limited = False
    for round_index in range(1, max_rounds + 1):
        predicted = true_peak_dbtp + gain
        if predicted <= true_peak_limit + 1e-9:
            rounds.append(
                GainRound(round_index, _round(gain), _round(predicted), "accepted")
            )
            break
        limited = True
        rounds.append(
            GainRound(
                round_index,
                _round(gain),
                _round(predicted),
                "true_peak_backoff",
            )
        )
        gain = true_peak_limit - true_peak_dbtp - 1e-6
    else:
        # The linear relation guarantees convergence in round two in normal
        # operation.  This clamp protects malformed/non-finite measurements.
        gain = min(gain, true_peak_limit - true_peak_dbtp - 1e-6)
        if rounds:
            rounds[-1] = GainRound(
                rounds[-1].round,
                _round(gain),
                _round(true_peak_dbtp + gain),
                "final_clamp",
            )

    return GainPlan(
        gain_db=_round(gain),
        target_lufs=float(target_lufs),
        requested_gain_db=_round(requested),
        limited_by_gain_cap=bool(capped),
        limited_by_true_peak=bool(limited),
        rounds=tuple(rounds),
    )


def gain_amplitude(gain_db: float) -> float:
    return float(10.0 ** (gain_db / 20.0))


def apply_gain(block: np.ndarray, gain_db: float) -> np.ndarray:
    return np.asarray(block, dtype=np.float64) * gain_amplitude(gain_db)
