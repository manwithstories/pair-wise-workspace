"""共享整数媒体刻度与舍入规则。

所有模块（修复、合成、渲染、清单）统一使用整数刻度描述时间，
避免浮点漂移。默认 90000 刻度/秒（MPEG 传统时基），可被
24/25/30/60 fps 与 48kHz 音频整除。
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction

from .errors import TimebaseError

DEFAULT_TICKS_PER_SECOND = 90_000
MAX_TICKS_PER_SECOND = 10_000_000


@dataclass(frozen=True)
class TimeBase:
    """整数时间基准：1 秒 = ticks_per_second 刻度。"""

    ticks_per_second: int = DEFAULT_TICKS_PER_SECOND

    def __post_init__(self) -> None:
        tps = self.ticks_per_second
        if not isinstance(tps, int) or isinstance(tps, bool):
            raise TimebaseError(f"时间基准必须为整数，得到 {tps!r}")
        if tps <= 0 or tps > MAX_TICKS_PER_SECOND:
            raise TimebaseError(f"无法表示的时间基准: {tps}")

    def to_ticks(self, seconds: float | Fraction) -> int:
        """秒 → 整数刻度，采用 round-half-even（银行家舍入）。"""
        value = Fraction(seconds).limit_denominator(10**9) * self.ticks_per_second
        return _round_half_even(value)

    def ticks_to_seconds(self, ticks: int) -> float:
        return ticks / self.ticks_per_second

    def ticks_to_ms(self, ticks: int) -> float:
        return ticks * 1000.0 / self.ticks_per_second

    def frame_ticks(self, fps: int) -> int:
        """每帧刻度数；不能整除时拒绝，保证帧边界可精确表示。"""
        if fps <= 0:
            raise TimebaseError(f"非法帧率: {fps}")
        if self.ticks_per_second % fps != 0:
            raise TimebaseError(
                f"时间基准 {self.ticks_per_second} 无法整除帧率 {fps}"
            )
        return self.ticks_per_second // fps

    def require_duration(self, ticks: int, what: str = "时长") -> int:
        """拒绝负时长，返回非负刻度值。"""
        if ticks < 0:
            raise TimebaseError(f"{what}不能为负: {ticks}")
        return ticks


def _round_half_even(value: Fraction) -> int:
    floor = value.numerator // value.denominator
    remainder = value - floor
    if remainder < Fraction(1, 2):
        return floor
    if remainder > Fraction(1, 2):
        return floor + 1
    return floor if floor % 2 == 0 else floor + 1
