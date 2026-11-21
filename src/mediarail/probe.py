"""Stream packets out of a container and produce timeline events.

The probe never decodes frames and never holds more than one packet in
memory; it yields one :class:`PacketEvent` per demuxed packet so downstream
stages can operate with bounded memory regardless of file duration.
"""

from __future__ import annotations

import enum
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Iterator

import av

from .errors import ErrorCode, MediaError, Severity


class TrackKind(enum.Enum):
    VIDEO = "video"
    AUDIO = "audio"
    OTHER = "other"


@dataclass
class TrackInfo:
    """Static description of one elementary stream."""

    index: int
    kind: TrackKind
    codec: str
    time_base: Fraction
    duration: int | None
    frames: int
    rate: Fraction | None  # average frame/sample rate when known


@dataclass
class PacketEvent:
    """One demuxed packet, annotated with its position in the input."""

    track: TrackInfo
    packet_index: int          # per-track packet ordinal
    global_index: int          # ordinal across the whole container
    pts: int | None
    dts: int | None
    duration: int
    byte_offset: int | None
    size: int
    is_keyframe: bool
    raw: object = None         # the underlying av.Packet, kept for remuxing


@dataclass
class ProbeResult:
    """Tracks discovered plus every anomaly observed while scanning."""

    path: Path
    tracks: list[TrackInfo] = field(default_factory=list)
    errors: list[MediaError] = field(default_factory=list)
    packets_scanned: int = 0
    truncated: bool = False


def _track_kind(stream: av.stream.Stream) -> TrackKind:
    if stream.type == "video":
        return TrackKind.VIDEO
    if stream.type == "audio":
        return TrackKind.AUDIO
    return TrackKind.OTHER


def open_tracks(path: Path, result: ProbeResult) -> av.container.InputContainer:
    """Open a container and record its track layout into ``result``."""
    container = av.open(str(path))
    for stream in container.streams:
        rate = None
        if stream.type == "video" and stream.average_rate:
            rate = Fraction(stream.average_rate)
        elif stream.type == "audio" and stream.sample_rate:
            rate = Fraction(stream.sample_rate, 1)
        result.tracks.append(
            TrackInfo(
                index=stream.index,
                kind=_track_kind(stream),
                codec=stream.codec_context.name or "unknown",
                time_base=Fraction(stream.time_base),
                duration=stream.duration,
                frames=stream.frames or 0,
                rate=rate,
            )
        )
    if not any(t.kind in (TrackKind.VIDEO, TrackKind.AUDIO) for t in result.tracks):
        result.errors.append(
            MediaError(
                code=ErrorCode.MISSING_TRACK,
                message="container has no audio or video track",
                severity=Severity.UNREPAIRABLE,
            )
        )
    return container


def scan(path: str | Path, result: ProbeResult | None = None) -> Iterator[PacketEvent]:
    """Yield packet events; demux failures are recorded, not raised.

    On a demux failure the event stream ends and ``result.errors`` holds the
    first reproducible anomaly with its packet index and byte offset.
    """
    path = Path(path)
    if result is None:
        result = ProbeResult(path)
    container = open_tracks(path, result)
    per_track: dict[int, int] = {}
    global_index = 0
    last_offset = 0
    try:
        for packet in container.demux():
            if packet.stream_index is None:
                continue
            track = next(
                (t for t in result.tracks if t.index == packet.stream_index), None
            )
            if track is None or track.kind is TrackKind.OTHER:
                continue
            if (packet.size or 0) == 0 and packet.pts is None and packet.dts is None:
                # Empty flush trailer packet: no payload, no timestamps.
                result.errors.append(
                    MediaError(
                        code=ErrorCode.CORRUPT_PACKET,
                        message="empty packet with no timestamps dropped",
                        severity=Severity.REPAIRED,
                        track=track.kind.value,
                        packet_index=per_track.get(track.index, 0),
                        byte_offset=packet.pos,
                    )
                )
                continue
            idx = per_track.get(track.index, 0)
            per_track[track.index] = idx + 1
            offset = packet.pos if packet.pos is not None else last_offset
            last_offset = offset
            result.packets_scanned += 1
            yield PacketEvent(
                track=track,
                packet_index=idx,
                global_index=global_index,
                pts=packet.pts,
                dts=packet.dts,
                duration=packet.duration or 0,
                byte_offset=offset,
                size=packet.size or 0,
                is_keyframe=packet.is_keyframe,
                raw=packet,
            )
            global_index += 1
    except av.error.FFmpegError as exc:
        result.errors.append(
            MediaError(
                code=ErrorCode.DEMUX_FAILURE,
                message=f"demux failed: {exc}",
                severity=Severity.UNREPAIRABLE,
                packet_index=global_index,
                byte_offset=last_offset,
            )
        )
    finally:
        container.close()
    return


def check_truncation(
    result: ProbeResult,
    last_end_seconds: Fraction | None,
    last_byte_offset: int | None = None,
) -> None:
    """Flag inputs whose streams end far before the declared duration.

    ``last_end_seconds`` is the greatest (pts + duration) seen, in seconds.
    A truncated file stops demuxing cleanly at EOF, so the gap between the
    declared duration and the last observed packet is the only signal.
    """
    if last_end_seconds is None:
        return
    for track in result.tracks:
        if track.duration is None:
            continue
        declared = Fraction(track.duration) * track.time_base
        if declared - last_end_seconds > Fraction(2):
            result.truncated = True
            result.errors.append(
                MediaError(
                    code=ErrorCode.TRUNCATED_INPUT,
                    message=(
                        f"stream ends at {float(last_end_seconds):.3f}s but "
                        f"container declares {float(declared):.3f}s"
                    ),
                    severity=Severity.UNREPAIRABLE,
                    track=track.kind.value,
                    packet_index=result.packets_scanned,
                    byte_offset=last_byte_offset,
                )
            )
            return
