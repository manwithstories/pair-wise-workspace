"""音频归一：采样率转换、声道布局映射、边界补齐与空音轨处理。

读取用 soundfile；重采样用 NumPy 线性插值（纯本地、无外部服务）。
只在清单声明的填充策略下处理非整齐样本边界，并报告样本数与同步偏移。
"""
from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import numpy as np
import soundfile as sf

from .model import (AudioInfo, ErrorCategory, Issue, PaddingPolicy,
                    TrackDecl, US_PER_SEC, atomic_write_bytes,
                    resolve_within_root)

# 已知布局 -> 标准声道顺序（soundfile 采用 WAV / SMPTE 顺序）。
KNOWN_LAYOUTS: dict[str, list[str]] = {
    "mono": ["FC"],
    "stereo": ["FL", "FR"],
    "3.0": ["FL", "FR", "FC"],
    "4.0": ["FL", "FR", "BL", "BR"],
    "5.1": ["FL", "FR", "FC", "LFE", "BL", "BR"],
}


@dataclass(slots=True)
class AudioResult:
    info: AudioInfo
    issues: list[Issue]
    samples: np.ndarray            # 归一后浮点 (frames, target_channels)
    sync_offset_us: int
    boundary_pad_us: int


def _layout_channels(layout: str | None, channels: int) -> list[str]:
    if layout and layout in KNOWN_LAYOUTS:
        names = KNOWN_LAYOUTS[layout]
        if len(names) != channels:
            raise ValueError(f"布局 {layout} 需要 {len(names)} 声道，"
                             f"实际 {channels}")
        return list(names)
    # 未知/缺失布局：按声道数生成中性标签，不臆测物理位置。
    return [f"CH{i}" for i in range(channels)]


def _resample_linear(x: np.ndarray, src_rate: int,
                     dst_rate: int) -> np.ndarray:
    """按帧维度线性插值重采样，形状 (frames, channels)。"""
    if src_rate == dst_rate or len(x) == 0:
        return x.copy()
    duration = len(x) / src_rate
    dst_len = int(round(duration * dst_rate))
    if dst_len <= 0:
        return x[:0].copy()
    src_idx = np.arange(dst_len, dtype=np.float64) * src_rate / dst_rate
    i0 = np.floor(src_idx).astype(np.int64)
    i1 = np.minimum(i0 + 1, len(x) - 1)
    frac = (src_idx - i0)[:, None]
    return x[i0] * (1.0 - frac) + x[i1] * frac


def _map_channels(x: np.ndarray, src_names: list[str],
                  dst_names: list[str]) -> tuple[np.ndarray, list[int]]:
    """声道映射。

    - 同名声道直通；
    - 立体声 -> 单声道按标准下混 0.5*(FL+FR)；
    - 单声道 -> 立体声向 FL/FR 复制（上混）；
    - 目标缺失声道填静音并以 -1 标记。channel_map 记录逐目标来源：
      非负为源索引，-1 为静音，-2 为下混/上混生成。
    """
    out = np.zeros((len(x), len(dst_names)), dtype=np.float32)
    channel_map: list[int] = []

    for j, name in enumerate(dst_names):
        if name in src_names:
            i = src_names.index(name)
            out[:, j] = x[:, i]
            channel_map.append(i)
        elif name == "FC" and {"FL", "FR"} <= set(src_names):
            il, ir = src_names.index("FL"), src_names.index("FR")
            out[:, j] = 0.5 * (x[:, il] + x[:, ir])
            channel_map.append(-2)
        elif name in ("FL", "FR") and src_names == ["FC"]:
            out[:, j] = x[:, 0]
            channel_map.append(-2)
        else:
            channel_map.append(-1)     # 源中无对应声道 -> 静音
    return out, channel_map


