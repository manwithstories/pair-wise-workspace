"""Shared error model used across every pipeline stage.

Error records always carry the processing stage, the asset identifier, the
segment index (when applicable), a byte offset (when applicable) and a
recoverability classification.  Three classes are distinguished:

* ``skippable``  – the offending item can be dropped, processing continues
* ``retryable``  – a transient failure (network/disk), a retry may succeed
* ``fatal``      – unrecoverable media/input error, the pipeline aborts
"""

from __future__ import annotations

import enum
from dataclasses import dataclass, field, asdict
from typing import Any


class ErrorStage(str, enum.Enum):
    MANIFEST = "manifest"
    TIMELINE = "timeline"
    SEGMENT = "segment"
    STREAM = "stream"
    RECOVERY = "recovery"
    CHECKPOINT = "checkpoint"
    METRICS = "metrics"
    IO = "io"


class ErrorKind(str, enum.Enum):
    SKIPPABLE = "skippable"
    RETRYABLE = "retryable"
    FATAL = "fatal"


# Exit codes documented in the acceptance specification.
EXIT_OK = 0
EXIT_INVALID = 2
EXIT_METRICS = 3
EXIT_UNRECOVERABLE = 4


@dataclass
class ErrorRecord:
    """A single structured error located on the media timeline."""

    stage: ErrorStage
    asset_id: str
    message: str
    kind: ErrorKind
    segment_index: int | None = None
    byte_offset: int | None = None
    path: str | None = None
    recoverable: bool | None = None
    detail: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        if isinstance(self.stage, str):
            self.stage = ErrorStage(self.stage)
        if isinstance(self.kind, str):
            self.kind = ErrorKind(self.kind)
        if self.recoverable is None:
            self.recoverable = self.kind is not ErrorKind.FATAL

    @property
    def is_fatal(self) -> bool:
        return self.kind is ErrorKind.FATAL

    def to_dict(self) -> dict[str, Any]:
        data = asdict(self)
        data["stage"] = self.stage.value
        data["kind"] = self.kind.value
        return data


class PipelineError(Exception):
    """Exception wrapping an :class:`ErrorRecord` for stage-bound propagation."""

    def __init__(self, record: ErrorRecord) -> None:
        super().__init__(record.message)
        self.record = record


def _make(
    stage: ErrorStage,
    kind: ErrorKind,
    asset_id: str,
    message: str,
    *,
    segment_index: int | None = None,
    byte_offset: int | None = None,
    path: str | None = None,
    detail: dict[str, Any] | None = None,
) -> ErrorRecord:
    return ErrorRecord(
        stage=stage,
        asset_id=asset_id,
        message=message,
        kind=kind,
        segment_index=segment_index,
        byte_offset=byte_offset,
        path=path,
        detail=detail or {},
    )


def skippable(stage: ErrorStage, asset_id: str, message: str, **kw: Any) -> ErrorRecord:
    return _make(stage, ErrorKind.SKIPPABLE, asset_id, message, **kw)


def retryable(stage: ErrorStage, asset_id: str, message: str, **kw: Any) -> ErrorRecord:
    return _make(stage, ErrorKind.RETRYABLE, asset_id, message, **kw)


def fatal(stage: ErrorStage, asset_id: str, message: str, **kw: Any) -> ErrorRecord:
    return _make(stage, ErrorKind.FATAL, asset_id, message, **kw)


class ErrorCollector:
    """Accumulates structured errors and reports aggregate status."""

    def __init__(self) -> None:
        self._records: list[ErrorRecord] = []

    def add(self, record: ErrorRecord) -> ErrorRecord:
        self._records.append(record)
        return record

    def extend(self, records: list[ErrorRecord]) -> None:
        self._records.extend(records)

    @property
    def records(self) -> list[ErrorRecord]:
        return list(self._records)

    def has_fatal(self) -> bool:
        return any(r.is_fatal for r in self._records)

    def count(self, kind: ErrorKind) -> int:
        return sum(1 for r in self._records if r.kind is kind)

    def to_list(self) -> list[dict[str, Any]]:
        return [r.to_dict() for r in self._records]
