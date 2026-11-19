"""Unified error model: stage, track, raw byte offset and recovery action."""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any


class Stage(str, Enum):
    """Pipeline stage an error belongs to."""

    PARSE = "parse"
    INDEX = "index"
    TRANSFORM = "transform"
    VALIDATE = "validate"


class Recovery(str, Enum):
    """Recovery action taken (or not) for an error."""

    NONE = "none"
    SKIPPED_GARBAGE = "skipped_garbage"
    DROPPED_UNIT = "dropped_unit"
    REORDERED = "reordered"
    DEDUPLICATED = "deduplicated"
    RETRANSMITTED = "retransmitted"
    UNWRAPPED = "unwrapped"
    QUARANTINED = "quarantined"
    SEGMENTED = "segmented"
    UNRECOVERABLE = "unrecoverable"


@dataclass
class ErrorRecord:
    """One structured error event.

    ``byte_offset`` always refers to the offset inside the *original* input
    stream file, never to a reassembled intermediate buffer.
    """

    stage: str
    track: str
    message: str
    byte_offset: int | None = None
    recovery: str = Recovery.NONE.value
    packet_seq: int | None = None

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {
            "stage": self.stage,
            "track": self.track,
            "message": self.message,
            "recovery": self.recovery,
        }
        if self.byte_offset is not None:
            d["byte_offset"] = self.byte_offset
        if self.packet_seq is not None:
            d["packet_seq"] = self.packet_seq
        return d


@dataclass
class ErrorLog:
    """Collects errors; a failure in one track never hides other tracks."""

    records: list[ErrorRecord] = field(default_factory=list)

    def add(
        self,
        stage: Stage,
        track: str,
        message: str,
        byte_offset: int | None = None,
        recovery: Recovery = Recovery.NONE,
        packet_seq: int | None = None,
    ) -> ErrorRecord:
        rec = ErrorRecord(
            stage=stage.value,
            track=track,
            message=message,
            byte_offset=byte_offset,
            recovery=recovery.value,
            packet_seq=packet_seq,
        )
        self.records.append(rec)
        return rec

    @property
    def unrecovered(self) -> list[ErrorRecord]:
        return [r for r in self.records if r.recovery == Recovery.UNRECOVERABLE.value]

    def to_list(self) -> list[dict[str, Any]]:
        return [r.to_dict() for r in self.records]


class RemuxError(Exception):
    """Base error."""


class InputError(RemuxError):
    """Unreadable input or bad arguments -> process exit code 2."""


class UnrecoveredDataError(RemuxError):
    """Unrecovered data or failed index validation -> process exit code 1."""
