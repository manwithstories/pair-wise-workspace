"""整数采样刻度与统一时间戳换算规则。

主时间线使用 *整数采样刻度（tick）*：1 tick = 主采样率下的 1 个采样
周期，所有轨道偏移、块长度、交叠区间与输出窗口都以 tick 描述，避免
浮点时间戳在跨节点重建时产生漂移。

确定性策略（全项目统一，禁止在别处另立规则）：
    * 重采样：线性插值，采样中心映射
        src_pos = (dst_i + 0.5) * src_rate / dst_rate - 0.5
    * 标量 tick 换算：四舍六入五成双（round-half-even）
    * 静音填充：精确 0.0，不叠加抖动（无 dither）
    * int16 量化：先裁剪到 [-1.0, 1.0]，再 round-half-even
"""

from dataclasses import dataclass
from fractions import Fraction

import numpy as np

RESAMPLE_METHOD = "linear"
FILL_VALUE = 0.0
QUANTIZE_DTYPE = np.int16
INT16_MAX = 32767
INT16_MIN = -32768


@dataclass(frozen=True)
class TimeBase:
    """主时间基准：固定整数采样率。"""

    rate: int

    def __post_init__(self) -> None:
        if not isinstance(self.rate, int) or isinstance(self.rate, bool):
            raise TypeError("master rate must be an integer sample rate")
        if self.rate <= 0:
            raise ValueError("sample rate must be positive")

    def to_seconds(self, tick: int) -> float:
        """整数 tick -> 秒（仅用于展示，不参与定位）。"""
        return tick / self.rate

    def fraction(self) -> Fraction:
        """PyAV time_base = 1 / master_rate，pts 即整数 tick。"""
        return Fraction(1, self.rate)

    def src_position(self, master_tick: float, src_rate: int) -> float:
        """主刻度位置 -> 源采样率下的精确（浮点）采样位置。"""
        if src_rate <= 0:
            raise ValueError("source sample rate must be positive")
        return master_tick * (src_rate / self.rate)

    def master_position(self, src_tick: float, src_rate: int) -> float:
        """源采样位置 -> 主刻度下的精确（浮点）位置。"""
        if src_rate <= 0:
            raise ValueError("source sample rate must be positive")
        return src_tick * (self.rate / src_rate)

    def to_source_frames(self, length_ticks: int, src_rate: int) -> int:
        """主刻度块长度 -> 源块应有的采样帧数（round-half-even）。"""
        if length_ticks < 0:
            raise ValueError("length must be non-negative")
        return int(round(length_ticks * src_rate / self.rate))

    def to_master_ticks(self, src_frames: int, src_rate: int) -> int:
        """源采样帧数 -> 主刻度长度（round-half-even）。"""
        if src_frames < 0:
            raise ValueError("frame count must be non-negative")
        return int(round(src_frames * self.rate / src_rate))


def quantize_int16(samples: np.ndarray) -> np.ndarray:
    """float 波形确定性量化为 int16：裁剪 + round-half-even，无抖动。"""
    clipped = np.clip(samples, -1.0, 1.0)
    # numpy 的 around 即 round-half-even；astype 只做无损收窄。
    return np.rint(clipped * INT16_MAX).astype(QUANTIZE_DTYPE)
