"""音频采样率与声道布局归一。

- 支持 mono(1) / stereo(2) / 5.1(6) 三种目标布局；
- 缺声道补零、额外声道按显式策略（drop / fold）确定性处理；
- 不足一帧的尾部按 pad（补零）或 truncate（截断）记录；
- 重采样用 NumPy 线性插值，整数位置精确、无浮点累加误差；
- soundfile 负责 wav/flac 读写，全部限定在工作目录内。
"""

from __future__ import annotations

import dataclasses
from pathlib import Path

import numpy as np
import soundfile as sf

from .errors import ErrorBook, ErrorCode
from .safety import resolve_within

LAYOUT_CHANNELS = {"mono": 1, "stereo": 2, "5.1": 6}
# 5.1 标准槽位顺序（ITU）：L, R, C, LFE, Ls, Rs。
LAYOUT_51 = ("L", "R", "C", "LFE", "Ls", "Rs")


@dataclasses.dataclass(frozen=True)
class AudioSpec:
    sample_rate: int
    layout: str

    @property
    def channels(self) -> int:
        return LAYOUT_CHANNELS[self.layout]


@dataclasses.dataclass
class NormalizeResult:
    samples: np.ndarray
    """shape=(帧数, 目标声道数)，float64。"""
    spec: AudioSpec
    conversions: list[dict]
    source_frames: int
    output_frames: int


def _channel_map(src: int, dst: int, layout: str) -> np.ndarray:
    """生成 (src, dst) 线性映射矩阵，规则确定可核对。"""
    matrix = np.zeros((src, dst), dtype=np.float64)
    if dst == 1:  # 混合到单声道：等权平均。
        matrix[:, 0] = 1.0 / src
        return matrix
    if src == 1:  # 单声道上混：中央复制到 L/R，其余槽位补零。
        matrix[0, 0] = 1.0
        matrix[0, 1] = 1.0
        return matrix
    common = min(src, dst)
    for i in range(common):
        matrix[i, i] = 1.0
    if layout == "5.1" and src == 2:
        # 立体声 -> 5.1：L/R 落位，C/LFE/环绕留零（缺声道另行记录）。
        return matrix
    return matrix


def normalize_layout(
    samples: np.ndarray,
    spec: AudioSpec,
    *,
    source: str,
    errors: ErrorBook,
    extra_policy: str = "drop",
) -> tuple[np.ndarray, list[dict]]:
    """把 (n, src_ch) 归一到目标布局，返回新数组与转换记录。"""
    if samples.ndim == 1:
        samples = samples[:, None]
    src_ch = samples.shape[1]
    dst_ch = spec.channels
    conversions: list[dict] = []

    if src_ch < dst_ch:
        missing = dst_ch - src_ch
        errors.add(
            ErrorCode.AUDIO_MISSING_CHANNEL,
            f"{source}: 源声道 {src_ch} 少于目标 {dst_ch}，缺失 {missing} 个槽位补零",
            source=source,
            fatal=False,
            detail={"missing": missing, "layout": spec.layout},
        )
        conversions.append({"type": "pad_zero_channels", "count": missing})
    elif src_ch > dst_ch:
        errors.add(
            ErrorCode.AUDIO_EXTRA_CHANNEL,
            f"{source}: 源声道 {src_ch} 多于目标 {dst_ch}，按 {extra_policy} 处理",
            source=source,
            fatal=False,
            detail={"extra": src_ch - dst_ch, "policy": extra_policy},
        )
        if extra_policy == "fold":
            matrix = _channel_map(src_ch, dst_ch, spec.layout)
            converted = samples @ matrix
            conversions.append({"type": "fold_extra_channels", "count": src_ch - dst_ch})
            return converted, conversions
        conversions.append({"type": "drop_extra_channels", "count": src_ch - dst_ch})
        return samples[:, :dst_ch].copy(), conversions

    matrix = _channel_map(src_ch, dst_ch, spec.layout)
    if not np.allclose(matrix, np.eye(dst_ch)):
        conversions.append({"type": "linear_channel_map", "src": src_ch, "dst": dst_ch})
    return samples @ matrix, conversions


def resample_linear(samples: np.ndarray, src_rate: int, dst_rate: int) -> np.ndarray:
    """NumPy 线性插值重采样；相同采样率原样返回。"""
    if src_rate == dst_rate:
        return samples.copy()
    n = samples.shape[0]
    if n == 0:
        return np.empty((0, samples.shape[1]), dtype=np.float64)
    duration = n / src_rate
    out_n = int(np.floor(duration * dst_rate))
    src_positions = np.arange(out_n, dtype=np.float64) * (src_rate / dst_rate)
    idx0 = np.floor(src_positions).astype(np.int64)
    idx1 = np.minimum(idx0 + 1, n - 1)
    frac = (src_positions - idx0)[:, None]
    return samples[idx0] * (1.0 - frac) + samples[idx1] * frac


def normalize_audio(
    samples: np.ndarray,
    src_rate: int,
    spec: AudioSpec,
    *,
    source: str,
    errors: ErrorBook,
    frame_size: int = 1024,
    tail_policy: str = "pad",
    extra_policy: str = "drop",
) -> NormalizeResult:
    """完整归一流程：重采样 -> 声道布局 -> 整帧尾部处理。"""
    if samples.ndim == 1:
        samples = samples[:, None]
    source_frames = samples.shape[0]
    conversions: list[dict] = []

    resampled = resample_linear(samples.astype(np.float64), src_rate, spec.sample_rate)
    if src_rate != spec.sample_rate:
        conversions.append(
            {"type": "resample", "from": src_rate, "to": spec.sample_rate}
        )

    laid_out, layout_ops = normalize_layout(
        resampled, spec, source=source, errors=errors, extra_policy=extra_policy
    )
    conversions.extend(layout_ops)

    remainder = laid_out.shape[0] % frame_size
    if remainder:
        missing = frame_size - remainder
        errors.add(
            ErrorCode.AUDIO_PARTIAL_TAIL,
            f"{source}: 尾部仅 {remainder} 样本、不足一帧 {frame_size}，按 {tail_policy} 处理",
            source=source,
            fatal=False,
            detail={"remainder": remainder, "missing": missing, "policy": tail_policy},
        )
        if tail_policy == "pad":
            padding = np.zeros((missing, laid_out.shape[1]), dtype=np.float64)
            laid_out = np.vstack([laid_out, padding])
            conversions.append({"type": "pad_tail", "samples": missing})
        else:
            laid_out = laid_out[:-remainder].copy()
            conversions.append({"type": "truncate_tail", "samples": remainder})

    return NormalizeResult(
        samples=laid_out,
        spec=spec,
        conversions=conversions,
        source_frames=source_frames,
        output_frames=laid_out.shape[0],
    )


def read_audio(path: str | Path, root: str | Path) -> tuple[np.ndarray, int]:
    """在工作目录内读取音频，返回 (样本数组, 采样率)。"""
    safe = resolve_within(root, path)
    data, rate = sf.read(str(safe), always_2d=True, dtype="float64")
    return data, rate


def write_audio(path: str | Path, root: str | Path, samples: np.ndarray, rate: int) -> Path:
    """在工作目录内写出音频（wav），原子替换由调用方或检查点层保证。"""
    safe = resolve_within(root, path)
    safe.parent.mkdir(parents=True, exist_ok=True)
    sf.write(str(safe), samples, rate, subtype="FLOAT")
    return safe
