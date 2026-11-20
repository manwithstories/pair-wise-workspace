"""Shared error model and process exit codes.

All stages raise the same small family of exceptions so the CLI can map them to
stable exit codes.  Non-fatal corruption is recorded as :class:`MediaIssue`
objects inside manifests instead of being silently dropped.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

EXIT_OK = 0
EXIT_INPUT = 2
EXIT_INTERRUPTED = 3
EXIT_METRICS = 4

#: Largest accepted deviation between requested and measured bitrate.
MAX_BITRATE_DEVIATION_PCT = 15.0
#: Largest accepted A/V alignment or random-access error, in milliseconds.
MAX_ALIGNMENT_MS = 40.0
#: Default RAM ceiling for bounded internal caches (bytes).
MEMORY_LIMIT_BYTES = 128 * 1024 * 1024


class MediaError(Exception):
    """Base class carrying a stable machine-readable code and byte offset."""

    exit_code = EXIT_INPUT
    code = "media_error"

    def __init__(self, message: str, *, offset: int | None = None,
                 code: str | None = None) -> None:
        super().__init__(message)
        self.message = message
        self.offset = offset
        if code is not None:
            self.code = code

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {"code": self.code, "message": self.message}
        if self.offset is not None:
            out["offset"] = self.offset
        return out


class MediaInputError(MediaError):
    """Bad arguments, missing files, malformed configuration or schema."""

    exit_code = EXIT_INPUT
    code = "input_error"


class MediaCorruptError(MediaError):
    """Damaged media input that cannot be decoded any further."""

    exit_code = EXIT_INPUT
    code = "corrupt_input"


class CheckpointConflict(MediaInputError):
    """An existing checkpoint was produced by incompatible input/config."""

    code = "checkpoint_conflict"

    def __init__(self, message: str, *, reason: str,
                 offset: int | None = None) -> None:
        super().__init__(message, offset=offset)
        self.reason = reason

    def to_dict(self) -> dict[str, Any]:
        out = super().to_dict()
        out["reason"] = self.reason
        return out


class Interrupted(MediaError):
    """A recoverable stage interruption; re-running resumes from checkpoint."""

    exit_code = EXIT_INTERRUPTED
    code = "interrupted"


class MetricsExceeded(MediaError):
    """Verification found a metric outside its documented limit."""

    exit_code = EXIT_METRICS
    code = "metrics_exceeded"


@dataclass(frozen=True)
class MediaIssue:
    """A located, non-fatal defect discovered while scanning or decoding."""

    code: str
    message: str
    offset: int
    severity: str = "error"
    detail: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
            "offset": self.offset,
            "severity": self.severity,
        }
        if self.detail:
            out["detail"] = dict(self.detail)
        return out
