"""时间线修复：跨日回绕识别、漂移修复与冲突标记。

所有修复都在统一有理数时间基准上进行，并受显式阈值约束：
- 回绕只在“足够大的向后跳变 + 单调性上下文”下确认，小回退视为歧义；
- 漂移按稳健线性拟合估计，逐点残差超阈值的记录不静默搬动，改为标记；
- 重叠 cue 永不静默移动，仅打 ``overlap-conflict`` 标记供下游隔离。
"""
from __future__ import annotations

import statistics
from dataclasses import dataclass
from fractions import Fraction

from .model import Cue, ErrorRecord, TimecodeRecord
from .timecode import frame_to_time

#: 向后跳变超过该比例（相对 24h 周期）才认定为跨日回绕。
WRAP_RATIO = Fraction(8, 10)
#: 默认漂移修复阈值（秒）与单条残差上限（秒）。
DEFAULT_DRIFT_LIMIT = Fraction(1, 4)


@dataclass
class RepairConfig:
    """修复参数；阈值为 0 时关闭对应修复。"""

    drift_limit: Fraction = DEFAULT_DRIFT_LIMIT
    enable_wrap: bool = True
    enable_drift: bool = True


def repair_timecodes(
    records: list[TimecodeRecord], config: RepairConfig | None = None
) -> tuple[list[TimecodeRecord], list[ErrorRecord]]:
    """把周期内帧序号还原为单调时间线，并修复可恢复漂移。

    返回 (修复后记录, 错误/恢复证据)；不可恢复记录保留原值并打标记。
    """

    cfg = config or RepairConfig()
    errors: list[ErrorRecord] = []
    if not records:
        return records, errors
    day = 0
    prev_time: Fraction | None = None
    period = Fraction(24 * 3600)
    timeline: list[Fraction] = []
    for rec in records:
        base = frame_to_time(rec.frame, rec.rate_fps, rec.drop_frame)
        if prev_time is not None and cfg.enable_wrap:
            jump = prev_time - base
            if jump >= WRAP_RATIO * period:
                day += 1
                errors.append(_err(rec, "cross-day-wrap",
                                   f"跨日回绕 +{day}d", "wrap", True))
            elif base <= prev_time:
                rec.flags.append(
                    "duplicate-timecode" if base == prev_time
                    else "ambiguous-rewind"
                )
                label = "重复时间码" if base == prev_time else "非单调回退"
                errors.append(_err(
                    rec, "ambiguous-timestamp",
                    f"{label} {float(prev_time - base):.3f}s，保留原值",
                    "flag-ambiguous", False,
                ))
        value = base + day * period
        rec.timeline = value
        timeline.append(value)
        prev_time = value
    if cfg.enable_drift and cfg.drift_limit > 0 and len(timeline) >= 2:
        _repair_drift(records, timeline, cfg, errors)
    return records, errors


def _repair_drift(
    records: list[TimecodeRecord],
    timeline: list[Fraction],
    cfg: RepairConfig,
    errors: list[ErrorRecord],
) -> None:
    """以中位斜率做稳健线性拟合，超阈值的残差标记而不静默修正。"""

    n = len(timeline)
    xs = list(range(n))
    slope, intercept = _median_line(xs, timeline)
    corrected: list[Fraction] = []
    for i, rec in enumerate(records):
        ideal = intercept + slope * i
        residual = timeline[i] - ideal
        if abs(residual) > cfg.drift_limit:
            rec.flags.append("drift-conflict")
            errors.append(_err(
                rec, "drift-over-limit",
                f"漂移残差 {float(abs(residual)):.3f}s 超过阈值 "
                f"{float(cfg.drift_limit):.3f}s，保留原值",
                "conflict-mark", False,
            ))
            corrected.append(timeline[i])
        else:
            if residual != 0:
                errors.append(_err(
                    rec, "drift-clamp",
                    f"漂移修正 {float(residual):+.3f}s",
                    "drift-clamp", True,
                ))
            corrected.append(ideal)
    for rec, value in zip(records, corrected):
        rec.timeline = value


def _median_line(
    xs: list[int], ys: list[Fraction]
) -> tuple[Fraction, Fraction]:
    """Theil-Sen 风格中位斜率 + 中位截距的有理数稳健拟合。"""

    slopes = [
        (ys[j] - ys[i]) / (j - i)
        for i in range(len(xs))
        for j in range(i + 1, len(xs))
    ]
    slope = statistics.median(slopes)
    intercepts = [ys[i] - slope * i for i in range(len(xs))]
    return slope, statistics.median(intercepts)


def repair_cues(
    cues: list[Cue],
) -> tuple[list[Cue], list[ErrorRecord]]:
    """处理重复序号与重叠区间；重叠只标记不移动。"""

    errors: list[ErrorRecord] = []
    seen: set[int] = set()
    next_seq = max((c.seq for c in cues), default=0) + 1
    for cue in cues:
        if cue.seq in seen:
            errors.append(ErrorRecord(
                source=cue.ref.source, start=cue.ref.start, end=cue.ref.end,
                stage="repair", reason=f"重复序号 {cue.seq}，重新编号为 {next_seq}",
                action="resequence", recoverable=True,
            ))
            cue.seq = next_seq
            next_seq += 1
        seen.add(cue.seq)
    ordered = sorted(cues, key=lambda c: (c.start, c.end))
    prev_end: Fraction | None = None
    prev_cue: Cue | None = None
    for cue in ordered:
        if prev_end is not None and cue.start < prev_end:
            cue.flags.append("overlap-conflict")
            if prev_cue is not None:
                prev_cue.flags.append("overlap-conflict")
            errors.append(ErrorRecord(
                source=cue.ref.source, start=cue.ref.start, end=cue.ref.end,
                stage="repair",
                reason="cue 与前序区间重叠，禁止静默移动",
                action="conflict-mark", recoverable=False,
            ))
        if prev_end is None or cue.end > prev_end:
            prev_end, prev_cue = cue.end, cue
    return ordered, errors


def _err(
    rec: TimecodeRecord, reason: str, detail: str, action: str, ok: bool
) -> ErrorRecord:
    return ErrorRecord(
        source=rec.ref.source, start=rec.ref.start, end=rec.ref.end,
        stage="repair", reason=f"{reason}: {detail}",
        action=action, recoverable=ok,
    )
