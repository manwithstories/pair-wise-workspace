"""Small numeric helpers shared by graph processors."""

from __future__ import annotations

import math

import numpy as np


def db_to_linear(db: float | np.ndarray) -> float | np.ndarray:
    return np.power(10.0, np.asarray(db, dtype=np.float64) / 20.0)


def linear_to_db(value: float, floor_db: float = -120.0) -> float:
    if not math.isfinite(value) or value <= 0.0:
        return floor_db
    return max(floor_db, 20.0 * math.log10(value))


def require_finite(name: str, value, *, field: str) -> float:
    from .errors import ManifestError

    try:
        number = float(value)
    except (TypeError, ValueError) as exc:
        raise ManifestError(f"must be a finite number, got {value!r}", field) from exc
    if not math.isfinite(number):
        raise ManifestError(f"must be finite, got {value!r}", field)
    return number

