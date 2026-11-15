"""Decode layer: packet-by-packet decoding with bad-packet tolerance.

Frames are emitted in *decode order*. Display-order reordering lives in
:mod:`cinecheck.reorder`. A packet the decoder refuses ends decoding; the
frames already produced are retained so truncated files still report
partial statistics instead of failing outright.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Iterator

import av

from .demux import PacketRecord


@dataclass(frozen=True)
class DecodedFrame:
    """One decoded frame with the timing context of its source packet."""

    order: int
    pts: int | None
    dts: int | None
    keyframe: bool
    pict_type: str
    packet_index: int


def _pict_name(frame: av.video.frame.VideoFrame) -> str:
    value = frame.pict_type
    name = getattr(value, "name", None)
    return str(name or value or "?").upper()


def _drain(
    decoder,
    first: av.video.frame.VideoFrame | None,
    packet: PacketRecord | None,
    order: int,
) -> tuple[list[DecodedFrame], int]:
    """Pull every frame the decoder currently holds (PyAV 13 yields one)."""

    emitted: list[DecodedFrame] = []
    frame = first
    while frame is not None:
        emitted.append(
            DecodedFrame(
                order=order,
                pts=frame.pts,
                dts=getattr(packet, "dts", None),
                keyframe=bool(frame.key_frame),
                pict_type=_pict_name(frame),
                packet_index=getattr(packet, "index", -1),
            )
        )
        order += 1
        try:
            frame = decoder.send(None)
        except (StopIteration, av.FFmpegError):
            frame = None
            break
    return emitted, order


def decode_packets(
    container: av.container.InputContainer,
    stream: av.video.stream.VideoStream,
    packets: Iterator[PacketRecord],
) -> Iterator[DecodedFrame]:
    """Decode *packets* one at a time, yielding frames in decode order.

    The PyAV decode generator is primed with ``send(None)`` and then fed a
    packet at a time. A single send can surface one frame while more stay
    buffered, so each packet is followed by a drain loop. On
    :class:`av.FFmpegError` (corrupt packet or truncated tail) iteration
    stops cleanly; frames already decoded are preserved by the caller.
    """

    decoder = container.decode(stream)
    order = 0
    try:
        primed = decoder.send(None)
    except StopIteration:
        return
    buffered, order = _drain(decoder, primed, None, order)
    for frame in buffered:
        yield frame

    for packet in packets:
        try:
            first = decoder.send(packet)
        except av.FFmpegError:
            decoder.close()
            return
        except StopIteration:
            return
        emitted, order = _drain(decoder, first, packet, order)
        for frame in emitted:
            yield frame

    try:
        while True:
            frame = decoder.send(None)
            if frame is None:
                break
            emitted, order = _drain(decoder, frame, None, order)
            for item in emitted:
                yield item
    except (av.FFmpegError, StopIteration):
        pass
    decoder.close()
