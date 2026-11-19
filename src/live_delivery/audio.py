"""NumPy / SoundFile 音频变换：重采样、声道布局归一、响度与峰值控制。"""
from __future__ import annotations

import math

import numpy as np

TARGET_LAYOUTS = {"mono": 1, "stereo": 2}
SILENCE_THRESHOLD_DBFS = -60.0


def resample(data: np.ndarray, src_rate: int, dst_rate: int) -> np.ndarray:
    """线性插值重采样；末尾不足一帧的残余样本按位置截断保留。"""
    if src_rate == dst_rate or data.shape[1] == 0:
        return data
    n_in = data.shape[1]
    n_out = max(1, int(round(n_in * dst_rate / src_rate)))
    pos = np.minimum(np.arange(n_out) * (src_rate / dst_rate), n_in - 1)
    idx = np.arange(n_in)
    return np.stack([np.interp(pos, idx, ch) for ch in data]).astype(np.float32)


def normalize_layout(data: np.ndarray, src_layout: str, dst_layout: str) -> np.ndarray:
    """声道布局归一：单声道 ↔ 立体声，多余声道截取前 N 路。"""
    dst_ch = TARGET_LAYOUTS[dst_layout]
    src_ch = data.shape[0]
    if src_ch == dst_ch:
        return data
    if dst_ch == 1:
        return data.mean(axis=0, keepdims=True).astype(np.float32)
    if src_ch == 1:
        return np.repeat(data, dst_ch, axis=0)
    return np.ascontiguousarray(data[:dst_ch])


# ---------------------------------------------------------------- K 加权滤波


def _high_shelf(rate: int) -> tuple[list[float], list[float]]:
    """BS.1770 前级高架滤波器（RBJ 双二阶）。"""
    fc, gain_db, q = 1681.9744509555319, 3.999843853973347, 0.7071752369554196
    A = 10.0 ** (gain_db / 40.0)
    w0 = 2.0 * math.pi * fc / rate
    alpha = math.sin(w0) / (2.0 * q)
    cw = math.cos(w0)
    root = 2.0 * math.sqrt(A) * alpha
    b0 = A * ((A + 1.0) + (A - 1.0) * cw + root)
    b1 = -2.0 * A * ((A - 1.0) + (A + 1.0) * cw)
    b2 = A * ((A + 1.0) + (A - 1.0) * cw - root)
    a0 = (A + 1.0) - (A - 1.0) * cw + root
    a1 = 2.0 * ((A - 1.0) - (A + 1.0) * cw)
    a2 = (A + 1.0) - (A - 1.0) * cw - root
    return [b0 / a0, b1 / a0, b2 / a0], [a1 / a0, a2 / a0]


def _high_pass(rate: int) -> tuple[list[float], list[float]]:
    """BS.1770 RLB 高通滤波器（RBJ 双二阶）。"""
    fc, q = 38.13547087613982, 0.5003270373253953
    w0 = 2.0 * math.pi * fc / rate
    alpha = math.sin(w0) / (2.0 * q)
    cw = math.cos(w0)
    b0 = (1.0 + cw) / 2.0
    b1 = -(1.0 + cw)
    b2 = (1.0 + cw) / 2.0
    a0 = 1.0 + alpha
    a1 = -2.0 * cw
    a2 = 1.0 - alpha
    return [b0 / a0, b1 / a0, b2 / a0], [a1 / a0, a2 / a0]


def _biquad_run(x: np.ndarray, b: list[float], a: list[float],
                state: tuple[float, float]) -> tuple[np.ndarray, tuple[float, float]]:
    """直接 II 型双二阶，携带状态以支持跨块流式处理。"""
    b0, b1, b2 = b
    a1, a2 = a
    z1, z2 = state
    y = np.empty(x.shape[0], dtype=np.float64)
    for i in range(x.shape[0]):
        xi = x[i]
        yi = b0 * xi + z1
        z1 = b1 * xi - a1 * yi + z2
        z2 = b2 * xi - a2 * yi
        y[i] = yi
    return y, (z1, z2)


