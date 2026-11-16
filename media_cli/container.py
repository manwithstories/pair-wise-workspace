"""Container opening and video stream probing."""

from __future__ import annotations

from contextlib import contextmanager
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Iterator

import av


class MediaOpenError(RuntimeError):
    """The input cannot be opened or has no usable video stream."""


@dataclass(frozen=True)
class ContainerInfo:
    path: str
    format_name: str
    video_stream_index: int
    codec_name: str
    width: int
    height: int
    time_base: Fraction
    average_rate: Fraction | None
    start_time: int | None
    duration_seconds: float | None
    packet_count: int
    has_pts: bool
    has_dts: bool
    probe_truncated: bool = False

    @property
    def time_base_text(self) -> str:
        return f"{self.time_base.numerator}/{self.time_base.denominator}"

    @property
    def average_rate_fps(self) -> float | None:
        if not self.average_rate:
            return None
        return float(self.average_rate)


def _as_fraction(value: object) -> Fraction:
    if value is None:
        return Fraction(0, 1)
    return Fraction(value.numerator, value.denominator)


def select_video_stream(container: av.container.InputContainer, stream_index: int | None = None):
    if stream_index is not None:
        for stream in container.streams:
            if stream.index == stream_index and stream.type == "video":
                return stream
        raise MediaOpenError(f"stream {stream_index} is not a video stream")

    for stream in container.streams.video:
        return stream
    raise MediaOpenError("input has no video stream")


@contextmanager
def open_video(
    path: str | Path,
    stream_index: int | None = None,
) -> Iterator[tuple[av.container.InputContainer, object, Fraction]]:
    """Open a media file and yield its container, selected stream and time base."""

    media_path = Path(path)
    if not media_path.is_file():
        raise MediaOpenError(f"input file does not exist: {media_path}")
    try:
        container = av.open(str(media_path))
    except (OSError, av.error.FFmpegError) as exc:
        raise MediaOpenError(f"cannot open {media_path}: {exc}") from exc

    try:
        stream = select_video_stream(container, stream_index)
        time_base = _as_fraction(stream.time_base)
        if time_base <= 0:
            time_base = Fraction(1, 1)
        yield container, stream, time_base
    finally:
        container.close()


def inspect_container(path: str | Path, stream_index: int | None = None) -> ContainerInfo:
    """Read stream parameters and count packets without retaining them."""

    media_path = Path(path)
    with open_video(media_path, stream_index) as (container, stream, time_base):
        packet_count = 0
        has_pts = False
        has_dts = False
        truncated = False
        try:
            for packet in container.demux(stream):
                if packet.size == 0:
                    continue
                packet_count += 1
                has_pts = has_pts or packet.pts is not None
                has_dts = has_dts or packet.dts is not None
        except av.error.FFmpegError:
            truncated = packet_count > 0

        duration_seconds = None
        if stream.duration is not None:
            duration_seconds = float(stream.duration * time_base)
        elif container.duration is not None:
            duration_seconds = container.duration / 1_000_000

        rate = getattr(stream, "average_rate", None)
        average_rate = _as_fraction(rate) if rate else None
        codec = stream.codec_context
        return ContainerInfo(
            path=str(media_path),
            format_name=container.format.name or "unknown",
            video_stream_index=stream.index,
            codec_name=codec.name or "unknown",
            width=codec.width or 0,
            height=codec.height or 0,
            time_base=time_base,
            average_rate=average_rate,
            start_time=stream.start_time,
            duration_seconds=duration_seconds,
            packet_count=packet_count,
            has_pts=has_pts,
            has_dts=has_dts,
            probe_truncated=truncated,
        )
