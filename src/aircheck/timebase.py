"""Unified integer time base, timestamp conversion and the shared error model.

Every media module reports timestamp problems through the types defined here so
that a single anomaly vocabulary (negative, non-monotonic, wrap, gap) is used by
demux, mapping, hashing and the CLI.

The canonical clock is integer nanoseconds.  It is a *validation* clock only:
lossless remux always forwards each packet's original ``pts``/``dts`` untouched,
so adopting the canonical clock can never perturb the compressed payload.
"""

from __future__ import annotations

import enum
from dataclasses import dataclass, field
from fractions import Fraction

TICKS_PER_SECOND = 1_000_000_000

#: A backwards jump larger than this many raw ticks is treated as a 32-bit
#: timestamp wrap rather than ordinary disorder.
WRAP_THRESHOLD = 1 << 30


class ExitCode(enum.IntEnum):
    """Process exit codes, part of the public terminal contract."""

    OK = 0
    ACCEPTANCE_DIFF = 1
    USAGE = 2
    INPUT = 2
    UNRECOVERABLE = 3


class ErrorCategory(enum.StrEnum):
    USAGE = "usage"
    INPUT = "input"
    TIMESTAMP = "timestamp"
    MAPPING = "mapping"
    DECODE = "decode"
    CORRUPT = "corrupt"
    TRUNCATED = "truncated"
    NO_INDEX = "no_index"
    CANCELLED = "cancelled"
    RESUME = "resume"
    QUEUE = "queue"
    INTERNAL = "internal"


class AircheckError(Exception):
    """Base error carrying a stable category and exit code.

    ``context`` is a flat mapping that the CLI serialises verbatim; it exists so
    a failed asset never loses the stream numbers or byte offsets that explain
    the failure.
    """

    category: ErrorCategory = ErrorCategory.INTERNAL
    exit_code: int = ExitCode.UNRECOVERABLE

    def __init__(self, message: str, **context: object) -> None:
        super().__init__(message)
        self.message = message
        self.context: dict[str, object] = dict(context)

    def to_dict(self) -> dict[str, object]:
        return {
            "category": self.category.value,
            "message": self.message,
            "exit_code": int(self.exit_code),
            "context": self.context,
        }


class UsageError(AircheckError):
    category = ErrorCategory.USAGE
    exit_code = ExitCode.USAGE


class InputError(AircheckError):
    category = ErrorCategory.INPUT
    exit_code = ExitCode.INPUT


class MappingError(AircheckError):
    category = ErrorCategory.MAPPING
    exit_code = ExitCode.INPUT


class TimestampError(AircheckError):
    category = ErrorCategory.TIMESTAMP
    exit_code = ExitCode.UNRECOVERABLE


class DecodeError(AircheckError):
    category = ErrorCategory.DECODE
    exit_code = ExitCode.UNRECOVERABLE


class CorruptStreamError(AircheckError):
    category = ErrorCategory.CORRUPT
    exit_code = ExitCode.UNRECOVERABLE


class TruncatedStreamError(AircheckError):
    category = ErrorCategory.TRUNCATED
    exit_code = ExitCode.UNRECOVERABLE


class NoIndexError(AircheckError):
    category = ErrorCategory.NO_INDEX
    exit_code = ExitCode.UNRECOVERABLE


class CancelledError(AircheckError):
    category = ErrorCategory.CANCELLED
    exit_code = ExitCode.UNRECOVERABLE


class QueueOverflowError(AircheckError):
    category = ErrorCategory.QUEUE
    exit_code = ExitCode.UNRECOVERABLE


class UnrecoverableMuxError(AircheckError):
    category = ErrorCategory.MAPPING
    exit_code = ExitCode.UNRECOVERABLE


@dataclass(frozen=True)
class TimestampIssue:
    """One detected timestamp anomaly, anchored to a stream and packet."""

    kind: str
    stream_index: int
    pts: int | None
    previous_pts: int | None
    time_base: Fraction
    detail: str = ""