def process_audio(track: TrackDecl, root: str,
                  reference_duration_us: int | None = None) -> AudioResult:
    path = resolve_within_root(root, track.path)
    issues: list[Issue] = []

    data, sr = sf.read(str(path), dtype="float32", always_2d=True)
    src_channels = data.shape[1]
    if sr != track.sample_rate or src_channels != track.channels:
        raise ValueError(
            f"音频 {track.track_id} 文件参数 {sr}Hz/{src_channels}ch 与清单 "
            f"{track.sample_rate}Hz/{track.channels}ch 不一致")

    src_names = _layout_channels(track.channel_layout, src_channels)
    duration_us = round(len(data) * US_PER_SEC / sr) if sr else 0

    if len(data) == 0:
        issues.append(Issue(
            ErrorCategory.EMPTY_AUDIO, track.track_id,
            "空音轨：未读取到任何样本，按目标布局输出静音骨架",
            recovered=True))

    target_rate = track.target_sample_rate or sr
    target_layout = track.target_layout or track.channel_layout or \
        ("mono" if src_channels == 1 else f"{src_channels}ch-generic")
    dst_names = _layout_channels(
        target_layout if target_layout in KNOWN_LAYOUTS else None,
        _generic_count(target_layout, src_channels))

    # 1) 重采样
    rs = _resample_linear(data, sr, target_rate)
    # 2) 声道映射
    mapped, channel_map = _map_channels(rs, src_names, dst_names)

    # 3) 边界补齐：对齐到参考时间线（通常为机位视频时长）。
    # 空音轨保留零样本骨架，不按参考时长补齐，避免伪造内容与虚高样本差。
    padded = 0
    target_frames = (round(reference_duration_us * target_rate / US_PER_SEC)
                     if reference_duration_us is not None and len(data) > 0
                     else None)
    if target_frames is not None:
        if len(mapped) < target_frames:
            short = target_frames - len(mapped)
            if track.padding_policy is PaddingPolicy.REJECT:
                from .model import PipelineError
                raise PipelineError(
                    ErrorCategory.MANIFEST_BAD_VALUE,
                    f"音频 {track.track_id} 缺 {short} 样本且策略为 reject",
                    track=track.track_id)
            if track.padding_policy is PaddingPolicy.SILENCE:
                mapped = np.pad(mapped, ((0, short), (0, 0)))
                padded = short
            # TRUNCATE：不补齐，保留较短长度
        elif len(mapped) > target_frames and \
                track.padding_policy is PaddingPolicy.TRUNCATE:
            mapped = mapped[:target_frames]

    boundary_pad_us = round(padded * US_PER_SEC / target_rate) if target_rate else 0
    info = AudioInfo(
        sample_rate=sr, channels=src_channels,
        layout=track.channel_layout or "generic",
        frames=len(data), duration_us=duration_us,
        target_rate=target_rate, target_channels=len(dst_names),
        target_layout=target_layout, output_frames=len(mapped),
        padded_samples=padded, channel_map=channel_map)
    return AudioResult(info=info, issues=issues, samples=mapped,
                       sync_offset_us=track.sync_offset_us,
                       boundary_pad_us=boundary_pad_us)


def _generic_count(layout: str | None, fallback: int) -> int:
    if not layout:
        return fallback
    if layout in KNOWN_LAYOUTS:
        return len(KNOWN_LAYOUTS[layout])
    if layout.endswith("ch-generic"):
        try:
            return int(layout.split("ch", 1)[0])
        except ValueError:
            return fallback
    return fallback


def write_normalized(result: AudioResult, out_path: str,
                     subtype: str = "PCM_16") -> None:
    """原子写出归一 WAV（先临时文件后替换）。"""
    import os
    import tempfile
    out = Path(out_path)
    out.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".fd-audio-", suffix=".wav",
                               dir=out.parent)
    os.close(fd)
    try:
        sf.write(tmp, result.samples, result.info.target_rate,
                 subtype=subtype)
        os.replace(tmp, out)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise
