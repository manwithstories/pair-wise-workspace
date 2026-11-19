"""Counter unwrap and acquisition-clock drift estimation.

Two operations live here:

* :func:`unwrap_counter` expands a width-limited counter (8/16/32/64-bit) into
  a monotonic integer series. A jump whose shortest explanation still spans
  more than half of the counter modulus is consistent with several wrap counts
  and therefore ambiguous — it is rejected instead of guessed.
* :func:`fit_drift` fits the unwrapped acquisition clock to reference time as
  an exact rational linear map and retains every fitting residual.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

from .model import AmbiguousWrapError, MediaMixError, TICKS_PER_SECOND


@dataclass
class UnwrapResult:
    """Monotonic counter series plus the number of wraps reconstructed."""

    values: list[int]
    wraps: int
    modulus: int


@dataclass
class DriftModel:
    """Exact rational map ``reference_tick = slope*clock_tick + intercept``.

    ``slope``/``intercept`` are exact :class:`Fraction` values; residuals are
    kept both as exact fractions (ticks) and summarized in parts-per-million.
    """

    slope: Fraction
    intercept: Fraction
    n: int
    residuals: list[Fraction] = field(default_factory=list)
    max_abs_residual_ticks: int = 0
    rms_residual_ticks: float = 0.0
    drift_ppm: float = 0.0  # (slope - clock_rate_scale) in ppm, <0 means slow

    def to_reference(self, clock_tick: int) -> int:
        """Map one unwrapped acquisition tick to a master-clock tick.

        Rounds half up (toward positive infinity).
        """
        value = self.slope * clock_tick + self.intercept
        whole, rem = divmod(value.numerator, value.denominator)
        if rem * 2 >= value.denominator:
            whole += 1
        return whole

    def clock_tick_to_seconds(self, clock_tick: int) -> float:
        return self.to_reference(clock_tick) / TICKS_PER_SECOND

    def as_dict(self) -> dict:
        return {
            "slope_num": self.slope.numerator,
            "slope_den": self.slope.denominator,
            "intercept_num": self.intercept.numerator,
            "intercept_den": self.intercept.denominator,
            "n": self.n,
            "max_abs_residual_ticks": self.max_abs_residual_ticks,
            "rms_residual_ticks": self.rms_residual_ticks,
            "drift_ppm": self.drift_ppm,
        }


def unwrap_counter(raw_values: list[int], bits: int, *, path: str = "", frame_seqs: list[int] | None = None) -> UnwrapResult:
    """Expand wrapped counter readings into a monotonic integer series.

    Any single-step jump whose magnitude exceeds half the counter modulus is
    ambiguous — zero wraps (a genuine large gap) and one wrap (a small step
    across the boundary) both explain it — so it raises
    :class:`AmbiguousWrapError`. A backward jump within half the modulus is
    uniquely explained by exactly one wrap.
    """
    if not 1 <= bits <= 64:
        raise MediaMixError(f"counter width must be in 1..64, got {bits}", path=path)
    modulus = 1 << bits
    half = modulus // 2
    if not raw_values:
        return UnwrapResult(values=[], wraps=0, modulus=modulus)

    out: list[int] = []
    wraps = 0
    prev = raw_values[0]
    out.append(prev)
    for i in range(1, len(raw_values)):
        cur = raw_values[i]
        if not 0 <= cur < modulus:
            seq = frame_seqs[i] if frame_seqs is not None else None
            raise MediaMixError(
                f"counter value {cur} outside {bits}-bit range [0,{modulus})",
                path=path, frame_seq=seq,
            )
        delta = cur - prev
        seq = frame_seqs[i] if frame_seqs is not None else None
        if delta < 0:
            # Backward reading: a wrap whose forward-equivalent gap must be at
            # most half the modulus for one wrap to be the unique explanation.
            forward_gap = delta + modulus
            if forward_gap > half:
                raise AmbiguousWrapError(
                    f"counter jump {delta} spans more than half the {bits}-bit "
                    f"modulus; multiple wraps are indistinguishable",
                    path=path, frame_seq=seq,
                )
            wraps += 1
            out.append(out[-1] + forward_gap)
        else:
            # A forward jump beyond half the modulus cannot distinguish "no
            # wrap, huge gap" from "one wrap, small step" either.
            if delta > half:
                raise AmbiguousWrapError(
                    f"counter jump {delta} spans more than half the {bits}-bit "
                    f"modulus; wrap count cannot be determined",
                    path=path, frame_seq=seq,
                )
            out.append(out[-1] + delta)
        prev = cur
    return UnwrapResult(values=out, wraps=wraps, modulus=modulus)


def fit_drift(
    clock_ticks: list[int],
    reference_ticks: list[int],
    clock_rate: int,
) -> DriftModel:
    """Least-squares linear fit of reference time over acquisition clock.

    Sums are computed with exact :class:`Fraction` arithmetic so the rational
    map is reproducible; only the residual summary statistics are float.
    """
    if len(clock_ticks) != len(reference_ticks):
        raise MediaMixError("clock and reference series have different lengths")
    n = len(clock_ticks)
    if n < 2:
        raise MediaMixError("drift fit needs at least two samples")
    if any(clock_ticks[i] >= clock_ticks[i + 1] for i in range(n - 1)):
        raise MediaMixError("unwrapped clock ticks must be strictly increasing")

    sx = sum(clock_ticks)
    sy = sum(reference_ticks)
    sxx = sum(x * x for x in clock_ticks)
    sxy = sum(x * y for x, y in zip(clock_ticks, reference_ticks))
    denom = n * sxx - sx * sx
    if denom == 0:
        raise MediaMixError("degenerate clock series: zero span")
    slope = Fraction(n * sxy - sy * sx, denom)
    intercept = Fraction(sy, n) - slope * Fraction(sx, n)

    model = DriftModel(slope=slope, intercept=intercept, n=n)
    # Ideal undrifted mapping: acquisition ticks -> master ticks.
    ideal = Fraction(TICKS_PER_SECOND, clock_rate)
    model.drift_ppm = float((slope - ideal) / ideal * 1_000_000)
    for x, y in zip(clock_ticks, reference_ticks):
        res = Fraction(y) - (slope * x + intercept)
        model.residuals.append(res)
    model.max_abs_residual_ticks = max(int(abs(r)) for r in model.residuals)
    model.rms_residual_ticks = (
        sum(float(r) ** 2 for r in model.residuals) / n
    ) ** 0.5
    return model
