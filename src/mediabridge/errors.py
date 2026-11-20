"""Locatable, recoverable error model shared by every stage.

Two families matter operationally:

``RecoverablePacketError``
    A single packet (or a small byte range) is bad.  The pipeline skips the
    damaged interval, records exactly where it happened and keeps going.  This
    is what a truncated tail, a zeroed AAC frame or a flipped H.264 NAL looks
    like.

``StructuralError``
    The container itself cannot be trusted: no usable streams, an unsupported
    codec, an input that changed under us, a version mismatch, an index that
    points outside the file.  Retrying is pointless and the run must fail with
    a dedicated exit code.

Every error carries enough context to name the offending position:
``track_index``, ``packet_index`` (per-track, zero based), ``stream_index`` and
the raw timestamps that were read from the container.  ``exit_code`` maps the
error onto the CLI contract (2 = unrecoverable input, 3 = resume/index
mismatch, 4 = internal failure).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


EXIT_OK = 0
EXIT_UNRECOVERABLE = 2
EXIT_RESUME_MISMATCH = 3
EXIT_INTERNAL = 4


class MediaBridgeError(Exception):
    """Base class with a stable, machine readable ``code``."""

    code = "mediabridge_error"
    exit_code = EXIT_INTERNAL
    reason = "internal processing failure"

    def __init__(
        self,
        message: str,
        *,
        track_index: int | None = None,
        packet_index: int | None = None,
        stream_index: int | None = None,
        dts: int | None = None,
        pts: int | None = None,
        time_base: tuple[int, int] | None = None,
        context: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.message = message
        self.track_index = track_index
        self.packet_index = packet_index
        self.stream_index = stream_index
        self.dts = dts
        self.pts = pts
        self.time_base = time_base
        self.context = dict(context or {})

    # -- serialisation -----------------------------------------------------
    def as_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
            "reason": self.reason,
            "exit_code": self.exit_code,
        }
        for name in ("track_index", "packet_index", "stream_index", "dts", "pts"):
            value = getattr(self, name)
            if value is not None:
                payload[name] = value
        if self.time_base is not None:
            payload["time_base"] = list(self.time_base)
        if self.context:
            payload["context"] = self.context
        return payload

    def position(self) -> dict[str, Any]:
        """Compact location used in summaries and checkpoint records."""
        out: dict[str, Any] = {}
        for name in ("track_index", "stream_index", "packet_index", "dts", "pts"):
            value = getattr(self, name)
            if value is not None:
                out[name] = value
        if self.time_base is not None:
            out["time_base"] = list(self.time_base)
        return out

    def __str__(self) -> str:  # pragma: no cover - cosmetic
        return self.message


class RecoverablePacketError(MediaBridgeError):
    """A damaged packet that can be skipped without aborting the run."""

    code = "recoverable_packet"
    exit_code = EXIT_OK
    reason = "damaged packet skipped"

    def __init__(self, message: str, *, skip_until: int | None = None, **kwargs) -> None:
        super().__init__(message, **kwargs)
        self.skip_until = skip_until


class StructuralError(MediaBridgeError):
    """The container or its metadata cannot be used."""

    code = "structural_error"
    exit_code = EXIT_UNRECOVERABLE
    reason = "container structure is unusable"


class UnsupportedCodecError(StructuralError):
    code = "unsupported_codec"
    reason = "codec is not supported by this pipeline"


class TruncatedInputError(StructuralError):
    code = "truncated_input"
    reason = "input ends before the container index is complete"


class InputChangedError(StructuralError):
    """Resume refused: the source no longer matches the checkpoint."""

    code = "input_changed"
    exit_code = EXIT_RESUME_MISMATCH
    reason = "input fingerprint does not match checkpoint"


class CheckpointMismatchError(StructuralError):
    code = "checkpoint_mismatch"
    exit_code = EXIT_RESUME_MISMATCH
    reason = "checkpoint is incompatible with this run"


class IndexIncompatibleError(StructuralError):
    code = "index_incompatible"
    exit_code = EXIT_RESUME_MISMATCH
    reason = "index is corrupt, incomplete or written by another version"


class IndexWriteError(MediaBridgeError):
    code = "index_write_error"
    exit_code = EXIT_INTERNAL
    reason = "could not commit index chunk"


class TimestampToleranceError(MediaBridgeError):
    """A timestamp discontinuity exceeds the configured repair tolerance."""

    code = "timestamp_tolerance_exceeded"
    exit_code = EXIT_UNRECOVERABLE
    reason = "timestamp discontinuity exceeds tolerance"

    def __init__(self, message: str, *, deviation: float | None = None, **kwargs) -> None:
        super().__init__(message, **kwargs)
        self.deviation = deviation

    def as_dict(self) -> dict[str, Any]:
        payload = super().as_dict()
        if self.deviation is not None:
            payload["deviation_seconds"] = self.deviation
        return payload


@dataclass
class DamagedInterval:
    """Bookkeeping for skipped packets, used by summaries and the index."""

    track_index: int
    start_packet: int
    end_packet: int
    reason: str
    dts: int | None = None
    pts: int | None = None
    recovered: int = 0
    notes: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        payload = {
            "track_index": self.track_index,
            "start_packet": self.start_packet,
            "end_packet": self.end_packet,
            "packets": self.end_packet - self.start_packet + 1,
            "reason": self.reason,
        }
        if self.dts is not None:
            payload["dts"] = self.dts
        if self.pts is not None:
            payload["pts"] = self.pts
        if self.recovered:
            payload["resynced_after_packets"] = self.recovered
        if self.notes:
            payload["notes"] = self.notes
        return payload
