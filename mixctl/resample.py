"""有理比多相重采样（全程 float64）。

比例 ``target_sr / source_sr`` 经最大公约数化简为 ``L/M``（例如
44100 -> 48000 为 160/147）。先设计截止于归一化角频率
``min(1, L/M) * pi`` 的加窗 sinc 低通，再按多相分解得到 ``L`` 个
相位的 FIR 组。sinc 原型在通带内直流增益恰好为 L，无需额外归一化。

重采样器维护“下一个输出对应的输入游标”和一段输入历史，状态随输入
单调推进，块边界不保存任何曲线相位之外的东西，因此按任意块长切分
都能逐样本复现离线整段结果。
"""

from __future__ import annotations

from dataclasses import dataclass
from math import gcd

import numpy as np


def rational_ratio(source_sr: int, target_sr: int) -> tuple[int, int]:
    if source_sr <= 0 or target_sr <= 0:
        raise ValueError(f"采样率必须为正数: source={source_sr} target={target_sr}")
    divisor = gcd(int(source_sr), int(target_sr))
    return int(target_sr) // divisor, int(source_sr) // divisor


def _design_phases(up: int, down: int, half_taps: int = 16) -> np.ndarray:
    """返回形状 (up, 2*half_taps) 的多相 FIR 组。

    原型 h[k] 定义在 k 属于 [-half_taps*up, half_taps*up) 的整数网格；
    多相行 p 收集满足 k % up == p 的系数，保证每相抽头数相同。
    """
    cutoff = min(1.0, float(up) / float(down))
    radius = half_taps * up
    positions = np.arange(-radius, radius, dtype=np.float64)
    normalized = positions / up
    with np.errstate(divide="ignore", invalid="ignore"):
        kernel = np.sinc(cutoff * normalized) * cutoff
    kernel *= np.hanning(2 * radius).astype(np.float64)
    phases = np.empty((up, 2 * half_taps), dtype=np.float64)
    for phase in range(up):
        phases[phase] = kernel[phase::up][: 2 * half_taps]
    return phases


@dataclass
class RatioInfo:
    source_sr: int
    target_sr: int
    numerator: int
    denominator: int

    @property
    def ratio_text(self) -> str:
        return f"{self.numerator}/{self.denominator}"


class Resampler:
    """单源采样率到目标采样率的流式多相重采样器（逐声道一致处理）。"""

    def __init__(self, source_sr: int, target_sr: int, half_taps: int = 16):
        self.source_sr = int(source_sr)
        self.target_sr = int(target_sr)
        self.up, self.down = rational_ratio(source_sr, target_sr)
        self.identity = self.up == self.down
        self.half_taps = int(half_taps)
        self._phases = None if self.identity else _design_phases(
            self.up, self.down, self.half_taps
        )
        self._buffer = np.empty((0, 0), dtype=np.float64)
        self._buf_start = 0      # 缓冲首样本对应的输入绝对下标
        self._pushed_total = 0   # 已推入的输入样本总数
        self._produced = 0       # 已产出的输出样本数
        self._eof = False

    @property
    def ratio(self) -> RatioInfo:
        return RatioInfo(self.source_sr, self.target_sr, self.up, self.down)

    def push(self, block: np.ndarray) -> None:
        data = np.asarray(block, dtype=np.float64)
        if data.ndim == 1:
            data = data[:, None]
        if data.ndim != 2:
            raise ValueError("音频块维度必须为 1 或 2")
        if self._buffer.size:
            if data.shape[1] != self._buffer.shape[1]:
                raise ValueError("声道数在流式输入之间发生变化")
            self._buffer = np.concatenate([self._buffer, data], axis=0)
        else:
            self._buffer = data.copy()
        self._pushed_total += data.shape[0]

    def mark_eof(self) -> None:
        self._eof = True

    def needed_input_for(self, output_count: int) -> int:
        """再产出 output_count 个样本所需的输入绝对下标（不含）。

        非 EOF 模式要求最后一个输出的抽头窗右缘 (center + half) 严格
        落在已推入缓冲内，因此需要 half 个额外前瞻样本。
        """
        if output_count <= 0:
            return self._pushed_total
        last_n = self._produced + output_count - 1
        last_center = (last_n * self.down) // self.up
        return last_center + self.half_taps + 1

    def _channels(self) -> int:
        return self._buffer.shape[1] if self._buffer.ndim == 2 else 0

    def _trim(self) -> None:
        # 未来输出引用的最小输入下标为 oldest_center - half_taps。
        oldest_center = (self._produced * self.down) // self.up
        safe_start = oldest_center - self.half_taps
        drop = safe_start - self._buf_start
        if drop > 0:
            self._buffer = self._buffer[drop:]
            self._buf_start += drop

    def read(self, max_output: int) -> np.ndarray:
        if max_output <= 0 or self._channels() == 0:
            return np.empty((0, max(self._channels(), 0)), dtype=np.float64)
        if self.identity:
            available = self._pushed_total - self._produced
            take = min(max_output, max(available, 0))
            start = self._produced - self._buf_start
            out = self._buffer[start:start + take].copy()
            self._produced += take
            self._trim()
            return out
        return self._read_polyphase(max_output)

    def _read_polyphase(self, max_output: int) -> np.ndarray:
        half = self.half_taps
        channels = self._channels()
        n_values = np.arange(
            self._produced, self._produced + max_output, dtype=np.int64
        )
        phases = (n_values * self.down) % self.up
        centers = (n_values * self.down) // self.up
        local_centers = centers - self._buf_start
        buf_len = self._buffer.shape[0]
        if self._eof:
            fit = np.nonzero(local_centers - half < buf_len)[0]
        else:
            fit = np.nonzero(local_centers + half < buf_len)[0]
        if fit.size == 0:
            return np.empty((0, channels), dtype=np.float64)
        keep = int(fit[-1]) + 1
        n_values = n_values[:keep]
        phases = phases[:keep]
        local_centers = local_centers[:keep]
        offsets = np.arange(-half, half, dtype=np.int64)
        local_idx = local_centers[:, None] + offsets[None, :]
        valid = (local_idx >= 0) & (local_idx < buf_len)
        taps = np.where(valid, self._phases[phases], 0.0)
        gathered = self._buffer[np.where(valid, local_idx, 0)]
        output = np.einsum("ntc,nt->nc", gathered, taps).astype(np.float64)
        self._produced += n_values.size
        self._trim()
        return output

    def flush(self) -> np.ndarray:
        self.mark_eof()
        pieces: list[np.ndarray] = []
        while True:
            oldest_center = (self._produced * self.down) // self.up
            if oldest_center - self.half_taps >= self._pushed_total:
                break
            piece = self.read(4096)
            if piece.shape[0] == 0:
                break
            pieces.append(piece)
        if not pieces:
            return np.empty((0, self._channels()), dtype=np.float64)
        return np.concatenate(pieces, axis=0)