def as_fraction(value: object) -> Fraction:
    """Coerce a PyAV ``time_base`` (or numeric) into an exact Fraction."""
    if isinstance(value, Fraction):
        return value
    if value is None:
        raise ValueError("time base must not be None")
    if isinstance(value, int):
        return Fraction(value, 1)
    if isinstance(value, tuple) and len(value) == 2:
        return Fraction(int(value[0]), int(value[1]))
    return Fraction(value)  # type: ignore[arg-type]


def to_ticks(value: int | None, time_base: Fraction | object) -> int | None:
    """Convert a raw timestamp to canonical nanoseconds without float error."""
    if value is None:
        return None
    tb = as_fraction(time_base)
    return (int(value) * tb.numerator * TICKS_PER_SECOND) // tb.denominator


def rescale_ticks(ticks: int, src: Fraction, dst: Fraction) -> int:
    """Rescale integer ticks between two rational time bases (round half up)."""
    if src == dst:
        return ticks
    numerator = ticks * src.numerator * dst.denominator
    denominator = src.denominator * dst.numerator
    quotient, remainder = divmod(numerator, denominator)
    if remainder * 2 >= denominator:
        quotient += 1
    return quotient


def ticks_to_seconds(ticks: int) -> float:
    return ticks / TICKS_PER_SECOND


def format_ticks(ticks: int | None) -> str:
    if ticks is None:
        return "none"
    sign = "-" if ticks < 0 else ""
    whole, frac = divmod(abs(ticks), TICKS_PER_SECOND)
    return f"{sign}{whole}.{frac:09d}"


def segment_index(ticks: int, segment_ticks: int) -> int:
    """Map a canonical timestamp onto a non-negative segment slot."""
    if segment_ticks <= 0:
        raise ValueError("segment_ticks must be positive")
    return ticks // segment_ticks


@dataclass
class TimestampTracker:
    """Per-stream monotonicity / wrap / negative timestamp detector.

    Feed it every demuxed packet in mux order.  It classifies a backwards step
    as a 32-bit *wrap* only when the jump is large and lands near zero;
    everything else is *non_monotonic*.  Negative timestamps *before* the stream
    has produced its first non-negative timestamp are treated as encoder
    priming (AAC/Opus pre-roll) rather than damage, and are counted separately.

    The tracker records the issue list and keeps a phase counter so callers can
    tell wrapped runs apart from genuine timeline resets.
    """

    stream_index: int
    time_base: Fraction
    previous_raw: int | None = None
    previous_dts: int | None = None
    wraps: int = 0
    priming_packets: int = 0
    issues: list[TimestampIssue] = field(default_factory=list)
    _started: bool = False

    def observe(self, pts: int | None, dts: int | None = None) -> list[TimestampIssue]:
        found: list[TimestampIssue] = []
        if pts is None:
            return found
        if pts < 0:
            if self._started:
                found.append(
                    TimestampIssue(
                        "negative", self.stream_index, pts, self.previous_raw, self.time_base,
                        "negative presentation timestamp after the stream started",
                    )
                )
            else:
                # Encoder priming / pre-roll emitted before timeline zero.
                self.priming_packets += 1
                self.previous_raw = pts
                if dts is not None:
                    self.previous_dts = dts
                return found
        elif self.previous_raw is not None and pts < max(self.previous_raw, 0):
            delta = max(self.previous_raw, 0) - pts
            if delta > WRAP_THRESHOLD and pts < WRAP_THRESHOLD:
                self.wraps += 1
                found.append(
                    TimestampIssue(
                        "wrap", self.stream_index, pts, self.previous_raw, self.time_base,
                        f"timestamp wrapped after jump of {delta} raw ticks",
                    )
                )
            else:
                found.append(
                    TimestampIssue(
                        "non_monotonic", self.stream_index, pts, self.previous_raw,
                        self.time_base, f"timestamp moved backwards by {delta} raw ticks",
                    )
                )
        if pts >= 0:
            self._started = True
        self.previous_raw = pts
        if dts is not None:
            self.previous_dts = dts
        self.issues.extend(found)
        return found

    def report(self) -> dict[str, object]:
        kinds = sorted({issue.kind for issue in self.issues})
        return {
            "stream_index": self.stream_index,
            "time_base": str(self.time_base),
            "wraps": self.wraps,
            "priming_packets": self.priming_packets,
            "issues": len(self.issues),
            "kinds": kinds,
        }
