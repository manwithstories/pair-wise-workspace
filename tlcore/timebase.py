"""Exact nanosecond and rational-second conversions.

No floating-point value is accepted at a time boundary.  All calculations use
:class:`fractions.Fraction`, then round to the nearest nanosecond (half away
from zero) at the public boundary.
"""

from __future__ import annotations

from fractions import Fraction
from typing import Any

NS_PER_SECOND = 1_000_000_000


class TimebaseError(ValueError):
    """A time or frame-rate value cannot be interpreted exactly."""


def _is_number(value: object) -> bool:
    return isinstance(value, (int, Fraction)) and not isinstance(value, bool)


def rational_seconds(num: int, den: int) -> Fraction:
    if isinstance(num, bool) or isinstance(den, bool):
        raise TimebaseError("num and den must be integers")
    if not isinstance(num, int) or not isinstance(den, int):
        raise TimebaseError("num and den must be integers")
    if den == 0:
        raise TimebaseError("denominator must not be zero")
    return Fraction(num, den)


def parse_rate(value: object, field: str = "fps") -> Fraction:
    """Parse a frame rate without allowing float rounding."""
    if isinstance(value, bool) or isinstance(value, float):
        raise TimebaseError(f"{field} must be an integer or num/den, not float")
    if isinstance(value, int):
        if value <= 0:
            raise TimebaseError(f"{field} must be positive")
        return Fraction(value, 1)
    if isinstance(value, Fraction):
        if value <= 0:
            raise TimebaseError(f"{field} must be positive")
        return value
    if isinstance(value, str):
        text = value.strip()
        try:
            if "/" in text:
                left, right = text.split("/", 1)
                result = rational_seconds(int(left), int(right))
            else:
                result = Fraction(int(text), 1)
        except (ValueError, ZeroDivisionError) as exc:
            raise TimebaseError(f"invalid {field}: {value!r}") from exc
        if result <= 0:
            raise TimebaseError(f"{field} must be positive")
        return result
    if isinstance(value, dict):
        if "num" not in value or "den" not in value:
            raise TimebaseError(f"{field} requires num and den")
        try:
            return rational_seconds(value["num"], value["den"])
        except TimebaseError:
            raise
        except Exception as exc:
            raise TimebaseError(f"invalid {field}: {value!r}") from exc
    raise TimebaseError(f"{field} must be an integer, string, or num/den object")


