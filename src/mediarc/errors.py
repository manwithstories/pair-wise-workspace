"""Error model for mediarc.

Every error is *locatable*: it knows which file it came from, which stream
(if any) was being processed, which pipeline stage raised it, and where in the
source file the trouble was found.  Offsets are always expressed as byte
positions in the **source container file**, never in a decoded or remuxed
buffer.  When libavformat cannot tell us a byte position we record ``None`` and
the rendered diagnostic says ``offset=unknown`` rather than inventing a value.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable

#: Pipeline stages, in execution order.  A stage name is part of the public
#: diagnostic contract, so we keep them as plain strings and validate early.
STAGES = ("generate", "probe", "timeline", "audio", "color", "archive", "verify")

#: Stable machine-readable error codes.  Each code maps to one line in the
#: terminal diagnostic; codes are never reused for a different meaning.
CODES: dict[str, str] = {
    "missing-stream": "mapped stream is absent from the container",
    "duplicate-mapping": "the same stream is mapped more than once",
    "type-mismatch": "mapped stream has the wrong media type",
    "stream-index-out-of-range": "mapped stream index does not exist",
    "probe-failed": "container could not be opened",
    "unsupported-container": "container format is not MOV, MP4 or MKV",
    "truncated-container": "container ends before its declared duration",
    "corrupt-packet": "a packet could not be demuxed or decoded",
    "negative-timestamp": "timestamp precedes the timeline origin",
    "non-monotonic-timestamp": "timestamp order regressed",
    "rounding-collision": "two frames collapsed onto one integer tick",
    "edit-out-of-range": "edit point lies outside the material",
    "edit-order": "edit points are not strictly increasing",
    "transition-mismatch": "a transition does not meet the timeline definition",
    "transition-overlap": "a transition overlap is invalid",
    "unsupported-rate": "sample rate is not one of 44100/48000/96000",
    "unsupported-layout": "channel layout is not one of mono/stereo/5.1",
    "audio-decode": "audio frames could not be decoded",
    "audio-loudness": "integrated loudness limit could not be met",
    "color-conflict": "color metadata disagrees between container and codec",
    "color-unsupported": "color value is not in the supported enumeration",
    "manifest-corrupt": "manifest is not valid JSON for this schema",
    "manifest-schema": "manifest fields are missing or mistyped",
    "stage-hash-mismatch": "resumed stage input changed since it was recorded",
    "stage-missing-input": "a later stage ran without its predecessor's output",
    "verification-failed": "a recorded hash or keyframe index did not verify",
    "keyframe-index-incomplete": "the source has no keyframe where the index says it should",
    "limit-exceeded": "a configured size/duration/parallelism limit was hit",
    "io": "filesystem operation failed",
}


@dataclass(slots=True)
class MediaError(Exception):
    """A failure with enough context to locate the offending bytes."""

    code: str
    message: str
    stage: str = "probe"
    path: str | None = None
    stream_index: int | None = None
    offset: int | None = None
    detail: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        if self.code not in CODES:
            raise ValueError(f"unknown error code: {self.code!r}")
        if self.stage not in STAGES:
            raise ValueError(f"unknown stage: {self.stage!r}")
        if self.offset is not None and self.offset < 0:
            # A negative source offset is never meaningful; treat it as unknown.
            self.offset = None
        Exception.__init__(self, self.message)

    # -- rendering ---------------------------------------------------------
    def render(self) -> str:
        """One-line, stable, greppable diagnostic."""
        parts = [f"error code={self.code}", f"stage={self.stage}"]
        parts.append(f"path={self.path}" if self.path else "path=unknown")
        parts.append(
            f"stream={self.stream_index}" if self.stream_index is not None else "stream=-"
        )
        parts.append(
            f"offset={self.offset}" if self.offset is not None else "offset=unknown"
        )
        parts.append(f"message={self.message}")
        return " ".join(parts)

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "stage": self.stage,
            "path": self.path,
            "stream_index": self.stream_index,
            "offset": self.offset,
            "message": self.message,
            "detail": self.detail,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "MediaError":
        return cls(
            code=str(data["code"]),
            message=str(data.get("message", "")),
            stage=str(data.get("stage", "probe")),
            path=data.get("path"),
            stream_index=data.get("stream_index"),
            offset=data.get("offset"),
            detail=dict(data.get("detail") or {}),
        )


def bridge(code: str, exc: BaseException, **kwargs: Any) -> MediaError:
    """Wrap a foreign exception (OSError, av.FFmpegError, ...) as MediaError."""
    return MediaError(code, f"{type(exc).__name__}: {exc}", **kwargs)


def make(code: str, message: str, **kwargs: Any) -> MediaError:
    return MediaError(code, message, **kwargs)


def minimal_offset(errors: Iterable[MediaError]) -> int | None:
    """Smallest known byte offset among errors (used for terminal summaries)."""
    known = [e.offset for e in errors if e.offset is not None]
    return min(known) if known else None
