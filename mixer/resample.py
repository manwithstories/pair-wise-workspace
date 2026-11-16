"""确定性多采样率重采样。

采用加 Hann 窗的归一化 sinc 插值(窗宽固定 taps=32):
- 纯函数式计算,不使用随机数,任何分块方式产出逐采样相同的序列;
- 下采样时截止频率按 ratio 收缩,防止混叠;
- 边界以零填充,flush 后输出长度恰为 ceil(输入帧数 * ratio);
- 内部一律 float64,保证离线/流式两条路径共享同一数值。
"""

from __future__ import annotations

import math

import numpy as np

from .types import ConfigError


def _sinc(x: np.ndarray) -> np.ndarray:
    """向量化 sinc,精确处理 x=0。"""

    out = np.ones_like(x, dtype=np.float64)
    nz = x != 0.0
    px = np.pi * x[nz]
    out[nz] = np.sin(px) / px
    return out


class Resampler:
    """流式重采样器,按块喂入、按块产出,块大小不影响结果。

    输入输出均为形状 (帧数, 声道数) 的 ndarray;单声道可传 1D,
    产出也保持 1D。
    """

    def __init__(self, in_rate: int, out_rate: int, taps: int = 32) -> None:
        if not isinstance(in_rate, int) or not isinstance(out_rate, int):
            raise ConfigError("采样率必须是整数")
        if in_rate <= 0 or out_rate <= 0:
            raise ConfigError("采样率必须为正整数")
        if taps < 4 or taps % 2 != 0:
            raise ConfigError("taps 必须是不小于 4 的偶数")
        self.in_rate = in_rate
        self.out_rate = out_rate
        self.ratio = out_rate / in_rate
        self.identity = in_rate == out_rate
        self.taps = taps
        self._half = taps // 2
        self._cutoff = min(1.0, self.ratio)
        self._buf: np.ndarray | None = None
        self._one_d = False
        self._in_consumed = 0
        self._out_produced = 0

    # ---- 长度规划 -------------------------------------------------------

    def output_frames_for(self, input_frames: int) -> int:
        """给定整段输入帧数,返回最终输出帧数(含边界零填充)。"""

        return math.ceil(input_frames * self.ratio)

    # ---- 内部核 ---------------------------------------------------------

    def _drain(self, allow_flush: bool) -> np.ndarray:
        assert self._buf is not None
        n = self._buf.shape[0]
        n_abs = self._in_consumed + n
        total_in = n_abs  # flush 时全部输入都已到达
        half = self._half
        ratio = self.ratio
        cutoff = self._cutoff
        cols = np.arange(-half + 1, half + 1, dtype=np.float64)

        emitted: list[np.ndarray] = []
        while True:
            j = self._out_produced
            if not allow_flush and j >= self.output_frames_for(total_in):
                # 非 flush 阶段也不允许越过规划长度
                break
            center = (j + 0.5) / ratio - 0.5
            base = math.floor(center)
            if not allow_flush and base + half >= n_abs:
                # 右边界所需未来样本尚未到达
                break
            if allow_flush and j >= self.output_frames_for(total_in):
                break

            # 一次尽量多地连续产出,减少 Python 循环
            j_end = j + 1
            if allow_flush:
                j_end = self.output_frames_for(total_in)
            else:
                while True:
                    nxt = j_end
                    c_nxt = (nxt + 0.5) / ratio - 0.5
                    if math.floor(c_nxt) + half >= n_abs:
                        break
                    if nxt >= self.output_frames_for(total_in):
                        break
                    j_end = nxt + 1
            if j_end <= j:
                if allow_flush:
                    break
                break

            jj = np.arange(j, j_end, dtype=np.float64)
            centers = (jj + 0.5) / ratio - 0.5
            bases = np.floor(centers).astype(np.int64)
            fracs = centers - bases
            u = cols[None, :] - fracs[:, None]
            window = 0.5 * (1.0 + np.cos(np.pi * u / half))
            window = np.where(np.abs(u) <= half, window, 0.0)
            weights = _sinc(u * cutoff) * cutoff * window

            idx = bases[:, None] + cols.astype(np.int64)[None, :]
            valid = (idx >= self._in_consumed) & (idx < n_abs)
            rel = idx - self._in_consumed
            rel_clip = np.clip(rel, 0, n - 1)
            gathered = self._buf[rel_clip]  # (m, taps, ch)
            w = weights * valid
            wsum = w.sum(axis=1, keepdims=True)
            safe = np.where(wsum > 0.0, wsum, 1.0)
            w = w / safe
            out = np.einsum("mzc,mz->mc", gathered, w)
            out = np.where(wsum > 0.0, out, 0.0)
            emitted.append(out)
            self._out_produced = j_end

        if not emitted:
            ch = 1 if self._one_d else self._buf.shape[1]
            empty = np.zeros((0, ch), dtype=np.float64)
            return empty[:, 0] if self._one_d else empty

        result = np.concatenate(emitted, axis=0)
        # 丢弃未来输出不再需要的历史样本
        if self._out_produced > 0:
            c_next = (self._out_produced + 0.5) / ratio - 0.5
            min_needed = math.floor(c_next) - half + 1
            keep = min_needed - self._in_consumed
            if keep > 0:
                self._buf = self._buf[keep:]
                self._in_consumed += keep
        return result[:, 0] if self._one_d else result

    # ---- 公共 API -------------------------------------------------------

    def feed(self, chunk: np.ndarray) -> np.ndarray:
        """喂入一块输入,返回当前可确定的输出(右边界样本留给后续)。"""

        arr = np.asarray(chunk, dtype=np.float64)
        squeeze = arr.ndim == 1
        if squeeze:
            arr = arr[:, None]
        if arr.ndim != 2:
            raise ConfigError("重采样输入必须是 1D 或 2D 数组")
        self._one_d = squeeze
        if self.identity:
            self._in_consumed += arr.shape[0]
            self._out_produced += arr.shape[0]
            return arr[:, 0].copy() if squeeze else arr.copy()

        self._buf = arr if self._buf is None else np.concatenate([self._buf, arr], axis=0)
        return self._drain(allow_flush=False)

    def flush(self) -> np.ndarray:
        """补齐右边界零填充,输出剩余全部样本。"""

        if self.identity or self._buf is None:
            return np.zeros(0, dtype=np.float64)
        out = self._drain(allow_flush=True)
        # 已无后续输出,释放残余缓冲
        self._buf = None
        return out


def resample_whole(signal: np.ndarray, in_rate: int, out_rate: int) -> np.ndarray:
    """一次性重采样便捷函数(测试与夹具使用)。"""

    rs = Resampler(in_rate, out_rate)
    head = rs.feed(signal)
    tail = rs.flush()
    return np.concatenate([head, tail], axis=0)
