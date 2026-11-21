"""Error model for the archival media pipeline.

Errors fall into two severity classes:

* :class:`RecoverableMediaError` marks a single bad region (a corrupted
  packet, a truncated tail, a non-monotonic timestamp section).  The
  pipeline isolates the region, records its location and keeps going.
* :class:`FatalMediaError` stops the run.  Already committed checkpoints
  stay on disk so the next invocation can resume.

Every error carries a stable ``code`` string plus the position in the
*corrected shared timeline* (``time_s``) where it was detected, so the
manifest can report exact, auditable locations.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum


class Severity(str, Enum):
    RECOVERABLE = "recoverable"
    FATAL = "fatal"


# Stable machine-readable codes.
E_TRUNCATED = "truncated_input"
E_CORRUPT_PACKET = "corrupt_packet"
E_MISSING_TRACK = "missing_track"
E_NON_MONOTONIC = "non_monotonic_timestamp"
E_INDEX_WRITE = "index_write_failure"
E_UNSUPPORTED = "unsupported_media"
E_LIMIT = "resource_limit"
E_CHECKPOINT_INCOMPATIBLE = "incompatible_checkpoint"


class MediaError(Exception):
    """Base class carrying a stable code and timeline position."""

    severity: Severity = Severity.FATAL

    def __init__(
        self,
        code: str,
        message: str,
        *,
        time_s: float | None = None,
        track: str | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.time_s = time_s
        self.track = track

    def to_dict(self) -> dict:
        return {
            "code": self.code,
            "severity": self.severity.value,
            "message": self.message,
            "time_s": None if self.time_s is None else round(self.time_s, 6),
            "track": self.track,
        }


class RecoverableMediaError(MediaError):
    """A bad region that can be isolated while processing continues."""

    severity = Severity.RECOVERABLE


class FatalMediaError(MediaError):
    """Processing cannot continue; committed checkpoints are preserved."""


@dataclass
class ErrorRegion:
    """An isolated bad region recorded in the index/manifest.

    ``start_s``/``end_s`` bracket the region on the corrected timeline.
    For a single packet failure the two values coincide.
    """

    code: str
    message: str
    start_s: float
    end_s: float
    track: str | None = None

    def to_dict(self) -> dict:
        return {
            "code": self.code,
            "severity": Severity.RECOVERABLE.value,
            "message": self.message,
            "start_s": round(self.start_s, 6),
            "end_s": round(self.end_s, 6),
            "track": self.track,
        }


@dataclass
class ErrorLog:
    """Collects recoverable regions; raises on the first fatal error."""

    regions: list[ErrorRegion] = field(default_factory=list)

    def recoverable(self, exc: RecoverableMediaError, *, end_s: float | None = None) -> ErrorRegion:
        region = ErrorRegion(
            code=exc.code,
            message=exc.message,
            start_s=0.0 if exc.time_s is None else exc.time_s,
            end_s=exc.time_s if end_s is None else end_s,
            track=exc.track,
        )
        self.regions.append(region)
        return region

    def has_errors(self) -> bool:
        return bool(self.regions)

    def to_dicts(self) -> list[dict]:
        return [r.to_dict() for r in self.regions]
