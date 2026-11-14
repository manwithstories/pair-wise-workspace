"""Packet-level video decoding with integer timestamp preservation."""

from __future__ import annotations

from collections import deque
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Optional

import av


AV_ERROR = getattr(av, "AVError", getattr(av, "FFmpegError", Exception))


@dataclass(slots=True)
class FrameInfo:
    """One decoded video frame, indexed in decoder output order."""

    decode_index: int
    packet_index: int
    pts: Optional[int]
    dts: Optional[int]
    pict_type: str
    keyframe: bool
    packet_size: int
    decode_error: Optional[str] = None
    synthetic: bool = False


@dataclass(slots=True)
class DecodeResult:
    """All metadata needed by reordering and repair."""

    path: Path
    frames: list[FrameInfo] = field(default_factory=list)
    packets: int = 0
    decode_errors: int = 0
    time_base: Fraction = Fraction(1, 90000)
    stream_fps: Optional[Fraction] = None
    source_is_raw: bool = False
    codec_name: str = ""
    width: int = 0
    height: int = 0

    @property
    def decoded_frames(self) -> int:
        return len(self.frames)

    @property
    def packet_frame_delta(self) -> int:
        return self.packets - self.decoded_frames


def _picture_type(frame: av.VideoFrame) -> str:
    value = frame.pict_type
    if isinstance(value, int):
        return {1: "I", 2: "P", 3: "B"}.get(value, "other")
    name = getattr(value, "name", str(value)).upper()
    return name if name in {"I", "P", "B"} else "other"


def _normal_exceptions() -> tuple[type[BaseException], ...]:
    return (AV_ERROR, ValueError, RuntimeError, OverflowError)


def _frame_timestamp(value: Optional[int]) -> Optional[int]:
    return int(value) if value is not None else None


def _raw_stream_hint(path: Path) -> bool:
    return path.suffix.lower() in {".h264", ".264", ".avc", ".hevc", ".h265"}


