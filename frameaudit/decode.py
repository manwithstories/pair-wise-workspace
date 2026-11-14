"""Streaming packet decode with per-packet fault isolation."""

from __future__ import annotations

from dataclasses import asdict, dataclass
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterator

import av

from frameaudit.errors import InputError, ProcessingError
from frameaudit.model import DecodedFrame
from frameaudit.stats import pict_type, plane_hash


@dataclass(slots=True)
class BadRegion:
    segment: int
    packet_idx: int | None
    kind: str
    message: str

    def as_dict(self) -> dict[str, Any]:
        return asdict(self)


def _frame_pts_us(frame: av.VideoFrame, stream: av.VideoStream) -> int | None:
    if frame.pts is None:
        return None
    time_base = frame.time_base or stream.time_base or Fraction(1, 1000)
    return round(frame.pts * time_base.numerator * 1_000_000 / time_base.denominator)


def _clean_error(exc: Exception) -> str:
    text = " ".join(str(exc).split())
    return text[:240] if text else type(exc).__name__


def _decoded_record(
    frame: av.VideoFrame,
    stream: av.VideoStream,
    decode_idx: int,
    segment: int,
) -> DecodedFrame:
    return DecodedFrame(
        decode_idx=decode_idx,
        pts_raw_us=_frame_pts_us(frame, stream),
        pict_type=pict_type(frame),
        keyframe=bool(frame.key_frame),
        plane_hash=plane_hash(frame),
        frame=frame,
        missing_pts=frame.pts is None,
        segment=segment,
    )


def iter_decode_frames(
    path: str | Path,
    errors: list[BadRegion] | None = None,
) -> Iterator[DecodedFrame]:
    """Yield decoded frames; one bad packet is recorded and skipped."""

    source = Path(path)
    if not source.is_file():
        raise InputError(f"input does not exist: {source}")
    regions = errors if errors is not None else []

    try:
        container = av.open(str(source))
    except Exception as exc:
        raise InputError(f"cannot open input: {source}: {exc}") from exc

    try:
        if not container.streams.video:
            raise InputError(f"input has no video stream: {source}")
        stream = container.streams.video[0]
        segment = 0
        decoded = 0
        packet_idx = 0

        try:
            for packet_idx, packet in enumerate(container.demux(stream), start=1):
                if packet.size == 0:
                    continue
                try:
                    frames = list(packet.decode())
                except Exception as exc:
                    regions.append(BadRegion(segment, packet_idx, "bad_packet", _clean_error(exc)))
                    segment += 1
                    continue

                for frame in frames:
                    try:
                        current = _decoded_record(frame, stream, decoded, segment)
                    except Exception as exc:
                        regions.append(
                            BadRegion(segment, packet_idx, "bad_frame", _clean_error(exc))
                        )
                        segment += 1
                        continue
                    if current.plane_hash is None:
                        regions.append(
                            BadRegion(segment, packet_idx, "unknown_pixel_format", "plane hash unavailable")
                        )
                    decoded += 1
                    yield current

            if decoded:
                try:
                    tail = stream.codec_context.decode(None)
                except Exception as exc:
                    regions.append(BadRegion(segment, None, "truncated", _clean_error(exc)))
                    tail = []
                for frame in tail:
                    current = _decoded_record(frame, stream, decoded, segment)
                    if current.plane_hash is None:
                        regions.append(
                            BadRegion(segment, None, "unknown_pixel_format", "plane hash unavailable")
                        )
                    decoded += 1
                    yield current
        except Exception as exc:
            if not isinstance(exc, (InputError, ProcessingError)):
                regions.append(BadRegion(segment, packet_idx or None, "truncated", _clean_error(exc)))
                segment += 1

        if decoded == 0:
            detail = regions[-1].message if regions else "no decodable video frames"
            raise ProcessingError(f"decode produced no frames: {source}: {detail}")
    finally:
        container.close()

