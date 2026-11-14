"""Packet-by-packet video decode.

Only frame metadata (pts/dts/pict_type) is recorded; pixel buffers are
dropped immediately so memory stays flat on long GOP sources.
"""

from __future__ import annotations

from dataclasses import dataclass, field

import av

from .probe import ProbeInfo


@dataclass
class FrameRecord:
    """One decoded frame, in decode order."""

    decode_index: int
    pts: float | None
    dts: float | None
    pict_type: str
    key_frame: bool


@dataclass
class DecodeResult:
    records: list = field(default_factory=list)
    frames_decoded: int = 0
    expected_frames: int | None = None
    truncated: bool = False


def _expected_frames(info: ProbeInfo) -> int | None:
    if info.nb_frames > 0:
        return info.nb_frames
    if info.duration is not None and info.avg_frame_rate > 0:
        seconds = float(info.duration * info.time_base)
        return int(round(seconds * float(info.avg_frame_rate)))
    return None


def decode_frames(container, stream, info: ProbeInfo) -> DecodeResult:
    """Decode every packet of the video stream, keeping metadata only."""
    result = DecodeResult(expected_frames=_expected_frames(info))
    ctx = stream.codec_context
    index = 0

    def _collect(frames):
        nonlocal index
        for frame in frames:
            result.records.append(
                FrameRecord(
                    decode_index=index,
                    pts=None if frame.pts is None else float(frame.pts),
                    dts=None if frame.dts is None else float(frame.dts),
                    pict_type=frame.pict_type.name if frame.pict_type else "NONE",
                    key_frame=bool(frame.key_frame),
                )
            )
            index += 1
            # Pixels are never converted or stored; the frame object is
            # released on the next loop iteration.

    try:
        for packet in container.demux(stream):
            if packet.size == 0:
                continue
            try:
                _collect(packet.decode())
            except av.AVError:
                result.truncated = True
                break
    except av.AVError:
        # Truncated tail: keep the frames decoded so far.
        result.truncated = True

    # Flush frames still held by the decoder at end of stream.
    try:
        _collect(ctx.decode(None))
    except av.AVError:
        result.truncated = True

    result.frames_decoded = len(result.records)
    if (
        result.expected_frames is not None
        and result.frames_decoded < result.expected_frames
    ):
        result.truncated = True
    return result