def seconds_to_ns(value: int | Fraction) -> int:
    """Convert exact seconds to integer nanoseconds, rounding halves away."""
    if isinstance(value, bool) or isinstance(value, float):
        raise TimebaseError("floats are not allowed; use an integer or Fraction")
    if not _is_number(value):
        raise TimebaseError("seconds must be an integer or Fraction")
    scaled = Fraction(value) * NS_PER_SECOND
    numerator, denominator = scaled.numerator, scaled.denominator
    if numerator >= 0:
        return (2 * numerator + denominator) // (2 * denominator)
    return -((2 * -numerator + denominator) // (2 * denominator))


def ns_to_seconds(ns: int) -> Fraction:
    if isinstance(ns, bool) or not isinstance(ns, int):
        raise TimebaseError("nanoseconds must be an integer")
    return Fraction(ns, NS_PER_SECOND)


def ns_to_string(ns: int) -> str:
    """Canonical rational-seconds rendering used in human reports."""
    value = ns_to_seconds(ns)
    if value.denominator == 1:
        return f"{value.numerator}s"
    return f"{value.numerator}/{value.denominator}s"


def _parse_text(value: str, fps: Fraction | None, version: int) -> int:
    text = value.strip()
    if not text:
        raise TimebaseError("time string is empty")
    if text.endswith("ns"):
        try:
            return int(text[:-2])
        except ValueError as exc:
            raise TimebaseError(f"invalid nanosecond value: {value!r}") from exc
    if text.endswith("f"):
        if fps is None:
            raise TimebaseError("frame time requires fps")
        try:
            frames = int(text[:-1])
        except ValueError as exc:
            raise TimebaseError(f"invalid frame value: {value!r}") from exc
        return seconds_to_ns(Fraction(frames, 1) / fps)
    if text.endswith("s"):
        body = text[:-1]
        try:
            if "/" in body:
                left, right = body.split("/", 1)
                return seconds_to_ns(rational_seconds(int(left), int(right)))
            return seconds_to_ns(Fraction(int(body), 1))
        except (ValueError, ZeroDivisionError) as exc:
            raise TimebaseError(f"invalid seconds value: {value!r}") from exc
    if version == 1:
        try:
            return int(text)
        except ValueError as exc:
            raise TimebaseError(f"invalid v1 nanosecond value: {value!r}") from exc
    if fps is not None:
        try:
            return seconds_to_ns(Fraction(int(text), 1) / fps)
        except ValueError as exc:
            raise TimebaseError(f"invalid v2 frame value: {value!r}") from exc
    raise TimebaseError(f"v2 time needs num/den, frame, or a unit suffix: {value!r}")


def parse_time(
    value: Any,
    version: int = 2,
    fps: Fraction | int | str | None = None,
    field: str = "time",
) -> int:
    """Parse a v1 nanosecond or v2 rational/frame time into integer ns.

    v1 accepts integer nanoseconds, ``{"ns": N}`` and ``"Nns"``.
    v2 accepts ``{"num": N, "den": D}``, ``{"frame": N}``, ``"N/Ds"``,
    ``"Ns"`` and (when *fps* is known) ``"Nf"``.  A bare v2 integer is a
    frame number when *fps* is supplied; otherwise it is seconds.
    """
    if version not in (1, 2):
        raise TimebaseError(f"unsupported time version: {version}")
    rate = None if fps is None else (fps if isinstance(fps, Fraction) else parse_rate(fps))
    if isinstance(value, bool) or isinstance(value, float):
        raise TimebaseError(f"invalid {field}: floats are not allowed")
    if isinstance(value, int):
        if version == 1:
            return value
        if rate is not None:
            return seconds_to_ns(Fraction(value, 1) / rate)
        return seconds_to_ns(Fraction(value, 1))
    if isinstance(value, Fraction):
        if version == 1:
            raise TimebaseError(f"invalid v1 {field}: expected integer ns")
        return seconds_to_ns(value)
    if isinstance(value, str):
        return _parse_text(value, rate, version)
    if isinstance(value, dict):
        if "ns" in value:
            ns = value["ns"]
            if isinstance(ns, bool) or not isinstance(ns, int):
                raise TimebaseError(f"invalid {field}.ns: expected integer")
            return ns
        if "frame" in value or "frames" in value:
            if rate is None:
                raise TimebaseError(f"invalid {field}: frame value requires fps")
            frames = value.get("frame", value.get("frames"))
            if isinstance(frames, bool) or not isinstance(frames, int):
                raise TimebaseError(f"invalid {field}.frame: expected integer")
            return seconds_to_ns(Fraction(frames, 1) / rate)
        if "num" in value or "den" in value:
            if version == 1:
                raise TimebaseError(f"invalid v1 {field}: rational seconds not allowed")
            if "num" not in value or "den" not in value:
                raise TimebaseError(f"invalid {field}: num and den are both required")
            try:
                return seconds_to_ns(rational_seconds(value["num"], value["den"]))
            except TimebaseError:
                raise
            except Exception as exc:
                raise TimebaseError(f"invalid {field} rational value") from exc
        if "seconds" in value:
            if version == 1:
                raise TimebaseError(f"invalid v1 {field}: rational seconds not allowed")
            return parse_time(value["seconds"], version=2, fps=rate, field=f"{field}.seconds")
    raise TimebaseError(f"invalid {field}: unsupported time value {value!r}")


def frames_for_duration(duration_ns: int, fps: Fraction | int | str) -> int:
    """Ceil duration * fps exactly; a positive sliver still occupies a frame."""
    if isinstance(duration_ns, bool) or not isinstance(duration_ns, int):
        raise TimebaseError("duration_ns must be an integer")
    if duration_ns < 0:
        raise TimebaseError("duration_ns must not be negative")
    rate = fps if isinstance(fps, Fraction) else parse_rate(fps)
    value = Fraction(duration_ns, NS_PER_SECOND) * rate
    return -(-value.numerator // value.denominator)


ns_to_fraction = ns_to_seconds
fraction_to_ns = seconds_to_ns


def parse_v1_time(value: Any, *, field: str = "time") -> int:
    return parse_time(value, version=1, field=field)


def parse_v2_time(value: Any, *, fps: Fraction | int | str | None = None,
                  field: str = "time") -> int:
    return parse_time(value, version=2, fps=fps, field=field)
