"""Shared error model for the regional media pipeline.

Every module (packet / clock / assemble / normalize / metadata / checkpoint /
manifest) reports problems with the same set of error codes so that the CLI,
the per-chunk manifest and the verification report stay comparable.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Iterable


class Code(str, Enum):
    """Stable error / diagnostic codes shared by all core modules."""

    # --- RTP / packet layer -------------------------------------------
    MALFORMED_HEADER = "ERR_MALFORMED_HEADER"
    TRUNCATED_PACKET = "ERR_TRUNCATED_PACKET"
    TRUNCATED_RECORD = "ERR_TRUNCATED_RECORD"
    RECORD_RESYNC = "ERR_RECORD_RESYNC"
    DUPLICATE_PACKET = "ERR_DUPLICATE_PACKET"
    REORDERED_PACKET = "ERR_REORDERED_PACKET"
    LOST_PACKET = "ERR_LOST_PACKET"
    SEQUENCE_WRAP = "ERR_SEQUENCE_WRAP"
    BOUNDARY_OVERLAP = "ERR_BOUNDARY_OVERLAP"

    # --- Clock / RTCP layer -------------------------------------------
    TIMESTAMP_WRAP = "ERR_TIMESTAMP_WRAP"
    TIMESTAMP_DISCONTINUITY = "ERR_TIMESTAMP_DISCONTINUITY"
    RTCP_MISSING = "ERR_RTCP_MISSING"
    RTCP_MALFORMED = "ERR_RTCP_MALFORMED"
    CLOCK_DRIFT = "ERR_CLOCK_DRIFT"

    # --- Audio processing --------------------------------------------
    UNSUPPORTED_LAYOUT = "ERR_UNSUPPORTED_LAYOUT"
    LAYOUT_MISMATCH = "ERR_LAYOUT_MISMATCH"
    RESAMPLE_FAILURE = "ERR_RESAMPLE_FAILURE"
    NONFINITE_SAMPLE = "ERR_NONFINITE_SAMPLE"
    CLIPPED_SAMPLE = "ERR_CLIPPED_SAMPLE"
    SILENT_BLOCK = "ERR_SILENT_BLOCK"
    SHORT_CHUNK = "ERR_SHORT_CHUNK"
    ENCODE_FAILURE = "ERR_ENCODE_FAILURE"
    EMPTY_STREAM = "ERR_EMPTY_STREAM"

    # --- Metadata -----------------------------------------------------
    METADATA_MISSING = "ERR_METADATA_MISSING"
    METADATA_CONFLICT = "ERR_METADATA_CONFLICT"
    VIDEO_TIME_SHIFT = "ERR_VIDEO_TIME_SHIFT"

    # --- Checkpoint / index ------------------------------------------
    CHUNK_HASH_MISMATCH = "ERR_CHUNK_HASH_MISMATCH"
    INDEX_INCONSISTENT = "ERR_INDEX_INCONSISTENT"
    CHUNK_MISSING = "ERR_CHUNK_MISSING"
    CHECKPOINT_INVALID = "ERR_CHECKPOINT_INVALID"
    INPUT_CHANGED = "ERR_INPUT_CHANGED"
    RESUME_UNAVAILABLE = "ERR_RESUME_UNAVAILABLE"

    # --- Metrics ------------------------------------------------------
    BITRATE_DEVIATION = "ERR_BITRATE_DEVIATION"
    ALIGNMENT_EXCEEDED = "ERR_ALIGNMENT_EXCEEDED"


# Codes that indicate a packet could not contribute audio.
DATA_LOSS_CODES = frozenset(
    {
        Code.TRUNCATED_PACKET,
        Code.TRUNCATED_RECORD,
        Code.LOST_PACKET,
        Code.MALFORMED_HEADER,
        Code.BOUNDARY_OVERLAP,
    }
)

# Codes that are expected on damaged input and do not by themselves fail a run.
RECOVERABLE_CODES = frozenset(
    {
        Code.DUPLICATE_PACKET,
        Code.REORDERED_PACKET,
        Code.TRUNCATED_PACKET,
        Code.TRUNCATED_RECORD,
        Code.RECORD_RESYNC,
        Code.LOST_PACKET,
        Code.SEQUENCE_WRAP,
        Code.TIMESTAMP_WRAP,
        Code.TIMESTAMP_DISCONTINUITY,
        Code.RTCP_MISSING,
        Code.CLOCK_DRIFT,
        Code.METADATA_MISSING,
        Code.METADATA_CONFLICT,
        Code.CLIPPED_SAMPLE,
        Code.SILENT_BLOCK,
        Code.SHORT_CHUNK,
        Code.EMPTY_STREAM,
    }
)


class Severity(str, Enum):
    INFO = "info"
    WARNING = "warning"
    ERROR = "error"
    FATAL = "fatal"


_SEVERITY: dict[Code, Severity] = {
    Code.DUPLICATE_PACKET: Severity.WARNING,
    Code.REORDERED_PACKET: Severity.WARNING,
    Code.SEQUENCE_WRAP: Severity.INFO,
    Code.TIMESTAMP_WRAP: Severity.INFO,
    Code.RTCP_MISSING: Severity.WARNING,
    Code.CLOCK_DRIFT: Severity.WARNING,
    Code.METADATA_MISSING: Severity.WARNING,
    Code.METADATA_CONFLICT: Severity.WARNING,
    Code.CLIPPED_SAMPLE: Severity.WARNING,
    Code.SILENT_BLOCK: Severity.INFO,
    Code.SHORT_CHUNK: Severity.INFO,
    Code.EMPTY_STREAM: Severity.WARNING,
    Code.LOST_PACKET: Severity.ERROR,
    Code.TRUNCATED_PACKET: Severity.ERROR,
    Code.TRUNCATED_RECORD: Severity.ERROR,
    Code.RECORD_RESYNC: Severity.WARNING,
    Code.BOUNDARY_OVERLAP: Severity.ERROR,
    Code.TIMESTAMP_DISCONTINUITY: Severity.ERROR,
    Code.RTCP_MALFORMED: Severity.ERROR,
    Code.UNSUPPORTED_LAYOUT: Severity.FATAL,
    Code.NONFINITE_SAMPLE: Severity.ERROR,
    Code.VIDEO_TIME_SHIFT: Severity.ERROR,
    Code.CHUNK_HASH_MISMATCH: Severity.ERROR,
    Code.INDEX_INCONSISTENT: Severity.ERROR,
    Code.CHUNK_MISSING: Severity.ERROR,
    Code.CHECKPOINT_INVALID: Severity.FATAL,
    Code.INPUT_CHANGED: Severity.FATAL,
    Code.BITRATE_DEVIATION: Severity.ERROR,
    Code.ALIGNMENT_EXCEEDED: Severity.ERROR,
}


def severity_of(code: Code) -> Severity:
    return _SEVERITY.get(code, Severity.ERROR)


@dataclass(frozen=True)
class Diagnostic:
    """One structured observation, serialisable for the JSON summary."""

    code: Code
    message: str
    where: str = ""
    detail: dict[str, Any] = field(default_factory=dict)

    @property
    def severity(self) -> Severity:
        return severity_of(self.code)

    def to_json(self) -> dict[str, Any]:
        return {
            "code": self.code.value,
            "severity": self.severity.value,
            "message": self.message,
            "where": self.where,
            "detail": dict(self.detail),
        }


class MediaError(Exception):
    """Fatal or per-session error carrying a shared :class:`Code`."""

    def __init__(
        self,
        code: Code,
        message: str,
        *,
        where: str = "",
        detail: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(f"{code.value}: {message}")
        self.code = code
        self.message = message
        self.where = where
        self.detail = dict(detail or {})

    def diagnostic(self) -> Diagnostic:
        return Diagnostic(self.code, self.message, self.where, self.detail)

    def to_json(self) -> dict[str, Any]:
        return self.diagnostic().to_json()


class ParamError(MediaError):
    """Invalid CLI / configuration argument (exit code 2)."""


class InputError(MediaError):
    """Unreadable or unsupported input (exit code 1)."""


class MetricError(MediaError):
    """Run completed but acceptance metrics were not met (exit code 3)."""


class Interrupted(MediaError):
    """Recoverable, checkpointed interruption (exit code 3)."""


class DiagnosticCollector:
    """Counts codes and keeps a bounded, deterministic sample of messages."""

    def __init__(self, limit: int = 200) -> None:
        self.limit = limit
        self.counts: dict[Code, int] = {}
        self.samples: list[Diagnostic] = []

    def add(self, diag: Diagnostic) -> None:
        self.counts[diag.code] = self.counts.get(diag.code, 0) + 1
        if len(self.samples) < self.limit:
            self.samples.append(diag)

    def add_code(
        self,
        code: Code,
        message: str,
        *,
        where: str = "",
        detail: dict[str, Any] | None = None,
    ) -> None:
        self.add(Diagnostic(code, message, where, detail or {}))

    def extend(self, diags: Iterable[Diagnostic]) -> None:
        for d in diags:
            self.add(d)

    def count(self, code: Code) -> int:
        return self.counts.get(code, 0)

    def to_json(self) -> dict[str, Any]:
        codes = {
            code.value: n
            for code, n in sorted(self.counts.items(), key=lambda kv: kv[0].value)
        }
        return {
            "codes": codes,
            "samples": [d.to_json() for d in self.samples],
        }
