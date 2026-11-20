"""Typed, locatable errors shared by every pipeline stage.

An error always carries enough provenance to answer *where*: the pipeline
stage, the byte offset inside the input record (or ``-1`` when unknown) and,
for media failures, the media time in seconds. Recoverable errors are
collected into error lists instead of tearing the pipeline down so later,
unrelated records keep processing.
"""

from __future__ import annotations

import enum
from dataclasses import asdict, dataclass, field
from typing import Any


class Stage(str, enum.Enum):
    """Ordered pipeline stages; the string value is persisted verbatim."""

    SUBTITLE = "subtitle"
    TIMELINE = "timeline"
    DECODE = "decode"
    RESAMPLE = "resample"
    SEGMENT = "segment"
    SILENCE = "silence"
    FINGERPRINT = "fingerprint"
    COLOR = "color"
    WRITE = "write"
    CHECKPOINT = "checkpoint"


# Exit-code contract used by the terminal adapter.
EXIT_OK = 0
EXIT_INPUT_ERROR = 2
EXIT_VERIFY_FAILED = 3
EXIT_CHECKPOINT_MISMATCH = 4


class RelayError(Exception):
    """Base class. ``stage`` defaults to None for pre-pipeline failures."""

    exit_code: int = EXIT_INPUT_ERROR

    def __init__(self, message: str, *, stage: Stage | None = None) -> None:
        super().__init__(message)
        self.message = message
        self.stage = stage


class InputError(RelayError):
    """A bad input record (bad timecode, truncated line, unknown format)."""

    exit_code = EXIT_INPUT_ERROR


class MediaError(RelayError):
    """A decode/container failure that may be recoverable at record level."""

    exit_code = EXIT_INPUT_ERROR


class CheckpointMismatch(RelayError):
    """Stored checkpoint no longer matches the input or parameter digest."""

    exit_code = EXIT_CHECKPOINT_MISMATCH


class VerifyFailure(RelayError):
    """Re-verification of an archived run produced different evidence."""

    exit_code = EXIT_VERIFY_FAILED


@dataclass(frozen=True, slots=True)
class ErrorRecord:
    """One isolated, non-fatal failure, serialised into the run summary."""

    stage: str
    kind: str
    message: str
    source: str = ""
    # Byte offset of the offending record inside the source file (-1 unknown).
    offset_bytes: int = -1
    # 1-based source line for text records (-1 unknown).
    line: int = -1
    # Decoder media time in seconds for media failures.
    media_time: float | None = None
    extra: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_exception(
        cls,
        exc: RelayError,
        *,
        source: str = "",
        offset_bytes: int = -1,
        line: int = -1,
        media_time: float | None = None,
        extra: dict[str, Any] | None = None,
    ) -> "ErrorRecord":
        return cls(
            stage=exc.stage.value if exc.stage else "pipeline",
            kind=type(exc).__name__,
            message=exc.message,
            source=source,
            offset_bytes=offset_bytes,
            line=line,
            media_time=media_time,
            extra=dict(extra or {}),
        )


def first_offset(errors: list[ErrorRecord]) -> int:
    """Smallest known byte offset across a run's errors, or -1 if none."""
    known = [e.offset_bytes for e in errors if e.offset_bytes >= 0]
    return min(known) if known else -1
