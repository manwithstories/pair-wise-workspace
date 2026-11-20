"""Streaming PyAV demux with bounded, locatable packet recovery.

Each manifest segment is opened independently and consumed packet-by-packet so
memory stays flat regardless of programme length.  Recovery rules:

* a missing reference is reported as :data:`Code.REFERENCE_MISSING`,
* an unopenable or exhausted-without-frames container is
  :data:`Code.SEGMENT_TRUNCATED`,
* individual packet/frame decode failures count as
  :data:`Code.PACKET_CORRUPT` / :data:`Code.DECODE_FAILURE`; a packet is
  retried at most ``max_decode_retries`` times (PyAV flushes decoder state
  once), never indefinitely,
* every recovered frame keeps its source PTS converted to seconds on the
  stream time base, together with the ``keyframe`` flag for the index module.

The caller (pipeline) decides whether damage means skip, silence-gap or stop;
this module only produces diagnostics plus whatever audio was recoverable.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av
import numpy as np

from .errors import Code, Diagnostic
from .manifest_core import SegmentRef
from .timeline import rational, to_float_seconds

MAX_DECODE_RETRIES = 2


@dataclass
class DecodedChunk:
    """One decoded frame worth of audio plus its source timing."""

    samples: np.ndarray  # (n, channels) float32
    rate: int
    layout: str
    pts_seconds: Fraction | None
    keyframe: bool
    source_samples: int


@dataclass
class SegmentDecodeResult:
    segment: SegmentRef
    chunks: list[DecodedChunk] = field(default_factory=list)
    diagnostics: list[Diagnostic] = field(default_factory=list)
    packets_total: int = 0
    packets_failed: int = 0
    frames_total: int = 0
    keyframe_pts: list[Fraction] = field(default_factory=list)
    opened: bool = False
    truncated: bool = False

    @property
    def damaged(self) -> bool:
        return bool(self.diagnostics) or self.truncated

    @property
    def total_samples(self) -> int:
        return sum(int(c.samples.shape[0]) for c in self.chunks)

    def locator(self) -> "object":
        from .errors import Locator

        return Locator(
            manifest_path=str(self.segment.resolved_path),
            representation=self.segment.representation,
            sequence=self.segment.sequence,
            time_range=(
                to_float_seconds(self.segment.start),
                to_float_seconds(self.segment.end),
            ),
            segment_ref=self.segment.raw_ref,
        )


def _frame_to_planar(frame: av.AudioFrame) -> np.ndarray:
    """Copy a decoded frame to a contiguous ``(samples, channels)`` array."""
    channels = len(frame.layout.channels)
    n = int(frame.samples)
    if frame.format.is_planar:
        out = np.zeros((n, channels), dtype=np.float32)
        step = frame.format.bytes
        for c in range(channels):
            raw = bytes(frame.planes[c])
            usable = min(n, len(raw) // step)
            if usable:
                out[:usable, c] = np.frombuffer(raw, dtype="<f4", count=usable)
        return out
    width = channels * frame.format.bytes
    raw = bytes(frame.planes[0])
    usable = min(n, len(raw) // width)
    out = np.zeros((n, channels), dtype=np.float32)
    if usable:
        out[:usable] = np.frombuffer(raw, dtype="<f4", count=usable * channels).reshape(
            usable, channels
        )
    return out


def _diag(code: Code, message: str, result: SegmentDecodeResult, **detail: object) -> Diagnostic:
    from .errors import Locator

    locator = Locator(
        manifest_path=str(result.segment.resolved_path),
        representation=result.segment.representation,
        sequence=result.segment.sequence,
        time_range=(
            to_float_seconds(result.segment.start),
            to_float_seconds(result.segment.end),
        ),
        segment_ref=result.segment.raw_ref,
    )
    return Diagnostic(code, message, locator, dict(detail))


def _decode_packet(
    container: av.container.InputContainer,
    stream: av.audio.stream.AudioStream,
    packet: av.Packet,
) -> tuple[list[av.AudioFrame], Exception | None]:
    """Decode one packet with a bounded number of retries."""
    attempts = 0
    while True:
        try:
            return list(packet.decode()), None
        except av.error.FFmpegError as exc:
            attempts += 1
            if attempts > MAX_DECODE_RETRIES:
                return [], exc
            # A flush gives the decoder a clean state before the final attempt.
            try:
                list(container.decode(stream))
            except av.error.FFmpegError:
                pass


def demux_segment(segment: SegmentRef) -> SegmentDecodeResult:
    """Demux and decode a single segment, never raising for media damage."""
    result = SegmentDecodeResult(segment=segment)
    path: Path = segment.resolved_path
    if not path.exists():
        result.diagnostics.append(
            _diag(Code.REFERENCE_MISSING, "segment file does not exist", result,
                  path=str(path))
        )
        result.truncated = True
        return result
    try:
        container = av.open(str(path))
    except av.error.FFmpegError as exc:
        result.diagnostics.append(
            _diag(Code.SEGMENT_TRUNCATED, f"container cannot be opened: {exc}", result)
        )
        result.truncated = True
        return result
    result.opened = True
    try:
        stream = container.streams.audio[0]
        time_base = rational(stream.time_base) if stream.time_base else Fraction(1, 90000)
        rate = int(stream.rate)
        layout = stream.layout.name
        saw_packet = False
        for packet in container.demux(stream):
            saw_packet = True
            result.packets_total += 1
            frames, failure = _decode_packet(container, stream, packet)
            if failure is not None:
                result.packets_failed += 1
                code = (
                    Code.PACKET_CORRUPT
                    if packet.size and packet.size > 0
                    else Code.DECODE_FAILURE
                )
                result.diagnostics.append(
                    _diag(code, f"packet decode failed: {failure}", result,
                          packet_size=packet.size, attempts=MAX_DECODE_RETRIES + 1)
                )
                continue
            for frame in frames:
                samples = _frame_to_planar(frame)
                pts = (
                    rational(frame.pts) * time_base
                    if frame.pts is not None
                    else None
                )
                keyframe = bool(getattr(frame, "keyframe", False))
                if keyframe and pts is not None:
                    result.keyframe_pts.append(pts)
                result.chunks.append(
                    DecodedChunk(
                        samples=samples,
                        rate=int(frame.sample_rate or rate),
                        layout=layout,
                        pts_seconds=pts,
                        keyframe=keyframe,
                        source_samples=int(frame.samples),
                    )
                )
                result.frames_total += 1
        if not saw_packet or not result.chunks:
            result.truncated = True
            result.diagnostics.append(
                _diag(
                    Code.SEGMENT_TRUNCATED,
                    "segment yielded no decodable audio frames",
                    result,
                    saw_packet=saw_packet,
                )
            )
    except (av.error.FFmpegError, IndexError, ValueError) as exc:
        result.truncated = True
        result.diagnostics.append(
            _diag(Code.SEGMENT_TRUNCATED, f"demux aborted: {exc}", result)
        )
    finally:
        container.close()
    return result


def concatenate_chunks(result: SegmentDecodeResult) -> tuple[np.ndarray, int, str]:
    """Glue a segment's decoded frames into one array (bounded by segment size)."""
    if not result.chunks:
        return np.zeros((0, 1), dtype=np.float32), 0, "mono"
    rate = result.chunks[0].rate
    layout = result.chunks[0].layout
    samples = np.concatenate([c.samples for c in result.chunks], axis=0)
    return samples.astype(np.float32, copy=False), rate, layout


__all__ = [
    "MAX_DECODE_RETRIES",
    "DecodedChunk",
    "SegmentDecodeResult",
    "concatenate_chunks",
    "demux_segment",
]