class LoudnessMeter:
    """BS.1770 综合响度：K 加权、400ms 块、75% 重叠、两级门控（流式）。"""

    BLOCK_S = 0.400
    HOP_S = 0.100
    OFFSET = -0.691

    def __init__(self, rate: int, channels: int) -> None:
        self.rate = rate
        self.channels = channels
        self._block = int(round(rate * self.BLOCK_S))
        self._hop = int(round(rate * self.HOP_S))
        self._filters = [(_high_shelf(rate), _high_pass(rate)) for _ in range(channels)]
        self._states = [[(0.0, 0.0), (0.0, 0.0)] for _ in range(channels)]
        self._buf = np.zeros((channels, 0), dtype=np.float64)
        self.energies: list[float] = []

    def update(self, data: np.ndarray) -> None:
        if data.shape[1] == 0:
            return
        weighted = np.empty((data.shape[0], data.shape[1]), dtype=np.float64)
        for ch in range(min(data.shape[0], self.channels)):
            (bs, as_), (bh, ah) = self._filters[ch]
            y, self._states[ch][0] = _biquad_run(
                data[ch].astype(np.float64), bs, as_, self._states[ch][0])
            y, self._states[ch][1] = _biquad_run(y, bh, ah, self._states[ch][1])
            weighted[ch] = y
        self._buf = np.concatenate([self._buf, weighted], axis=1)
        while self._buf.shape[1] >= self._block:
            block = self._buf[:, : self._block]
            # 各声道等权：Σ z_i = 声道数 × 平均能量
            self.energies.append(float(np.mean(block ** 2)) * self.channels)
            self._buf = self._buf[:, self._hop:]

    def integrated(self) -> float | None:
        return gated_loudness(self.energies)


def gated_loudness(energies: list[float], offset: float = LoudnessMeter.OFFSET
                   ) -> float | None:
    """两级门控（绝对 -70 LUFS，相对 -10 LU）；无有效块返回 None。"""
    e = np.asarray([v for v in energies if v > 0], dtype=np.float64)
    if e.size == 0:
        return None
    lufs = offset + 10.0 * np.log10(e)
    abs_gate = lufs > -70.0
    if not abs_gate.any():
        return None
    mean1 = float(np.mean(e[abs_gate]))
    rel_gate = offset + 10.0 * math.log10(mean1) - 10.0
    keep = abs_gate & (lufs > rel_gate)
    if not keep.any():
        return None
    return offset + 10.0 * math.log10(float(np.mean(e[keep])))


def true_peak_dbfs(data: np.ndarray, oversample: int = 4) -> float:
    """4 倍过采样近似的真实峰值（dBFS）；空输入返回 -inf。"""
    peak = 0.0
    for ch in data:
        n = ch.shape[0]
        if n == 0:
            continue
        pos = np.arange(n * oversample) / oversample
        up = np.interp(pos, np.arange(n), ch)
        m = float(np.max(np.abs(up)))
        if m > peak:
            peak = m
    return 20.0 * math.log10(peak) if peak > 0 else -math.inf


def gain_db(measured_lufs: float | None, target_lufs: float,
            peak_dbfs: float, max_peak_dbfs: float) -> float:
    """按目标响度计算增益，并限制增益后峰值不超过上限以避免削波。"""
    if measured_lufs is None:
        return 0.0
    gain = target_lufs - measured_lufs
    if math.isfinite(peak_dbfs) and peak_dbfs + gain > max_peak_dbfs:
        gain = max_peak_dbfs - peak_dbfs
    return gain


def is_silence(data: np.ndarray, threshold_dbfs: float = SILENCE_THRESHOLD_DBFS) -> bool:
    if data.size == 0:
        return True
    rms = float(np.sqrt(np.mean(data.astype(np.float64) ** 2)))
    return rms < 10.0 ** (threshold_dbfs / 20.0)
