"""Unified error model shared by every pipeline stage and the CLI.

Every anomaly is reported as a :class:`MediaError` carrying the track it
belongs to, the packet position (packet index and byte offset) where it was
observed, and a recovery level describing how the pipeline may proceed.
"""

from __future__ import annotations

import enum
from dataclasses import dataclass, field
from typing import Any


class Severity(enum.Enum):
    """Recovery level of a reported anomaly."""

    INFO = "info"                  # informational, no action taken
    REPAIRED = "repaired"          # anomaly was automatically repaired
    UNREPAIRABLE = "unrepairable"  # kept as evidence, cannot auto-repair
    FATAL = "fatal"                # processing of this input must abort


class ErrorCode:
    """Stable machine-readable error codes."""

    DEMUX_FAILURE = "demux_failure"
    TRUNCATED_INPUT = "truncated_input"
    CORRUPT_PACKET = "corrupt_packet"
    MISSING_TRACK = "missing_track"
    TIMESTAMP_OVERFLOW = "timestamp_overflow"
    TIMESTAMP_WRAP = "timestamp_wrap"
    TIMESTAMP_RESET = "timestamp_reset"
    OUT_OF_ORDER_PACKET = "out_of_order_packet"
    AMBIGUOUS_TIMELINE = "ambiguous_timeline"
    DUPLICATE_TIMESTAMP = "duplicate_timestamp"
    NEGATIVE_DURATION = "negative_duration"
    AV_SKEW = "av_skew"
    DRIFT_SEGMENT = "drift_segment"
    INDEX_VERIFY_FAILED = "index_verify_failed"
    INPUT_CHANGED = "input_changed"
    CHECKPOINT_UNTRUSTED = "checkpoint_untrusted"
    WRITE_FAILURE = "write_failure"


@dataclass
class MediaError(Exception):
    """An anomaly located at a specific track / packet position."""

    code: str
    message: str
    severity: Severity = Severity.REPAIRED
    track: str | None = None
    packet_index: int | None = None
    byte_offset: int | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:  # pragma: no cover - display helper
        loc = []
        if self.track is not None:
            loc.append(f"track={self.track}")
        if self.packet_index is not None:
            loc.append(f"packet={self.packet_index}")
        if self.byte_offset is not None:
            loc.append(f"byte={self.byte_offset}")
        where = f" [{', '.join(loc)}]" if loc else ""
        return f"{self.severity.value}:{self.code}{where} {self.message}"

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "severity": self.severity.value,
            "track": self.track,
            "packet_index": self.packet_index,
            "byte_offset": self.byte_offset,
            "details": self.details,
        }


class CheckpointError(MediaError):
    """Input changed or checkpoint state is untrusted (exit code 3)."""

    def __init__(self, message: str, code: str = ErrorCode.CHECKPOINT_UNTRUSTED):
        super().__init__(code=code, message=message, severity=Severity.FATAL)


class WriteFailure(MediaError):
    """Disk write or output validation failure (exit code 4)."""

    def __init__(self, message: str, code: str = ErrorCode.WRITE_FAILURE, **kw: Any):
        super().__init__(code=code, message=message, severity=Severity.FATAL, **kw)
