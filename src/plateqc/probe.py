"""Container probing: open input, select the video stream, read codec params."""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction

import av


class ProbeError(Exception):
    """Input cannot be opened or has no usable video stream (exit code 3)."""


@dataclass
class ProbeInfo:
    path: str
    stream_index: int
    codec_name: str
    width: int
    height: int
    pix_fmt: str
    time_base: Fraction
    avg_frame_rate: Fraction
    has_b_frames: bool
    nb_frames: int
    duration: int | None
    nb_streams: int


def open_container(path: str) -> av.container.InputContainer:
    try:
        return av.open(path)
    except av.AVError as exc:
        raise ProbeError(f"cannot open input {path!r}: {exc}") from exc
    except OSError as exc:
        raise ProbeError(f"cannot open input {path!r}: {exc}") from exc


def select_stream(container, stream_index: int | None):
    if stream_index is not None:
        streams = list(container.streams)
        if stream_index < 0 or stream_index >= len(streams):
            raise ProbeError(
                f"stream index {stream_index} out of range (0..{len(streams) - 1})"
            )
        stream = streams[stream_index]
        if stream.type != "video":
            raise ProbeError(f"stream {stream_index} is not a video stream")
        return stream
    videos = list(container.streams.video)
    if not videos:
        raise ProbeError("input has no video stream")
    return videos[0]


def probe(path: str, stream_index: int | None = None):
    """Open the container and return (container, video_stream, ProbeInfo)."""
    container = open_container(path)
    try:
        stream = select_stream(container, stream_index)
    except ProbeError:
        container.close()
        raise
    cc = stream.codec_context
    rate = stream.average_rate or Fraction(0, 1)
    info = ProbeInfo(
        path=path,
        stream_index=stream.index,
        codec_name=cc.name or "unknown",
        width=cc.width or 0,
        height=cc.height or 0,
        pix_fmt=cc.pix_fmt or "",
        time_base=stream.time_base,
        avg_frame_rate=Fraction(rate),
        has_b_frames=bool(cc.has_b_frames),
        nb_frames=int(stream.frames or 0),
        duration=stream.duration,
        nb_streams=len(list(container.streams)),
    )
    return container, stream, info
