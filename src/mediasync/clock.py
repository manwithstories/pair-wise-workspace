"""共享媒体时间基准。

用有理数（:class:`fractions.Fraction`）统一视频帧、音频样本与索引时间，
全程不做浮点累加。时间戳回退或编码器重启时切分 *epoch* 分段记录，
保留源时间原值，绝不静默改写。
"""

from __future__ import annotations

import dataclasses
from fractions import Fraction

from .errors import ErrorBook, ErrorCode


@dataclasses.dataclass(frozen=True)
class TimedPoint:
    """一个带时间的媒体点（帧 / 样本块 / 索引项）。"""

    source_tick: int
    """源时间轴上的原始整数值，原样保留。"""
    epoch: int
    """所属连续时间段编号，从 0 开始。"""
    continuous_ms: Fraction
    """跨 epoch 拼接后的连续时间（毫秒，有理数）。"""
    kind: str = "frame"

    @property
    def seconds(self) -> Fraction:
        return self.continuous_ms / 1000

    def ms_rounded(self) -> int:
        """四舍五入到整数毫秒，仅用于展示与清单输出。"""
        return int(self.continuous_ms + Fraction(1, 2))


class MediaClock:
    """单一时间基准：负责 tick / 样本 / 毫秒之间的有理数换算。"""

    def __init__(self, timescale: int) -> None:
        if timescale <= 0:
            raise ValueError("timescale 必须为正整数")
        self.timescale = timescale

    def tick_to_seconds(self, tick: int) -> Fraction:
        return Fraction(tick, self.timescale)

    def tick_to_ms(self, tick: int) -> Fraction:
        return Fraction(tick * 1000, self.timescale)

    def ms_to_tick(self, ms: Fraction | int) -> int:
        value = Fraction(ms) * self.timescale / 1000
        return int(value)  # 清单规划只使用整除边界

    def samples_to_ms(self, sample_index: int, sample_rate: int) -> Fraction:
        if sample_rate <= 0:
            raise ValueError("sample_rate 必须为正整数")
        return Fraction(sample_index * 1000, sample_rate)

    def ms_to_samples(self, ms: Fraction | int, sample_rate: int) -> int:
        return int(Fraction(ms) * sample_rate / 1000)

    def align_ms(self, ms: Fraction | int, grid_ms: int) -> Fraction:
        """向下对齐到固定毫秒网格。"""
        value = Fraction(ms)
        grid = Fraction(grid_ms)
        return (value // grid) * grid


class TimelineBuilder:
    """按喂入顺序构建连续时间线，检测回退与编码器重启。

    - 时间戳严格递增时归入当前 epoch；
    - 出现回退（新 tick < 上一 tick）时记录 ``timestamp_backward`` 并开新 epoch；
    - 显式 :meth:`mark_restart` 记录 ``encoder_restart`` 并开新 epoch；
    - 新 epoch 从连续时间线的当前末端继续，源 tick 原值保留。
    """

    def __init__(self, clock: MediaClock, source: str, errors: ErrorBook) -> None:
        self.clock = clock
        self.source = source
        self.errors = errors
        self.epoch = 0
        self._last_tick: int | None = None
        self._epoch_base_tick: int | None = None
        self._epoch_origin_ms = Fraction(0)
        self._continuous_end_ms = Fraction(0)
        self.points: list[TimedPoint] = []

    @property
    def epoch_count(self) -> int:
        return self.epoch + 1

    def mark_restart(self, at_tick: int) -> None:
        self.errors.add(
            ErrorCode.ENCODER_RESTART,
            f"{self.source}: 编码器在 tick={at_tick} 重启，切换到新时间段",
            source=self.source,
            detail={"tick": at_tick, "new_epoch": self.epoch + 1},
            fatal=False,
        )
        self._open_epoch(at_tick)

    def _open_epoch(self, tick: int) -> None:
        self.epoch += 1
        self._epoch_origin_ms = self._continuous_end_ms
        self._epoch_base_tick = None
        self._last_tick = None

    def add(self, tick: int, kind: str = "frame") -> TimedPoint:
        if self._last_tick is not None and tick < self._last_tick:
            self.errors.add(
                ErrorCode.TIMESTAMP_BACKWARD,
                f"{self.source}: 时间戳回退 {self._last_tick} -> {tick}",
                source=self.source,
                timestamp_ms=int(self._continuous_end_ms),
                detail={"previous_tick": self._last_tick, "tick": tick},
                fatal=False,
            )
            self._open_epoch(tick)
        if self._epoch_base_tick is None:
            self._epoch_base_tick = tick
        # 新 epoch 内以首 tick 为局部零点，保留源时间间隔、不改写源值。
        local_ms = self.clock.tick_to_ms(tick - self._epoch_base_tick)
        continuous = self._epoch_origin_ms + local_ms
        point = TimedPoint(
            source_tick=tick,
            epoch=self.epoch,
            continuous_ms=continuous,
            kind=kind,
        )
        self.points.append(point)
        self._last_tick = tick
        if continuous > self._continuous_end_ms:
            self._continuous_end_ms = continuous
        return point

    @property
    def duration_ms(self) -> Fraction:
        return self._continuous_end_ms

    def epochs(self) -> list[tuple[int, Fraction, Fraction]]:
        """返回每个 epoch 的 (编号, 连续起点 ms, 连续终点 ms)。"""
        out: list[tuple[int, Fraction, Fraction]] = []
        for ep in range(self.epoch + 1):
            pts = [p for p in self.points if p.epoch == ep]
            if pts:
                out.append((ep, pts[0].continuous_ms, pts[-1].continuous_ms))
        return out
