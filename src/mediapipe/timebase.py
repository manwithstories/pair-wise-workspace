"""统一整数媒体时间基准：跨轨时间一律使用整数微秒表示。

原始 PTS/DTS 与其 time_base 始终保留在包对象中；本模块只负责
不同 time_base 与整数微秒之间的确定性换算，并在结果超出 int64
范围时抛出 :class:`TimebaseError`（舍入溢出）。
"""
from __future__ import annotations

from fractions import Fraction
from typing import Union

MICROSECONDS_PER_SECOND = 1_000_000
INT64_MIN = -(1 << 63)
INT64_MAX = (1 << 63) - 1

TimeBaseLike = Union[Fraction, tuple[int, int], list[int], float]


class TimebaseError(ValueError):
    """时间戳换算失败（非法 time_base 或舍入溢出 int64）。"""


def as_fraction(time_base: TimeBaseLike) -> Fraction:
    """把 PyAV 的 time_base（Fraction/元组/浮点）归一为 Fraction。"""
    if isinstance(time_base, Fraction):
        return time_base
    if isinstance(time_base, (tuple, list)):
        num, den = int(time_base[0]), int(time_base[1])
        if den == 0:
            raise TimebaseError(f"time_base 分母为 0: {time_base!r}")
        return Fraction(num, den)
    if isinstance(time_base, (int, float)):
        if float(time_base) <= 0:
            raise TimebaseError(f"time_base 必须为正数: {time_base!r}")
        return Fraction(float(time_base)).limit_denominator(1_000_000_000)
    raise TimebaseError(f"不支持的 time_base 类型: {type(time_base)!r}")


def _check_int64(value: int, what: str) -> int:
    if not (INT64_MIN <= value <= INT64_MAX):
        raise TimebaseError(f"{what}={value} 超出有符号 64 位整数范围（舍入溢出）")
    return value


def to_microseconds(timestamp: int, time_base: TimeBaseLike) -> int:
    """按 time_base 将原始时间戳换算为整数微秒（向下取整）。

    保留负值（由流水线负责检测并报错），结果必须落在 int64 内。
    """
    tb = as_fraction(time_base)
    if tb <= 0:
        raise TimebaseError(f"time_base 必须为正数: {tb!r}")
    exact = Fraction(int(timestamp), 1) * tb * MICROSECONDS_PER_SECOND
    # floor 对负数同样有定义
    us = exact.numerator // exact.denominator
    return _check_int64(us, "timestamp_us")


def from_microseconds(micros: int, time_base: TimeBaseLike) -> int:
    """整数微秒按目标 time_base 换算回原始刻度（四舍五入到最近刻度）。"""
    tb = as_fraction(time_base)
    if tb <= 0:
        raise TimebaseError(f"time_base 必须为正数: {tb!r}")
    # ticks = us / 1e6 / tb = us * den / (num * 1e6)，全程 Fraction 精确运算
    exact = Fraction(
        int(micros) * tb.denominator,
        tb.numerator * MICROSECONDS_PER_SECOND,
    )
    # 最近刻度，正数半数向上、负数半数向下（远离零），行为确定
    q, r = divmod(abs(exact.numerator), exact.denominator)
    if r * 2 >= exact.denominator:
        q += 1
    ticks = -q if exact < 0 else q
    return _check_int64(ticks, "timestamp_ticks")


def duration_microseconds(start_us: int, end_us: int) -> int:
    """安全时长（微秒），起止倒挂时抛错而非给出负数。"""
    if end_us < start_us:
        raise TimebaseError(
            f"时间线起止倒挂: start_us={start_us} > end_us={end_us}"
        )
    return end_us - start_us
