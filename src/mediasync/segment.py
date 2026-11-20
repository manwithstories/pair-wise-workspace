"""多表示层共同切点计算与分片清单生成。

输入每个表示层的关键帧毫秒时间（有理数），在固定目标分片时长网格上
寻找各表示层都存在可对齐关键帧的共同切点；任一表示层在容差窗口内
没有关键帧时拒绝该切点并输出未对齐原因。
"""

from __future__ import annotations

import dataclasses
from fractions import Fraction

from .errors import ErrorBook, ErrorCode


@dataclasses.dataclass(frozen=True)
class Representation:
    """一个码率表示层的关键帧时间线。"""

    name: str
    keyframe_ms: list[Fraction]
    timescale: int = 1000

    def nearest(self, target: Fraction) -> tuple[Fraction, Fraction] | None:
        """返回距 target 最近的关键帧 (时间, 偏差绝对值)；无帧返回 None。"""
        if not self.keyframe_ms:
            return None
        best = min(self.keyframe_ms, key=lambda t: abs(t - target))
        return best, abs(best - target)


@dataclasses.dataclass(frozen=True)
class CutPoint:
    index: int
    planned_ms: Fraction
    """网格规划时间。"""
    boundary_ms: Fraction
    """实际采用的共同边界（锚点表示层关键帧时间）。"""
    per_rep_ms: dict[str, Fraction]
    max_deviation_ms: Fraction


@dataclasses.dataclass(frozen=True)
class RejectedCut:
    planned_ms: Fraction
    reason: str
    per_rep_deviation: dict[str, Fraction | None]


@dataclasses.dataclass(frozen=True)
class Segment:
    sequence: int
    start_ms: Fraction
    end_ms: Fraction
    duration_ms: Fraction
    cuts: dict[str, Fraction]
    """每个表示层在该分片边界的关键帧时间。"""


@dataclasses.dataclass(frozen=True)
class SegmentPlan:
    segments: list[Segment]
    cuts: list[CutPoint]
    rejected: list[RejectedCut]
    aligned_representations: tuple[str, ...]
    total_representations: int

    @property
    def fully_aligned(self) -> bool:
        return (
            len(self.aligned_representations) == self.total_representations
            and bool(self.segments)
        )


