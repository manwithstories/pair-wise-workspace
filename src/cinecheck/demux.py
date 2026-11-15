"""Demux layer: read stream time base and packet-order pts/dts.

Only the first video stream is handled; audio streams are ignored.
The module never decodes frames and keeps no per-film growing state.
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Iterator

import av


class NoVideoStreamError(RuntimeError):
    """Raised when the container carries no decodable video stream."""


@dataclass(frozen=True)
class PacketRecord:
    """One packet as seen on the wire (decode order)."""

    index: int
    dts: int | None
    pts: int | None
    is_keyframe: bool
    size: int


@dataclass
class StreamInfo:
    time_base: Fraction
    nominal_rate: Fraction | None
    average_rate: Fraction | None
    codec: str
    width: int
    height: int
    frames_estimated: int


def _fraction(value) -> Fraction | None:
    if value is None:
        return None
    return Fraction(value.numerator, value.denominator)


_STANDARD_RATES = (
    Fraction(24, 1), Fraction(25, 1), Fraction(30, 1), Fraction(48, 1),
    Fraction(50, 1), Fraction(60, 1), Fraction(120, 1),
    Fraction(30000, 1001), Fraction(24000, 1001),
)


def nominal_rate(signalled: Fraction | None) -> Fraction | None:
    """Snap a slightly perturbed signalled rate to the nearest standard rate.

    A handful of injected timestamp anomalies shift the container's
    average rate (e.g. 24 -> 23.867). The declared/nominal rate used to
    rebuild the equal-interval axis must stay the true production rate, so
    we snap to the closest standard rate within 2%; otherwise the raw
    signalled value is returned unchanged.
    """

    if signalled is None:
        return None
    if signalled in _STANDARD_RATES:
        return signalled
    # Fractional NTSC rates are matched first with a very tight window so
    # a genuine 23.976 stream is never collapsed onto the integer 24 rate.
    ntsc = min(
        (rate for rate in _STANDARD_RATES if rate.denominator != 1),
        key=lambda rate: abs(rate - signalled),
    )
    if abs(ntsc - signalled) <= signalled * Fraction(2, 1000):
        return ntsc
    integer_rates = [rate for rate in _STANDARD_RATES if rate.denominator == 1]
    best = min(integer_rates, key=lambda rate: abs(rate - signalled))
    # A 0.6% window absorbs small injected perturbations: 24 fps signalled
    # as ~23.867 still snaps back to the true production rate of 24.
    integer_window = signalled * Fraction(6, 1000)
    if abs(best - signalled) <= integer_window:
        return best
    return signalled


def open_video(path: str | Path) -> tuple[av.container.InputContainer, StreamInfo]:
    """Open *path* and return the container plus video stream metadata."""

    container = av.open(str(path))
    if not container.streams.video:
        container.close()
        raise NoVideoStreamError(f"no video stream in {path}")
    stream = container.streams.video[0]
    info = StreamInfo(
        time_base=Fraction(stream.time_base),
        nominal_rate=nominal_rate(_fraction(stream.average_rate)),
        average_rate=_fraction(stream.average_rate),
        codec=getattr(stream.codec_context, "name", "unknown") or "unknown",
        width=int(stream.codec_context.width or 0),
        height=int(stream.codec_context.height or 0),
        frames_estimated=int(getattr(stream, "frames", 0) or 0),
    )
    return container, info


def iter_packets(
    container: av.container.InputContainer,
    stream: av.video.stream.VideoStream,
) -> Iterator[PacketRecord]:
    """Yield packet records in decode order.

    A missing dts is preserved as ``None`` so downstream layers can infer
    it instead of silently fabricating a value at the demux boundary.
    """

    for index, packet in enumerate(container.demux(stream)):
        yield PacketRecord(
            index=index,
            dts=packet.dts,
            pts=packet.pts,
            is_keyframe=bool(packet.is_keyframe),
            size=int(packet.size or 0),
        )
