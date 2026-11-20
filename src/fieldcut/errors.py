"""Unified error model shared by every pipeline stage.

A record always carries the stage, the camera/stream it belongs to, the media
time (90 kHz ticks on the shared timeline) and, when known, the byte offset
inside the *original* input file. Recovery state travels with the record so
that a later re-run can verify already-completed segments instead of
re-encoding them.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any


class Stage(str, Enum):
    PROBE = "probe"
    ALIGN = "align"
    SEGMENT = "segment"
    ENCODE = "encode"
    MANIFEST = "manifest"
    PIPELINE = "pipeline"
    IO = "io"


class Recovery(str, Enum):
    NONE = "none"
    UNWRAPPED = "unwrapped"
    SEGMENTED = "segmented"          # quarantined inside a flagged range
    RESYNCED = "resynced"            # decoding resumed at a later keyframe
    SKIPPED = "skipped"              # un-decodable unit skipped, range kept
    RETRIED = "retried"
    UNRECOVERABLE = "unrecoverable"


@dataclass
class ErrorRecord:
    stage: str
    camera: str
    message: str
    media_time_ticks: int | None = None
    byte_offset: int | None = None
    recovery: str = Recovery.NONE.value

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {
            "stage": self.stage,
            "camera": self.camera,
            "message": self.message,
            "recovery": self.recovery,
        }
        if self.media_time_ticks is not None:
            d["media_time_ticks"] = self.media_time_ticks
        if self.byte_offset is not None:
            d["byte_offset"] = self.byte_offset
        return d


@dataclass
class ErrorLog:
    """Collects structured errors; one camera's failure never hides others."""

    records: list[ErrorRecord] = field(default_factory=list)

    def add(
        self,
        stage: Stage,
        camera: str,
        message: str,
        media_time_ticks: int | None = None,
        byte_offset: int | None = None,
        recovery: Recovery = Recovery.NONE,
    ) -> ErrorRecord:
        rec = ErrorRecord(stage.value, camera, message,
                          media_time_ticks, byte_offset, recovery.value)
        self.records.append(rec)
        return rec

    @property
    def unrecovered(self) -> list[ErrorRecord]:
        return [r for r in self.records
                if r.recovery == Recovery.UNRECOVERABLE.value]

    def for_stage(self, stage: Stage) -> list[ErrorRecord]:
        return [r for r in self.records if r.stage == stage.value]

    def to_list(self) -> list[dict[str, Any]]:
        return [r.to_dict() for r in self.records]


class FieldcutError(Exception):
    """Base class for pipeline failures mapped to CLI exit codes."""


class UsageError(FieldcutError):
    """Invalid arguments or unreadable input -> exit code 1 (per spec)."""


class UnrecoveredError(FieldcutError):
    """Unrecovered media or an alignment/manifest violation -> exit code 2."""
