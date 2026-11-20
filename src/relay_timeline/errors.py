"""Unified error taxonomy shared by every pipeline stage.

Two severity families exist:

* :class:`RecoverableError` marks transient/local problems (a truncated
  segment, a slow consumer, a gap that a policy can fill).  The pipeline
  records them and keeps running under ``gap-fill`` / ``skip`` policies.
* :class:`FatalError` marks problems that invalidate the whole run
  (path traversal, exceeded safety limits, an unwritable output).

Process exit codes follow the acceptance contract:

* 0 - acceptance passed
* 2 - input / timeline validation failure
* 3 - unrecoverable processing or write error
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum

EXIT_OK = 0
EXIT_VALIDATION = 2
EXIT_PROCESSING = 3


class Severity(str, Enum):
    """Whether an incident can be survived by a configured policy."""

    RECOVERABLE = "recoverable"
    FATAL = "fatal"


class ErrorCode(str, Enum):
    """Stable machine-readable error codes."""

    PATH_TRAVERSAL = "path_traversal"
    LIMIT_EXCEEDED = "limit_exceeded"
    MANIFEST_PARSE = "manifest_parse"
    MANIFEST_ALIGNMENT = "manifest_alignment"
    MISSING_SEGMENT = "missing_segment"
    TRUNCATED_SEGMENT = "truncated_segment"
    ZERO_LENGTH_SEGMENT = "zero_length_segment"
    TIMESTAMP_WRAP = "timestamp_wrap"
    TIMESTAMP_BACKWARD = "timestamp_backward"
    TIMESTAMP_DUPLICATE = "timestamp_duplicate"
    TIMESTAMP_GAP = "timestamp_gap"
    DRIFT_EXCEEDED = "drift_exceeded"
    BITRATE_MISMATCH = "bitrate_mismatch"
    DECODE_FAILURE = "decode_failure"
    SLOW_CONSUMER = "slow_consumer"
    CHECKPOINT_MISMATCH = "checkpoint_mismatch"
    WRITE_INTERRUPTED = "write_interrupted"
    CANCELLED = "cancelled"


# Error codes that abort the run regardless of policy.
FATAL_CODES = frozenset(
    {
        ErrorCode.PATH_TRAVERSAL,
        ErrorCode.LIMIT_EXCEEDED,
        ErrorCode.MANIFEST_PARSE,
        ErrorCode.CHECKPOINT_MISMATCH,
        ErrorCode.WRITE_INTERRUPTED,
    }
)


class RelayError(Exception):
    """Base class carrying a stable code and an offset in media time."""

    severity = Severity.FATAL

    def __init__(
        self,
        code: ErrorCode,
        message: str,
        *,
        offset: int = 0,
        segment: str | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.offset = offset
        self.segment = segment

    @property
    def is_fatal(self) -> bool:
        return self.severity is Severity.FATAL or self.code in FATAL_CODES


class FatalError(RelayError):
    """The run cannot continue."""

    severity = Severity.FATAL


class RecoverableError(RelayError):
    """A local incident; continuation depends on the configured policy."""

    severity = Severity.RECOVERABLE


@dataclass(slots=True)
class ErrorRecord:
    """One persisted incident, ordered by media offset."""

    code: str
    severity: str
    message: str
    offset: int
    segment: str | None = None
    time_range: tuple[int, int] | None = None
    recovered: bool = False

    def to_dict(self) -> dict:
        data = {
            "code": self.code,
            "severity": self.severity,
            "message": self.message,
            "offset": self.offset,
            "segment": self.segment,
            "recovered": self.recovered,
        }
        if self.time_range is not None:
            data["time_range"] = list(self.time_range)
        return data


@dataclass(slots=True)
class ErrorBook:
    """Collects incidents and tracks the first bad offset."""

    records: list[ErrorRecord] = field(default_factory=list)

    def add(self, err: RelayError, *, time_range: tuple[int, int] | None = None,
            recovered: bool = False) -> ErrorRecord:
        record = ErrorRecord(
            code=err.code.value,
            severity=err.severity.value,
            message=err.message,
            offset=err.offset,
            segment=err.segment,
            time_range=time_range,
            recovered=recovered,
        )
        self.records.append(record)
        return record

    def first_offset(self) -> int | None:
        if not self.records:
            return None
        return min(r.offset for r in self.records)

    def counts(self) -> dict[str, int]:
        out: dict[str, int] = {"recoverable": 0, "fatal": 0}
        for rec in self.records:
            out[rec.severity] += 1
        return out

    def to_list(self) -> list[dict]:
        return [r.to_dict() for r in self.records]