def _interval_ticks(time_base: Fraction, fps: Fraction) -> int:
    numerator = time_base.denominator * fps.denominator
    denominator = time_base.numerator * fps.numerator
    return max(1, (numerator + denominator // 2) // denominator)


def decode_file(path: str | Path, *, nominal_fps: Fraction = Fraction(25, 1)) -> DecodeResult:
    """Decode a container or raw elementary stream without loading frame planes.

    A malformed packet increments the error count and decoding continues with the
    next packet.  Missing PTS values are synthesized at ``nominal_fps`` only for
    elementary streams; the result is marked ``synthetic``.
    """

    source = Path(path)
    is_raw = _raw_stream_hint(source)
    result = DecodeResult(path=source, source_is_raw=is_raw)

    try:
        container = av.open(str(source))
    except (AV_ERROR, OSError, ValueError) as exc:
        raise ValueError(f"cannot open video input: {source}: {exc}") from exc

    try:
        streams = [stream for stream in container.streams if stream.type == "video"]
        if not streams:
            return result
        stream = streams[0]
        result.codec_name = getattr(stream.codec_context, "name", "") or ""
        result.width = int(stream.codec_context.width or 0)
        result.height = int(stream.codec_context.height or 0)
        rate = stream.average_rate or stream.guessed_rate
        if rate is not None and rate.denominator:
            result.stream_fps = Fraction(rate.numerator, rate.denominator)
        time_base = stream.time_base or stream.codec_context.time_base
        if time_base is not None and time_base.numerator > 0 and time_base.denominator > 0:
            result.time_base = Fraction(time_base.numerator, time_base.denominator)

        # PyAV yields decoded frames in presentation order, while packets arrive
        # in decode order.  Keep a small metadata-only association table so the
        # returned records preserve decode order and each packet's DTS/size.
        packet_meta: list[dict[str, object]] = []
        decoded_meta: list[tuple[Optional[int], str, bool]] = []
        seen_pts = False

        def consume(decoded: list[av.VideoFrame]) -> None:
            for frame in decoded:
                decoded_meta.append((
                    _frame_timestamp(frame.pts),
                    _picture_type(frame),
                    bool(frame.key_frame),
                ))

        iterator = container.demux(stream)
        while True:
            try:
                packet = next(iterator)
            except StopIteration:
                break
            except _normal_exceptions():
                result.decode_errors += 1
                break

            if packet.size == 0:
                try:
                    consume(packet.decode())
                except _normal_exceptions():
                    result.decode_errors += 1
                continue

            packet_index = len(packet_meta)
            packet_meta.append({
                "index": packet_index,
                "pts": _frame_timestamp(packet.pts),
                "dts": _frame_timestamp(packet.dts),
                "size": int(packet.size),
                "keyframe": bool(packet.is_keyframe),
            })
            result.packets += 1
            if packet.pts is not None:
                seen_pts = True
            try:
                consume(packet.decode())
            except _normal_exceptions():
                result.decode_errors += 1

        pts_to_packets: dict[int, deque[int]] = {}
        for meta in packet_meta:
            pts = meta["pts"]
            if pts is not None:
                pts_to_packets.setdefault(int(pts), deque()).append(int(meta["index"]))

        interval = _interval_ticks(result.time_base, nominal_fps)
        missing_synthetic = is_raw or not seen_pts
        buckets: dict[int, list[FrameInfo]] = {}
        used_meta: set[int] = set()
        next_meta_index = 0
        for frame_pts, pict_type, keyframe in decoded_meta:
            meta_index: Optional[int] = None
            if frame_pts is not None:
                match = pts_to_packets.get(frame_pts)
                if match:
                    meta_index = match.popleft()
            if meta_index is None and next_meta_index < len(packet_meta):
                meta_index = next_meta_index
                while meta_index < len(packet_meta) and meta_index in used_meta:
                    meta_index += 1
                if meta_index >= len(packet_meta):
                    meta_index = None
            next_meta_index = max(next_meta_index, (meta_index or 0) + 1)

            if meta_index is not None:
                used_meta.add(meta_index)
                meta = packet_meta[meta_index]
                pts = meta["pts"] if not missing_synthetic else None
                dts = meta["dts"] if not missing_synthetic else None
                packet_size = int(meta["size"])
                packet_keyframe = bool(meta["keyframe"])
                record_packet_index = int(meta["index"])
            else:
                pts = None
                dts = None
                packet_size = 0
                packet_keyframe = keyframe
                record_packet_index = max(0, len(packet_meta) - 1)

            if missing_synthetic or pts is None:
                synthetic_pts = 0
                synthetic = True
            else:
                synthetic_pts = int(pts)
                synthetic = is_raw or not seen_pts
            buckets.setdefault(record_packet_index, []).append(
                FrameInfo(
                    decode_index=0,
                    packet_index=record_packet_index,
                    pts=synthetic_pts,
                    dts=None if dts is None else int(dts),
                    pict_type=pict_type,
                    keyframe=packet_keyframe or keyframe,
                    packet_size=packet_size,
                    synthetic=synthetic,
                )
            )

        # Packet order is decode order.  Bucketing avoids an O(n log n) sort while
        # remaining stable when one packet emits multiple delayed frames.
        records: list[FrameInfo] = []
        for packet_index in range(len(packet_meta)):
            records.extend(buckets.get(packet_index, []))
        for packet_index in buckets:
            if packet_index >= len(packet_meta):
                records.extend(buckets[packet_index])
        for index, record in enumerate(records):
            record.decode_index = index
            if record.synthetic:
                record.pts = index * interval
        result.frames = records
        if not result.frames and result.packets:
            result.decode_errors += 1
        return result
    except (AV_ERROR, OSError, ValueError, RuntimeError, OverflowError) as exc:
        raise ValueError(f"cannot demux video input: {source}: {exc}") from exc
    finally:
        container.close()


def detect_nominal_fps(
    result: DecodeResult, override: Optional[Fraction] = None
) -> Fraction:
    """Choose an explicit rate, the stream rate, or a safe 25 fps fallback."""

    if override is not None and override > 0:
        return override
    if result.stream_fps is not None and result.stream_fps > 0:
        return result.stream_fps
    return Fraction(25, 1)
