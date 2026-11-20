"""Structured error model shared by every stage.

Errors never carry media payload.  They carry the track id, the media
timestamp, the frame index or sample range, the processing stage and a stable
machine-readable code.  Ordering is defined so "first error" is unambiguous:
by media time, then track, then stage.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import IntEnum
from fractions import Fraction
from typing import Any, Iterable, Mapping

from .model import format_rational, format_time


class ExitCode(IntEnum):
    """CLI exit codes mandated by the acceptance criteria."""

    OK = 0
    INVALID_INPUT = 2
    VERIFY_FAILED = 3


class Stage(IntEnum):
    """Processing stages, ordered by dependency."""

    TIMELINE = 0
    PROBE = 1
    NORMALIZE = 2
    TRANSITION = 3
    SEGMENT = 4
    ENCODE = 5
    MANIFEST = 6

    @property
    def label(self) -> str:
        return self.name.lower()


class Severity(IntEnum):
    WARNING = 0
    ERROR = 1
    FATAL = 2


class Policy(IntEnum):
    """What the caller asked us to do when a stage fails."""

    STOP = 0
    SKIP = 1


@dataclass(frozen=True)
class MediaLocation:
    """Where in a media file an anomaly was observed."""

    track: str = ""
    pts: Fraction | None = None
    frame: int | None = None
    sample_start: int | None = None
    sample_end: int | None = None
    stream_index: int | None = None

    def describe(self) -> str:
        parts: list[str] = []
        if self.track:
            parts.append(f"track={self.track}")
        if self.stream_index is not None:
            parts.append(f"stream={self.stream_index}")
        if self.pts is not None:
            parts.append(f"pts={format_rational(self.pts)}s ({format_time(self.pts)})")
        if self.frame is not None:
            parts.append(f"frame={self.frame}")
        if self.sample_start is not None:
            if self.sample_end is None:
                parts.append(f"samples={self.sample_start}")
            else:
                parts.append(f"samples=[{self.sample_start},{self.sample_end})")
        return " ".join(parts)


@dataclass(frozen=True)
class MediaError(Exception):
    """A single structured processing error."""

    code: str
    stage: Stage
    message: str
    location: MediaLocation = field(default_factory=MediaLocation)
    severity: Severity = Severity.ERROR
    context: Mapping[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:
        where = self.location.describe()
        suffix = f" [{where}]" if where else ""
        return f"{self.stage.label}/{self.code}: {self.message}{suffix}"

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "stage": self.stage.label,
            "severity": self.severity.name.lower(),
            "message": self.message,
        }
        loc = self.location
        if loc.track:
            payload["track"] = loc.track
        if loc.pts is not None:
            payload["pts"] = format_rational(loc.pts)
            payload["pts_seconds"] = format_time(loc.pts)
        if loc.frame is not None:
            payload["frame"] = loc.frame
        if loc.sample_start is not None:
            payload["sample_start"] = loc.sample_start
        if loc.sample_end is not None:
            payload["sample_end"] = loc.sample_end
        if loc.stream_index is not None:
            payload["stream_index"] = loc.stream_index
        if self.context:
            payload["context"] = dict(self.context)
        return payload

    def sort_key(self) -> tuple:
        pts = self.location.pts
        # An error without a timestamp sorts last; within the same timestamp we
        # keep track/stage ordering stable for deterministic reports.
        time_key = (0, pts) if pts is not None else (1, Fraction(0))
        return (time_key[0], time_key[1], self.location.track, int(self.stage), self.code)


class TimelineError(Exception):
    """Raised by validation code that reports through :class:`Issue`."""

    def __init__(self, issues: Iterable[Issue]):
        self.issues = list(issues)
        super().__init__("; ".join(str(i) for i in self.issues) or "timeline invalid")


@dataclass(frozen=True)
class Issue:
    """A validation finding owned by the timeline/rules layer."""

    code: str
    message: str
    location: MediaLocation = field(default_factory=MediaLocation)
    context: Mapping[str, Any] = field(default_factory=dict)

    def to_media_error(self, stage: Stage) -> MediaError:
        return MediaError(
            code=self.code,
            stage=stage,
            message=self.message,
            location=self.location,
            context=self.context,
        )


class ErrorCollector:
    """Accumulates errors and answers "what was the first anomaly?"."""

    def __init__(self) -> None:
        self._errors: list[MediaError] = []

    def add(self, error: MediaError) -> None:
        self._errors.append(error)

    def extend(self, errors: Iterable[MediaError]) -> None:
        self._errors.extend(errors)

    def __len__(self) -> int:
        return len(self._errors)

    @property
    def errors(self) -> list[MediaError]:
        return sorted(self._errors, key=MediaError.sort_key)

    @property
    def first(self) -> MediaError | None:
        ordered = self.errors
        return ordered[0] if ordered else None

    def fatal(self) -> MediaError | None:
        for err in self.errors:
            if err.severity >= Severity.FATAL:
                return err
        return None

    def any_error(self) -> bool:
        return any(err.severity >= Severity.ERROR for err in self._errors)

    def summary(self) -> dict[str, Any]:
        first = self.first
        payload: dict[str, Any] = {
            "count": len(self._errors),
            "first_error_track": first.location.track if first else None,
            "first_error_pts": (
                format_rational(first.location.pts) if first and first.location.pts is not None else None
            ),
            "first_error_pts_seconds": (
                format_time(first.location.pts) if first and first.location.pts is not None else None
            ),
            "first_error_code": first.code if first else None,
            "first_error_stage": first.stage.label if first else None,
            "first_error_message": first.message if first else None,
        }
        frame = next((e.location.frame for e in self.errors if e.location.frame is not None), None)
        if frame is not None:
            payload["first_error_frame"] = frame
        sample = next((e.location.sample_start for e in self.errors if e.location.sample_start is not None), None)
        if sample is not None:
            payload["first_error_sample"] = sample
        return payload
