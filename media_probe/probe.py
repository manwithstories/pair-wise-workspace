"""Open a media file, select the video stream, expose codec/timing facts."""

from __future__ import annotations

import os
from dataclasses import dataclass
from fractions import Fraction

import av

SUPPORTED_CODECS = ("h264", "hevc")


class ProbeError(Exception):
    """Raised when a file cannot be probed (maps to CLI exit code 3)."""


@dataclass
class ProbeResult:
    path: str
    stream_index: int
    codec: str
    width: int
    height: int
    time_base: Fraction
    nominal_fps: float | None
    duration_sec: float | None
    frame_count_hint: int | None


def _fps_from_rate(rate) -> float | None:
    if rate is None:
        return None
    try:
        value = float(rate)
    except (TypeError, ZeroDivisionError):
        return None
    return value if value > 0 else None


def probe(path: str) -> ProbeResult:
    """Open *path* and return static facts about the first supported video stream.

    Raises ProbeError when the file cannot be opened, has no video stream, or
    the video codec is not H.264/HEVC.
    """
    if not os.path.isfile(path):
        raise ProbeError(f"file not found: {path}")
    try:
        container = av.open(path)
    except av.error.FFmpegError as exc:
        raise ProbeError(f"cannot open {path}: {exc}") from exc
    try:
        stream = None
        for candidate in container.streams.video:
            if candidate.codec_context.name in SUPPORTED_CODECS:
                stream = candidate
                break
        if stream is None:
            if container.streams.video:
                raise ProbeError(
                    f"unsupported video codec: {container.streams.video[0].codec_context.name}"
                )
            raise ProbeError("no video stream found")
        ctx = stream.codec_context
        nominal_fps = _fps_from_rate(stream.average_rate) or _fps_from_rate(
            stream.base_rate
        )
        duration_sec = None
        if stream.duration is not None and stream.time_base:
            duration_sec = float(stream.duration * stream.time_base)
        elif container.duration:
            duration_sec = container.duration / av.time_base
        return ProbeResult(
            path=path,
            stream_index=stream.index,
            codec=ctx.name,
            width=ctx.width or 0,
            height=ctx.height or 0,
            time_base=Fraction(stream.time_base),
            nominal_fps=nominal_fps,
            duration_sec=duration_sec,
            frame_count_hint=stream.frames or None,
        )
    finally:
        container.close()
