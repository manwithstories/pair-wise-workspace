"""多制式 SMPTE 时间码解析与有理数时间换算。

支持 24/25/30/30000/1001 等分数帧率的非丢帧（``HH:MM:SS:FF``）
与丢帧（``HH:MM:SS;FF``，分隔符也可为 ``.`` 或末尾 ``;``）制式；
丢帧制式下非法帧号（每分钟前 2 帧、10 分钟整点除外）直接拒绝。
"""
from __future__ import annotations

import re
from fractions import Fraction

from .model import FieldIndexError

#: 常用帧率预设（名称 -> 精确帧率）。
RATE_PRESETS: dict[str, Fraction] = {
    "24": Fraction(24, 1),
    "25": Fraction(25, 1),
    "30": Fraction(30, 1),
    "29.97": Fraction(30000, 1001),
    "30000/1001": Fraction(30000, 1001),
    "23.976": Fraction(24000, 1001),
}

_TC_RE = re.compile(
    r"^\s*(\d{1,2})[:;.](\d{2})[:;.](\d{2})[:;.,](\d{1,2})\s*$"
)


class TimecodeError(FieldIndexError):
    """时间码语法或帧号非法。"""


def parse_rate(rate: str | Fraction | int | float) -> Fraction:
    """把帧率配置解析为精确 Fraction，支持 '30000/1001' 这类分数串。"""

    if isinstance(rate, Fraction):
        return rate
    if isinstance(rate, int):
        return Fraction(rate, 1)
    if isinstance(rate, float):
        return Fraction(rate).limit_denominator(1_000_000)
    text = str(rate).strip()
    if text in RATE_PRESETS:
        return RATE_PRESETS[text]
    if "/" in text:
        num, den = text.split("/", 1)
        return Fraction(int(num), int(den))
    return Fraction(text).limit_denominator(1_000_000)


def nominal_fps(rate: Fraction) -> int:
    """取标称整数帧率（丢帧判定按 30 系帧率处理）。"""

    return round(float(rate))


def is_drop_frame(raw: str) -> bool:
    """根据分隔符判断是否为丢帧制式：含 ';' 即 DF。"""

    return ";" in raw


def parse_timecode(raw: str, rate: Fraction) -> tuple[int, bool]:
    """解析时间码文本，返回 (24h 周期内帧序号, 是否丢帧)。

    丢帧制式仅允许 30 系帧率；非法丢帧帧号抛出 TimecodeError。
    """

    match = _TC_RE.match(raw)
    if not match:
        raise TimecodeError(f"无法识别的时间码: {raw!r}")
    hh, mm, ss, ff = (int(g) for g in match.groups())
    drop = is_drop_frame(raw)
    fps = nominal_fps(rate)
    if mm >= 60 or ss >= 60 or hh >= 24 or ff >= fps:
        raise TimecodeError(f"时间码字段越界: {raw!r}")
    if drop and fps != 30:
        raise TimecodeError(f"非 30 系帧率不支持丢帧制式: {raw!r} @ {rate}")
    frames = _to_frame_number(hh, mm, ss, ff, fps, drop)
    if frames >= fps * 3600 * 24:
        raise TimecodeError(f"时间码超出 24 小时周期: {raw!r}")
    return frames, drop


def _to_frame_number(
    hh: int, mm: int, ss: int, ff: int, fps: int, drop: bool
) -> int:
    """SMPTE ST 12-1 帧序号换算，并校验丢帧帧号合法性。"""

    total_minutes = 60 * hh + mm
    total_seconds = 60 * total_minutes + ss
    frames = total_seconds * fps + ff
    if drop:
        dropped = 2 * (total_minutes - total_minutes // 10)
        frames -= dropped
        if ff < 2 and ss == 0 and mm % 10 != 0:
            raise TimecodeError(
                f"丢帧制式非法帧号（每分钟前 2 帧已丢弃）: "
                f"{hh:02d}:{mm:02d}:{ss:02d};{ff:02d}"
            )
    return frames


def frame_to_time(
    frame: int, rate: Fraction, drop: bool = False
) -> Fraction:
    """帧序号 -> 周期内时间（秒，Fraction）。"""

    fps = nominal_fps(rate)
    if drop:
        if fps != 30:
            raise TimecodeError(f"非 30 系帧率不支持丢帧制式 @ {rate}")
        # ST 12-1 逆映射：先按整分钟估算丢帧事件数，再用边界帧号修正。
        # 相邻丢帧分钟边界 N 间距为 1798，10 分钟整点间距为 1800。
        frame = _undo_drop(frame)
    return Fraction(frame, rate)


def _undo_drop(frame: int) -> int:
    """丢帧帧序号 N -> 连续计数（补回被跳过的 2 帧/分钟，10 分钟除外）。

    第 m 分钟第一个合法帧的 N 序号为
    ``1800m - 2d(m) + (2 若 m 非 10 整点)``，其中 ``d(m)=m-m//10``；
    对分钟数二分即可精确求出已发生的丢帧事件数。
    """

    lo, hi = 0, frame // 1798 + 2
    while lo < hi:
        mid = (lo + hi + 1) // 2
        if _first_frame_of_minute(mid) <= frame:
            lo = mid
        else:
            hi = mid - 1
    events = lo - lo // 10
    return frame + 2 * events


def _first_frame_of_minute(minute: int) -> int:
    """第 minute 分钟（自零点起）第一个合法显示帧对应的 N 序号。"""

    dropped = minute - minute // 10
    return 1800 * minute - 2 * dropped + (2 if minute % 10 else 0)


def format_timecode(frame: int, rate: Fraction, drop: bool = False) -> str:
    """帧序号 -> 规范时间码文本（DF 用 ';' 分隔）。"""

    fps = nominal_fps(rate)
    frame_eff = _undo_drop(frame) if drop else frame
    hh, rem = divmod(frame_eff, fps * 3600)
    mm, rem = divmod(rem, fps * 60)
    ss, ff = divmod(rem, fps)
    sep = ";" if drop else ":"
    return f"{hh:02d}:{mm:02d}:{ss:02d}{sep}{ff:02d}"
