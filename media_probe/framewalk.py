"""Decode every video frame and pair it with its container packet record.

Packet timestamps (pts/dts/duration/size) are the container truth - this is
where timestamp defects live. Decoded frames provide pict_type/keyframe.
The two sequences are joined in decode order via a pts multiset.
"""

from __future__ import annotations

from collections import defaultdict, deque
from dataclasses import dataclass, field

import av


@dataclass
class FrameRecord:
    """One decoded frame, in decode (packet) order."""

    packet_index: int
    pts: int | None
    dts: int | None
    duration: int | None
    size: int
    keyframe: bool
    pict_type: str | None  # 'I' | 'P' | 'B' | None when decoder does not say


@dataclass
class WalkResult:
    frames: list[FrameRecord] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)
    packets_seen: int = 0
    frames_decoded: int = 0
    missing_intervals: list[tuple[float, float]] = field(default_factory=list)


def _is_flush_packet(packet) -> bool:
    return packet.pts is None and packet.size == 0


def walk(path: str, stream_index: int, duration_sec: float | None = None,
         time_base=None) -> WalkResult:
    """Decode all frames of the given video stream, tolerating bad packets.

    Decode/demux errors are recorded as warnings and walking continues (or
    stops cleanly when the demuxer itself fails, e.g. truncation).
    """
    result = WalkResult()
    container = av.open(path)
    try:
        stream = container.streams[stream_index]
        tb = time_base or stream.time_base
        packets = []  # (pts, dts, duration, size, keyframe)
        decoded = []  # (pts, pict_type, keyframe)
        try:
            for packet in container.demux(stream):
                flush = _is_flush_packet(packet)
                if not flush:
                    result.packets_seen += 1
                    packets.append(
                        (
                            packet.pts,
                            packet.dts,
                            packet.duration,
                            packet.size,
                            bool(packet.is_keyframe),
                        )
                    )
                try:
                    # flush packets carry no data but drain the decoder
                    for frame in packet.decode():
                        result.frames_decoded += 1
                        pict = frame.pict_type
                        decoded.append(
                            (
                                frame.pts,
                                str(pict) if pict is not None else None,
                                bool(frame.key_frame),
                            )
                        )
                except av.error.FFmpegError as exc:
                    result.warnings.append(
                        f"decode error at packet {result.packets_seen - 1}: {exc}"
                    )
        except av.error.FFmpegError as exc:
            result.warnings.append(f"demux stopped early: {exc}")

        # Join packets with decoded frames via pts multiset, in decode order.
        by_pts: dict[int, deque] = defaultdict(deque)
        no_pts_frames = 0
        for pts, pict, key in decoded:
            if pts is None:
                no_pts_frames += 1
            else:
                by_pts[pts].append((pict, key))
        if no_pts_frames:
            result.warnings.append(
                f"{no_pts_frames} decoded frames carried no pts; left unpaired"
            )
        unmatched_packets = 0
        for index, (pts, dts, duration, size, pkey) in enumerate(packets):
            pict = None
            fkey = False
            if pts is not None and by_pts.get(pts):
                pict, fkey = by_pts[pts].popleft()
            else:
                unmatched_packets += 1
            result.frames.append(
                FrameRecord(
                    packet_index=index,
                    pts=pts,
                    dts=dts,
                    duration=duration,
                    size=size,
                    keyframe=pkey or fkey,
                    pict_type=pict,
                )
            )
        leftover = sum(len(q) for q in by_pts.values())
        if unmatched_packets:
            result.warnings.append(
                f"{unmatched_packets} packets produced no decodable frame"
            )
        if leftover:
            result.warnings.append(
                f"{leftover} decoded frames could not be matched to packets"
            )

        # Missing tail detection (e.g. truncated file): compare the last
        # timestamp against the declared stream duration.
        if duration_sec:
            ends = [
                (r.pts + (r.duration or 0)) for r in result.frames if r.pts is not None
            ]
            if ends:
                tail_sec = max(ends) * float(tb)
                if duration_sec - tail_sec > 0.5:
                    result.missing_intervals.append((tail_sec, duration_sec))
                    result.warnings.append(
                        f"missing interval: stream ends at {tail_sec:.3f}s "
                        f"but duration metadata says {duration_sec:.3f}s"
                    )
    finally:
        container.close()
    return result
