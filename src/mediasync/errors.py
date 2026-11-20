"""Shared error model for mediasync.

Every failure that the CLI is expected to translate into a stable exit code
derives from :class:`MediaSyncError`.  The CLI maps error categories to exit
codes as documented on :data:`EXIT_CODES`.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Iterable, Sequence

# Exit codes (documented in the demo/verify acceptance contract).
EXIT_OK = 0
EXIT_INVALID_INPUT = 2
EXIT_UNRECOVERABLE = 3
EXIT_QUALITY_GATE = 4

EXIT_CODES: dict[str, int] = {
    "ok": EXIT_OK,
    "invalid": EXIT_INVALID_INPUT,
    "unrecoverable": EXIT_UNRECOVERABLE,
    "quality": EXIT_QUALITY_GATE,
}


class MediaSyncError(Exception):
    """Base class for all expected mediasync failures."""

    exit_code = EXIT_INVALID_INPUT
    category = "invalid"

    def __init__(self, message: str) -> None:
        super().__init__(message)
        self.message = message

    def __str__(self) -> str:  # pragma: no cover - trivial
        return self.message


class InputError(MediaSyncError):
    """Invalid path, malformed fixture, or out-of-range parameter."""

    exit_code = EXIT_INVALID_INPUT
    category = "invalid"


class DependencyError(InputError):
    """A required third-party package or FFmpeg library is missing."""


class CodecConflictError(InputError):
    """A stream mixes incompatible codecs or cannot be decoded by this build."""


class IndexError_(InputError):
    """A keyframe / packet index is malformed or internally inconsistent."""


class StreamError(InputError):
    """A raw elementary stream is truncated or otherwise unusable."""


class PlanError(InputError):
    """The timeline dependency graph is invalid (cycle, missing edge, overlap)."""


class UnrecoverableError(MediaSyncError):
    """At least one requested interval cannot be reconstructed."""

    exit_code = EXIT_UNRECOVERABLE
    category = "unrecoverable"

    def __init__(self, message: str, intervals: Sequence["Gap"] = ()) -> None:
        super().__init__(message)
        self.intervals = list(intervals)


class QualityGateError(MediaSyncError):
    """A manifest or synchronization metric violates its tolerance."""

    exit_code = EXIT_QUALITY_GATE
    category = "quality"


@dataclass(frozen=True)
class Gap:
    """A half-open interval ``[start, end)`` in 90 kHz ticks."""

    track: str
    start_ticks: int
    end_ticks: int
    reason: str = "unrecoverable"

    def __post_init__(self) -> None:
        if self.end_ticks <= self.start_ticks:
            raise InputError(
                f"gap on {self.track!r} must have positive length, "
                f"got [{self.start_ticks}, {self.end_ticks})"
            )

    @property
    def duration_ticks(self) -> int:
        return self.end_ticks - self.start_ticks

    def overlaps(self, other: "Gap") -> bool:
        return self.start_ticks < other.end_ticks and other.start_ticks < self.end_ticks

    def to_dict(self) -> dict[str, object]:
        return {
            "track": self.track,
            "start_ticks": self.start_ticks,
            "end_ticks": self.end_ticks,
            "duration_ticks": self.duration_ticks,
            "reason": self.reason,
        }


@dataclass
class IssueLog:
    """Collects non-fatal findings so they can be reported before exit."""

    issues: list[dict[str, object]] = field(default_factory=list)

    def add(self, kind: str, message: str, **context: object) -> None:
        entry: dict[str, object] = {"kind": kind, "message": message}
        entry.update(context)
        self.issues.append(entry)

    def extend(self, entries: Iterable[dict[str, object]]) -> None:
        self.issues.extend(entries)

    def __len__(self) -> int:
        return len(self.issues)
