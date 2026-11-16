"""Single source of truth for display frame indexes and repaired timestamps."""

from __future__ import annotations

from dataclasses import dataclass, field, replace
from fractions import Fraction
from pathlib import Path
from typing import Iterator

import av

from .container import ContainerInfo, inspect_container, open_video
from .packets import PacketRecord, extract_packets
from .reorder import reorder_packets
from .timestamps import RepairAction, TimestampRepairResult, repair_timestamps


@dataclass(frozen=True)
class FrameRecord:
    frame_index: int
    pts: int | None
    pts_ms: float
    dts: int | None
    decode_index: int
    keyframe: bool
    pict_type: str | None
    original_pts: int | None
    repairs: tuple[RepairAction, ...] = ()

    @property
    def repaired(self) -> bool:
        return bool(self.repairs)

    def timeline_dict(self) -> dict[str, object]:
        return {
            "frame_index": self.frame_index,
            "pts_ms": self.pts_ms,
            "keyframe": self.keyframe,
            "pict_type": self.pict_type,
        }


@dataclass
class FrameStream:
    info: ContainerInfo
    frames: list[FrameRecord]
    repair: TimestampRepairResult
    truncated: bool = False
    error: str | None = None
    warnings: list[str] = field(default_factory=list)


@dataclass(frozen=True)
class DecodedFrame:
    index: int
    frame: av.VideoFrame
    pts: int | None
    dts: int | None
    keyframe: bool
    pict_type: str | None


class DecodedFrameStream:
    """A one-pass decoded frame iterator that records tail truncation."""

    def __init__(self, path: str | Path, stream_index: int | None = None) -> None:
        self.path = path
        self.stream_index = stream_index
        self.truncated = False
        self.error: str | None = None
        self.count = 0

    def __iter__(self) -> Iterator[DecodedFrame]:
        with open_video(self.path, self.stream_index) as (container, stream, _):
            try:
                for index, frame in enumerate(container.decode(stream)):
                    self.count += 1
                    yield DecodedFrame(
                        index=index,
                        frame=frame,
                        pts=frame.pts,
                        dts=frame.dts,
                        keyframe=bool(frame.key_frame),
                        pict_type=frame.pict_type.name if frame.pict_type else None,
                    )
            except av.error.FFmpegError as exc:
                self.truncated = self.count > 0
                self.error = str(exc)


def iter_decoded_frames(
    path: str | Path, stream_index: int | None = None
) -> DecodedFrameStream:
    return DecodedFrameStream(path, stream_index)


def _default_interval(info: ContainerInfo) -> int | None:
    if not info.average_rate:
        return None
    interval = Fraction(1, 1) / info.average_rate / info.time_base
    return max(1, int(round(interval)))


def _empty_repair() -> TimestampRepairResult:
    return TimestampRepairResult(
        packets=[],
        actions=[],
        offset=0,
        repaired_timestamps=0,
        max_jump_ms_before=0.0,
        max_jump_ms_after=0.0,
        has_pts=False,
        has_dts=False,
        empty_stream=True,
        single_frame=False,
        has_b_frames=False,
    )


def build_frame_stream(path: str | Path, stream_index: int | None = None) -> FrameStream:
    """Demux, reorder and repair once for both timeline and stats consumers."""

    info = inspect_container(path, stream_index)
    packets = extract_packets(path, stream_index)
    truncated = info.probe_truncated or packets.truncated
    warnings: list[str] = []
    if not packets:
        warnings.append("empty stream: no video packets were found")
        return FrameStream(info, [], _empty_repair(), truncated, packets.error, warnings)

    usable = [packet for packet in packets if not packet.discard]
    if not usable:
        warnings.append("all packets are marked discard")
        return FrameStream(info, [], _empty_repair(), truncated, packets.error, warnings)
    ordered = reorder_packets(usable)
    repaired = repair_timestamps(
        ordered,
        info.time_base,
        default_interval=_default_interval(info),
    )
    actions_by_frame: dict[int, list[RepairAction]] = {}
    for action in repaired.actions:
        actions_by_frame.setdefault(action.frame_index, []).append(action)

    frames: list[FrameRecord] = []
    for index, (source, packet) in enumerate(zip(ordered, repaired.packets)):
        pts = packet.pts
        frames.append(
            FrameRecord(
                frame_index=index,
                pts=pts,
                pts_ms=round(float(Fraction(pts) * info.time_base * 1000), 6)
                if pts is not None
                else 0.0,
                dts=packet.dts,
                decode_index=source.index,
                keyframe=packet.keyframe,
                pict_type=packet.pict_type,
                original_pts=source.pts,
                repairs=tuple(actions_by_frame.get(index, ())),
            )
        )
    stream = FrameStream(info, frames, repaired, truncated, packets.error, warnings)
    _validate_decoded_timeline(stream, path, stream_index)
    if repaired.single_frame:
        stream.warnings.append("single frame: no frame interval is available")
    if not repaired.has_b_frames:
        stream.warnings.append("no B frames detected")
    return stream


def _validate_decoded_timeline(stream: FrameStream, path, stream_index) -> None:
    """Read frame metadata only; do not compute or retain pixel statistics."""

    decoded = iter_decoded_frames(path, stream_index)
    metadata: dict[int, tuple[str | None, bool]] = {}
    for item in decoded:
        metadata[item.index] = (item.pict_type, item.keyframe)
    for index, frame in enumerate(stream.frames):
        if index in metadata:
            pict_type, keyframe = metadata[index]
            stream.frames[index] = replace(
                frame,
                pict_type=pict_type or frame.pict_type,
                keyframe=keyframe,
            )
    if decoded.count < len(stream.frames):
        stream.frames = stream.frames[: decoded.count]
        stream.repair.packets = stream.repair.packets[: decoded.count]
        stream.repair.actions = [
            action for action in stream.repair.actions if action.frame_index < decoded.count
        ]
        stream.repair.repaired_timestamps = sum(
            action.action != "zero_origin" for action in stream.repair.actions
        ) + bool(stream.repair.offset)
    stream.repair.has_b_frames = any(frame.pict_type == "B" for frame in stream.frames)
    stream.repair.single_frame = len(stream.frames) == 1
    stream.repair.empty_stream = not stream.frames
    if decoded.truncated:
        stream.truncated = True
        stream.warnings.append("decoder stopped at a truncated tail")


