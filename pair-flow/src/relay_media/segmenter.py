"""按关键帧、目标时长与最大字节数对裸码流切片。

边界优先级：关键帧对齐 > 目标时长窗口 > 最大字节数硬约束。
关键帧缺失（窗口内无关键帧可切）时在最近的单元边界降级切开并标记；
每个片段保留源字节偏移范围，截断单元不参与正常切片，单独隔离。
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .bitstream import CodecUnit, ScanResult
from .timeline import ErrorKind, MediaError, Stage


@dataclass(slots=True)
class Segment:
    index: int
    start_offset: int
    end_offset: int
    pts_start_us: int | None
    pts_end_us: int | None
    starts_on_keyframe: bool
    degraded: bool = False
    unit_count: int = 0
    truncated: bool = False
    notes: list[str] = field(default_factory=list)

    @property
    def size_bytes(self) -> int:
        return self.end_offset - self.start_offset

    def to_dict(self) -> dict:
        return {
            "index": self.index,
            "start_offset": self.start_offset,
            "end_offset": self.end_offset,
            "size_bytes": self.size_bytes,
            "pts_start_us": self.pts_start_us,
            "pts_end_us": self.pts_end_us,
            "starts_on_keyframe": self.starts_on_keyframe,
            "degraded": self.degraded,
            "unit_count": self.unit_count,
            "truncated": self.truncated,
            "notes": self.notes,
        }


@dataclass(slots=True)
class SegmentPlan:
    target_duration_us: int
    max_bytes: int
    segments: list[Segment]
    errors: list[MediaError]

    def to_dict(self) -> dict:
        return {
            "target_duration_us": self.target_duration_us,
            "max_bytes": self.max_bytes,
            "segments": [s.to_dict() for s in self.segments],
            "errors": [e.to_dict() for e in self.errors],
        }


def segment_stream(
    scan: ScanResult,
    keyframe_offsets: list[int],
    *,
    target_duration_us: int,
    max_bytes: int,
    pts_by_offset: dict[int, int] | None = None,
) -> SegmentPlan:
    """基于扫描结果与关键帧索引切片。

    pts_by_offset: 单元起始偏移 -> 整数微秒 PTS；缺省则时间字段为 None。
    截断单元隔离不切片；关键帧缺失时降级到单元边界并登记错误。
    """

    pts_map = pts_by_offset or {}
    errors: list[MediaError] = []
    kf_set = set(keyframe_offsets)
    usable = [u for u in scan.units if not u.truncated]
    for unit in scan.units:
        if unit.truncated:
            errors.append(MediaError(
                Stage.SEGMENT, ErrorKind.TRUNCATED_UNIT, unit.start_offset,
                "截断单元隔离，不参与切片", recovered=False,
                context={"end_offset": unit.end_offset}))
    plan = SegmentPlan(target_duration_us, max_bytes, [], errors)
    if not usable:
        return plan

    starts = _boundaries(usable, kf_set, target_duration_us, max_bytes,
                         pts_map, errors)
    plan.segments = _build(usable, starts, pts_map, errors)
    return plan


def _boundaries(units, kf_set, target_us, max_bytes, pts_map, errors):
    """单趟收集片段起始下标，首个恒为 0。"""

    starts = [0]
    anchor = 0  # 当前片段首单元下标
    for i in range(1, len(units)):
        offset = units[i].start_offset
        byte_size = offset - units[anchor].start_offset
        start_pts = pts_map.get(units[anchor].start_offset)
        pts = pts_map.get(offset)
        duration_ok = (start_pts is not None and pts is not None
                       and pts - start_pts >= target_us)
        byte_full = byte_size >= max_bytes
        if not (duration_ok or byte_full):
            continue
        if offset in kf_set:
            starts.append(i)
            anchor = i
            continue
        # 窗口到期但非关键帧：仅在字节硬约束下立即降级切开，
        # 否则顺延等待后续关键帧。
        if byte_full:
            starts.append(i)
            anchor = i
            errors.append(MediaError(
                Stage.SEGMENT, ErrorKind.DEGRADED_BOUNDARY, offset,
                "达到最大字节约束但缺少关键帧，降级为单元边界切片",
                recovered=True, context={"size_bytes": byte_size}))
    # 尾段超时长却始终等不到关键帧收口：整体标记降级。
    tail_pts = pts_map.get(units[anchor].start_offset)
    end_pts = pts_map.get(units[-1].start_offset)
    if (tail_pts is not None and end_pts is not None
            and end_pts - tail_pts > target_us
            and units[-1].start_offset not in kf_set):
        errors.append(MediaError(
            Stage.SEGMENT, ErrorKind.DEGRADED_BOUNDARY,
            units[-1].start_offset,
            "尾段超出目标时长且无关键帧收口，标记降级边界",
            recovered=True))
    return starts


def _build(units, starts, pts_map, errors) -> list[Segment]:
    degraded_offsets = {e.offset for e in errors
                        if e.kind == ErrorKind.DEGRADED_BOUNDARY}
    bounds = starts + [len(units)]
    segments: list[Segment] = []
    for seg_idx, lo in enumerate(starts):
        hi = bounds[seg_idx + 1]
        first, last = units[lo], units[hi - 1]
        # 段首命中=字节硬切降级；段尾命中=超时无关键帧收口降级。
        degraded = (first.start_offset in degraded_offsets
                    or last.start_offset in degraded_offsets)
        segments.append(Segment(
            index=seg_idx,
            start_offset=first.start_offset,
            end_offset=last.end_offset,
            pts_start_us=pts_map.get(first.start_offset),
            pts_end_us=pts_map.get(last.start_offset),
            starts_on_keyframe=first.keyframe,
            degraded=degraded,
            unit_count=hi - lo,
            truncated=any(u.truncated for u in units[lo:hi]),
        ))
    return segments
