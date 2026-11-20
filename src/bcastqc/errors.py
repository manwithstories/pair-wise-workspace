"""Serialisable error taxonomy and recovery policy.

Every failure that can reach the user is expressed as a :class:`QcError`
subclass.  Each error carries:

``category``
    A stable identifier (string enum) that scripts may match on.
``recoverable``
    Whether the pipeline may continue with a documented degradation.
``strategy``
    The recovery action the pipeline is allowed to take (``NONE`` for fatal
    errors).
``exit_code``
    The process exit status mandated by the CLI contract:
    ``2`` invalid input or timeline, ``3`` unrecoverable media error,
    ``4`` resource or output error.
``details``
    JSON-safe context (byte offsets, timestamps, offending values).

The taxonomy is deliberately data-driven so the CLI, the reports and the tests
agree on what "recoverable" means.  Recovery is always *recorded*: a frame that
was synthesised or resynchronised is never reported as a clean decode.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Mapping

from . import EXIT_INPUT_INVALID, EXIT_MEDIA_UNRECOVERABLE, EXIT_RESOURCE

__all__ = [
    "RecoveryStrategy",
    "ErrorCategory",
    "ErrorSpec",
    "CATEGORY_SPECS",
    "QcError",
    "InputError",
    "TimelineError",
    "BitstreamError",
    "DecodeError",
    "ResourceError",
    "OutputError",
    "CancelledError",
    "make_error",
]


class RecoveryStrategy(str, Enum):
    """What the pipeline may do after an error of a given category."""

    NONE = "none"                 # fatal
    SCAN = "scan"                 # locate the next valid access unit
    RESYNC = "resync"             # jump to the next keyframe / independent unit
    SKIP = "skip"                 # drop the offending unit, keep the rest
    CLAMP = "clamp"               # clip the value into the legal range
    TRUNCATE = "truncate"         # stop early but keep earlier output


class ErrorCategory(str, Enum):
    # ---- index / bitstream structure ----
    INDEX_OUT_OF_RANGE = "index.out_of_range"
    INDEX_DUPLICATE = "index.duplicate"
    INDEX_OUT_OF_ORDER = "index.out_of_order"
    INDEX_MALFORMED = "index.malformed"
    STREAM_TRUNCATED = "stream.truncated"
    STREAM_CORRUPT = "stream.corrupt_access_unit"
    STREAM_EMPTY = "stream.empty"
    PACKET_LOSS = "stream.packet_loss"
    TIMESTAMP_JUMP = "stream.timestamp_jump"
    KEYFRAME_MISSING = "stream.keyframe_missing"
    # ---- input validation ----
    PATH_ESCAPE = "input.path_escape"
    LIMIT_EXCEEDED = "input.limit_exceeded"
    CHECKSUM_MISMATCH = "input.checksum_mismatch"
    MANIFEST_INVALID = "input.manifest_invalid"
    AUDIO_FORMAT = "input.audio_format"
    # ---- timeline ----
    TIMELINE_INVALID = "timeline.invalid"
    TIMELINE_CYCLE = "timeline.cycle"
    TRACK_UNREFERENCED = "timeline.track_unreferenced"
    OVERLAP = "timeline.overlap"
    # ---- decoding / rendering ----
    DECODE_FAILURE = "decode.failure"
    DECODER_UNAVAILABLE = "decode.unavailable"
    RENDER_FAILURE = "render.failure"
    # ---- runtime ----
    QUEUE_TIMEOUT = "runtime.consumer_timeout"
    CANCELLED = "runtime.cancelled"
    RESOURCE_EXHAUSTED = "runtime.resource_exhausted"
    OUTPUT_FAILURE = "runtime.output_failure"


@dataclass(frozen=True)
class ErrorSpec:
    recoverable: bool
    strategy: RecoveryStrategy
    exit_code: int


_SPECS: dict[ErrorCategory, ErrorSpec] = {
    ErrorCategory.INDEX_OUT_OF_RANGE: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_INPUT_INVALID),
    ErrorCategory.INDEX_DUPLICATE: ErrorSpec(True, RecoveryStrategy.SCAN, EXIT_INPUT_INVALID),
    ErrorCategory.INDEX_OUT_OF_ORDER: ErrorSpec(True, RecoveryStrategy.SCAN, EXIT_INPUT_INVALID),
    ErrorCategory.INDEX_MALFORMED: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_INPUT_INVALID),
    ErrorCategory.STREAM_TRUNCATED: ErrorSpec(True, RecoveryStrategy.TRUNCATE, EXIT_MEDIA_UNRECOVERABLE),
    ErrorCategory.STREAM_CORRUPT: ErrorSpec(True, RecoveryStrategy.SCAN, EXIT_MEDIA_UNRECOVERABLE),
    ErrorCategory.STREAM_EMPTY: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_INPUT_INVALID),
    ErrorCategory.PACKET_LOSS: ErrorSpec(True, RecoveryStrategy.SKIP, EXIT_MEDIA_UNRECOVERABLE),
    ErrorCategory.TIMESTAMP_JUMP: ErrorSpec(True, RecoveryStrategy.CLAMP, EXIT_MEDIA_UNRECOVERABLE),
    ErrorCategory.KEYFRAME_MISSING: ErrorSpec(True, RecoveryStrategy.RESYNC, EXIT_MEDIA_UNRECOVERABLE),
    ErrorCategory.PATH_ESCAPE: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_INPUT_INVALID),
    ErrorCategory.LIMIT_EXCEEDED: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_INPUT_INVALID),
    ErrorCategory.CHECKSUM_MISMATCH: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_INPUT_INVALID),
    ErrorCategory.MANIFEST_INVALID: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_INPUT_INVALID),
    ErrorCategory.AUDIO_FORMAT: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_INPUT_INVALID),
    ErrorCategory.TIMELINE_INVALID: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_INPUT_INVALID),
    ErrorCategory.TIMELINE_CYCLE: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_INPUT_INVALID),
    ErrorCategory.TRACK_UNREFERENCED: ErrorSpec(True, RecoveryStrategy.SKIP, EXIT_INPUT_INVALID),
    ErrorCategory.OVERLAP: ErrorSpec(True, RecoveryStrategy.CLAMP, EXIT_INPUT_INVALID),
    ErrorCategory.DECODE_FAILURE: ErrorSpec(True, RecoveryStrategy.RESYNC, EXIT_MEDIA_UNRECOVERABLE),
    ErrorCategory.DECODER_UNAVAILABLE: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_MEDIA_UNRECOVERABLE),
    ErrorCategory.RENDER_FAILURE: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_MEDIA_UNRECOVERABLE),
    ErrorCategory.QUEUE_TIMEOUT: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_RESOURCE),
    ErrorCategory.CANCELLED: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_RESOURCE),
    ErrorCategory.RESOURCE_EXHAUSTED: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_RESOURCE),
    ErrorCategory.OUTPUT_FAILURE: ErrorSpec(False, RecoveryStrategy.NONE, EXIT_RESOURCE),
}

CATEGORY_SPECS: Mapping[ErrorCategory, ErrorSpec] = _SPECS


def _json_safe(value: Any) -> Any:
    """Coerce arbitrary detail values into JSON-serialisable primitives."""
    if value is None or isinstance(value, (bool, int, str)):
        return value
    if isinstance(value, float):
        return value
    if isinstance(value, (list, tuple)):
        return [_json_safe(v) for v in value]
    if isinstance(value, dict):
        return {str(k): _json_safe(v) for k, v in value.items()}
    return str(value)


@dataclass
class QcError(Exception):
    """Base class for all recoverable and fatal pipeline errors."""

    category: ErrorCategory
    message: str
    details: dict[str, Any] = field(default_factory=dict)
    cause: str | None = None

    def __post_init__(self) -> None:
        Exception.__init__(self, self.message)

    # -- policy ---------------------------------------------------------
    @property
    def spec(self) -> ErrorSpec:
        return CATEGORY_SPECS[self.category]

    @property
    def recoverable(self) -> bool:
        return self.spec.recoverable

    @property
    def strategy(self) -> RecoveryStrategy:
        return self.spec.strategy

    @property
    def exit_code(self) -> int:
        return self.spec.exit_code

    # -- serialisation --------------------------------------------------
    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "category": self.category.value,
            "message": self.message,
            "recoverable": self.recoverable,
            "strategy": self.strategy.value,
            "exit_code": self.exit_code,
        }
        if self.details:
            payload["details"] = _json_safe(self.details)
        if self.cause:
            payload["cause"] = self.cause
        return payload

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "QcError":
        return cls(
            category=ErrorCategory(payload["category"]),
            message=str(payload["message"]),
            details=dict(payload.get("details") or {}),
            cause=payload.get("cause"),
        )

    def __str__(self) -> str:
        head = f"[{self.category.value}] {self.message}"
        if self.details:
            detail = ", ".join(f"{k}={v!r}" for k, v in sorted(self.details.items()))
            head += f" ({detail})"
        return head


class InputError(QcError):
    """Invalid or out-of-policy input (paths, manifests, checksums)."""


class TimelineError(QcError):
    """The timeline graph or track references are invalid."""


class BitstreamError(QcError):
    """Structural problems in raw bitstreams and keyframe indexes."""


class DecodeError(QcError):
    """Demuxing or decoding failures."""


class ResourceError(QcError):
    """Queue back-pressure, timeouts, cancellation, output I/O."""


class OutputError(ResourceError):
    """Disk or manifest write failures."""


class CancelledError(ResourceError):
    """Cooperative cancellation was requested."""


def make_error(category: ErrorCategory, message: str, **details: Any) -> QcError:
    """Create the most specific error subclass for ``category``."""
    cls: type[QcError]
    if category in {
        ErrorCategory.INDEX_OUT_OF_RANGE,
        ErrorCategory.INDEX_DUPLICATE,
        ErrorCategory.INDEX_OUT_OF_ORDER,
        ErrorCategory.INDEX_MALFORMED,
        ErrorCategory.STREAM_TRUNCATED,
        ErrorCategory.STREAM_CORRUPT,
        ErrorCategory.STREAM_EMPTY,
        ErrorCategory.PACKET_LOSS,
        ErrorCategory.TIMESTAMP_JUMP,
        ErrorCategory.KEYFRAME_MISSING,
    }:
        cls = BitstreamError
    elif category in {
        ErrorCategory.PATH_ESCAPE,
        ErrorCategory.LIMIT_EXCEEDED,
        ErrorCategory.CHECKSUM_MISMATCH,
        ErrorCategory.MANIFEST_INVALID,
        ErrorCategory.AUDIO_FORMAT,
    }:
        cls = InputError
    elif category in {
        ErrorCategory.TIMELINE_INVALID,
        ErrorCategory.TIMELINE_CYCLE,
        ErrorCategory.TRACK_UNREFERENCED,
        ErrorCategory.OVERLAP,
    }:
        cls = TimelineError
    elif category in {
        ErrorCategory.DECODE_FAILURE,
        ErrorCategory.DECODER_UNAVAILABLE,
        ErrorCategory.RENDER_FAILURE,
    }:
        cls = DecodeError
    elif category is ErrorCategory.CANCELLED:
        cls = CancelledError
    elif category is ErrorCategory.OUTPUT_FAILURE:
        cls = OutputError
    else:
        cls = ResourceError
    return cls(category=category, message=message, details=dict(details))