def plan_cuts(
    representations: list[Representation],
    segment_ms: int,
    max_deviation_ms: int,
    *,
    errors: ErrorBook,
    source: str = "ladder",
) -> SegmentPlan:
    """求共同切点。

    流程：以时长网格 0, segment_ms, 2*... 为规划点；每个规划点在锚点
    （第一个表示层）上吸附到最近关键帧，再要求其余表示层在
    ``max_deviation_ms`` 窗口内也有关键帧。
    """
    if segment_ms <= 0 or max_deviation_ms < 0:
        errors.add(
            ErrorCode.CONFIG_ERROR,
            "segment_ms 必须为正、max_deviation_ms 不可为负",
            source=source,
            fatal=True,
        )
        return SegmentPlan([], [], [], tuple(), len(representations))
    if not representations:
        errors.add(
            ErrorCode.ALIGNMENT_IMPOSSIBLE,
            "没有任何表示层可供对齐",
            source=source,
            fatal=True,
        )
        return SegmentPlan([], [], [], tuple(), 0)

    anchor = representations[0]
    # 切点必须严格落在素材内部：取最短表示层的末帧作为上界。
    min_end = min(
        (rep.keyframe_ms[-1] for rep in representations if rep.keyframe_ms),
        default=Fraction(0),
    )
    max_end = max(
        (rep.keyframe_ms[-1] for rep in representations if rep.keyframe_ms),
        default=Fraction(0),
    )
    grid = [
        Fraction(i * segment_ms)
        for i in range(1, int(min_end // segment_ms) + 1)
        if Fraction(i * segment_ms) < min_end
    ]
    tolerance = Fraction(max_deviation_ms)

    cuts: list[CutPoint] = []
    rejected: list[RejectedCut] = []
    for planned in grid:
        anchor_hit = anchor.nearest(planned)
        if anchor_hit is None or anchor_hit[1] > tolerance:
            rejected.append(
                RejectedCut(
                    planned_ms=planned,
                    reason=f"锚点表示层 {anchor.name} 在容差窗口内无关键帧",
                    per_rep_deviation={anchor.name: anchor_hit[1] if anchor_hit else None},
                )
            )
            continue
        boundary = anchor_hit[0]
        per_rep: dict[str, Fraction] = {anchor.name: boundary}
        deviations: dict[str, Fraction | None] = {anchor.name: anchor_hit[1]}
        max_dev = anchor_hit[1]
        ok = True
        for rep in representations[1:]:
            hit = rep.nearest(boundary)
            if hit is None:
                ok = False
                deviations[rep.name] = None
                continue
            deviations[rep.name] = hit[1]
            if hit[1] > tolerance:
                ok = False
                continue
            per_rep[rep.name] = hit[0]
            max_dev = max(max_dev, hit[1])
        if not ok:
            missing = [name for name, dev in deviations.items() if dev is None or dev > tolerance]
            rejected.append(
                RejectedCut(
                    planned_ms=planned,
                    reason=f"表示层 {','.join(missing)} 超出最大偏差 {max_deviation_ms}ms",
                    per_rep_deviation=deviations,
                )
            )
            continue
        cuts.append(
            CutPoint(
                index=len(cuts),
                planned_ms=planned,
                boundary_ms=boundary,
                per_rep_ms=per_rep,
                max_deviation_ms=max_dev,
            )
        )

    if not cuts:
        errors.add(
            ErrorCode.ALIGNMENT_IMPOSSIBLE,
            f"{source}: 全部 {len(grid)} 个规划切点均无法对齐，"
            f"未对齐原因见 rejected（容差 {max_deviation_ms}ms）",
            source=source,
            fatal=True,
            detail={"rejected": [dataclasses.asdict(r) for r in rejected]},
        )

    segments = _build_segments(cuts, representations, max_end)
    aligned = tuple(
        rep.name
        for rep in representations
        if all(rep.name in cut.per_rep_ms for cut in cuts)
    )
    return SegmentPlan(
        segments=segments,
        cuts=cuts,
        rejected=rejected,
        aligned_representations=aligned,
        total_representations=len(representations),
    )


def _build_segments(
    cuts: list[CutPoint],
    representations: list[Representation],
    tail_end: Fraction,
) -> list[Segment]:
    """切点 + 各表示层末尾关键帧 -> 分片区间。"""
    if not cuts:
        return []
    names = [rep.name for rep in representations]
    boundaries = [Fraction(0)] + [cut.boundary_ms for cut in cuts]
    segments: list[Segment] = []
    for seq, start in enumerate(boundaries):
        if seq < len(cuts):
            end = cuts[seq].boundary_ms
        else:
            end = tail_end
        per_cut = {
            name: (cuts[seq - 1].per_rep_ms[name] if seq > 0 else Fraction(0))
            for name in names
        }
        segments.append(
            Segment(
                sequence=seq,
                start_ms=start,
                end_ms=end,
                duration_ms=end - start,
                cuts=per_cut,
            )
        )
    return segments


def plan_to_dict(plan: SegmentPlan) -> dict:
    """清单可序列化表示，Fraction 统一为分子/分母，避免浮点造成摘要漂移。"""
    def q(value: Fraction) -> dict:
        return {"num": value.numerator, "den": value.denominator}

    return {
        "fully_aligned": plan.fully_aligned,
        "aligned_representations": list(plan.aligned_representations),
        "total_representations": plan.total_representations,
        "segments": [
            {
                "sequence": seg.sequence,
                "start_ms": q(seg.start_ms),
                "end_ms": q(seg.end_ms),
                "duration_ms": q(seg.duration_ms),
                "cuts": {name: q(t) for name, t in sorted(seg.cuts.items())},
            }
            for seg in plan.segments
        ],
        "rejected": [
            {
                "planned_ms": q(r.planned_ms),
                "reason": r.reason,
                "per_rep_deviation": {
                    name: (None if dev is None else q(dev))
                    for name, dev in sorted(r.per_rep_deviation.items())
                },
            }
            for r in plan.rejected
        ],
    }
