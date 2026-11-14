"""Container and packet inspection that never invokes the decoder."""

from __future__ import annotations

from fractions import Fraction
from typing import Iterator, Optional

import av

from .errors import InputOpenError, NoVideoStreamError, UnsupportedCodecError
from .models import MediaInfo, PacketMeta

SUPPORTED_CODECS = frozenset({"h264", "hevc"})


def _rate(stream: av.VideoStream) -> Optional[Fraction]:
    for value in (
        getattr(stream, "base_rate", None),
        getattr(stream, "guessed_rate", None),
        getattr(stream, "average_rate", None),
    ):
        if value:
            try:
                return Fraction(value)
            except (TypeError, ValueError, ZeroDivisionError):
                pass
    return None


def _duration(container: av.container.InputContainer, stream: av.VideoStream) -> Optional[Fraction]:
    if container.duration is not None:
        return Fraction(int(container.duration), 1_000_000)
    if stream.duration is not None and stream.time_base is not None:
        return Fraction(int(stream.duration)) * Fraction(stream.time_base)
    return None


def open_video(path: str) -> tuple[av.container.InputContainer, av.VideoStream, MediaInfo]:
    """Open a media file and validate that its first video stream is supported."""
    try:
        container = av.open(path)
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        raise InputOpenError(f"cannot open {path!r}: {exc}") from exc

    try:
        stream = next(iter(container.streams.video), None)
        if stream is None:
            raise NoVideoStreamError(f"{path!r} has no video stream")
        codec_name = stream.codec_context.name
        if codec_name not in SUPPORTED_CODECS:
            raise UnsupportedCodecError(
                f"unsupported video codec {codec_name!r}; expected H.264 or H.265"
            )
        if stream.time_base is None:
            raise InputOpenError(f"{path!r} video stream has no time base")
        info = MediaInfo(
            path=path,
            format_name=container.format.name or "unknown",
            stream_index=stream.index,
            codec_name=codec_name,
            time_base=Fraction(stream.time_base),
            width=int(stream.codec_context.width or 0),
            height=int(stream.codec_context.height or 0),
            average_rate=_rate(stream),
            duration=_duration(container, stream),
        )
        return container, stream, info
    except Exception:
        container.close()
        raise


def probe_file(path: str) -> MediaInfo:
    """Read container and video-stream metadata without decoding a frame."""
    container, _stream, info = open_video(path)
    container.close()
    return info


def packet_meta(packet: av.Packet, seq: int, time_base: Fraction) -> PacketMeta:
    return PacketMeta(
        seq=seq,
        stream_index=packet.stream_index,
        pts=packet.pts,
        dts=packet.dts,
        duration=packet.duration,
        time_base=time_base,
        is_keyframe=bool(packet.is_keyframe),
        is_discard=bool(packet.is_discard),
        size=int(packet.size),
    )


def iter_packet_meta(path: str) -> Iterator[PacketMeta]:
    """Yield video packet metadata in demux (decode) order, one at a time."""
    container, stream, info = open_video(path)
    seq = 0
    try:
        for packet in container.demux(stream):
            if not packet.size:
                continue
            yield packet_meta(packet, seq, info.time_base)
            seq += 1
    finally:
        container.close()


def count_video_packets(path: str) -> int:
    return sum(1 for _ in iter_packet_meta(path))
