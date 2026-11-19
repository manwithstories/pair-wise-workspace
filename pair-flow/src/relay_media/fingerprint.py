"""切片内容感知指纹与去重。

指纹分两部分，均为确定性整数位串，可 JSON 序列化：
- visual: 每帧下采样到 8x8 灰度，按 4 个 4x4 象限与全局均值比较，
  多帧逐位多数投票，得到 64 位稳定视觉哈希；
- temporal: 帧间能量差分符号序列，32 位时间结构指纹。
完全重复：双指纹全等；近似重复：视觉哈希汉明距离 <= 配置门限。
代表片段保留首次出现者，其余记录来源位置与距离。
"""

from __future__ import annotations

import io
from dataclasses import dataclass, field

import av
import numpy as np

from .bitstream import CODECS
from .timeline import ErrorKind, MediaError, Stage

_HASH_BITS = 64
_TEMPORAL_BITS = 32
_DEFAULT_HAMMING = 6


@dataclass(slots=True)
class Fingerprint:
    segment_index: int
    visual: int
    temporal: int
    frame_count: int
    source_offset: int
    end_offset: int

    def to_dict(self) -> dict:
        return {
            "segment_index": self.segment_index,
            "visual": self.visual,
            "temporal": self.temporal,
            "frame_count": self.frame_count,
            "source_offset": self.source_offset,
            "end_offset": self.end_offset,
        }


@dataclass(slots=True)
class Duplicate:
    segment_index: int
    representative: int
    exact: bool
    hamming_distance: int
    source_offset: int

    def to_dict(self) -> dict:
        return {
            "segment_index": self.segment_index,
            "representative": self.representative,
            "exact": self.exact,
            "hamming_distance": self.hamming_distance,
            "source_offset": self.source_offset,
        }


@dataclass(slots=True)
class DedupResult:
    fingerprints: list[Fingerprint]
    duplicates: list[Duplicate]
    representatives: list[int]
    errors: list[MediaError] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "fingerprints": [f.to_dict() for f in self.fingerprints],
            "duplicates": [d.to_dict() for d in self.duplicates],
            "representatives": self.representatives,
            "errors": [e.to_dict() for e in self.errors],
        }


def hamming_distance(a: int, b: int) -> int:
    return (a ^ b).bit_count()


def _frame_bits(gray: np.ndarray) -> np.ndarray:
    """8x8 灰度 -> 64 位比较结果（四象限均值 vs 全局均值）。"""

    small = gray.reshape(8, gray.shape[0] // 8, 8, gray.shape[1] // 8).mean(
        axis=(1, 3))
    global_mean = small.mean()
    return (small > global_mean).reshape(-1)


def compute_fingerprint(
    codec: str,
    payload: bytes,
    segment_index: int,
    source_offset: int,
    end_offset: int,
) -> Fingerprint:
    if codec not in CODECS:
        raise ValueError(f"不支持的编码标识: {codec!r}")
    fmt = "av1" if codec == "av1" else codec
    container = av.open(io.BytesIO(payload), mode="r", format=fmt)
    bit_rows: list[np.ndarray] = []
    energies: list[float] = []
    try:
        for frame in container.decode(video=0):
            gray = frame.to_ndarray(format="gray").astype(np.float32)
            # 裁剪到 8 的整数倍，保证 reshape 确定
            h, w = (gray.shape[0] // 8) * 8, (gray.shape[1] // 8) * 8
            bit_rows.append(_frame_bits(gray[:h, :w]))
            energies.append(float((gray[:h, :w] ** 2).mean()))
    finally:
        container.close()

    if not bit_rows:
        visual = 0
        temporal = 0
    else:
        stacked = np.stack(bit_rows)
        votes = stacked.mean(axis=0) >= 0.5           # 逐位多数投票
        visual = 0
        for flag in votes:
            visual = (visual << 1) | int(flag)
        temporal = _temporal_bits(energies)
    return Fingerprint(segment_index, visual, temporal, len(bit_rows),
                       source_offset, end_offset)


def _temporal_bits(energies: list[float]) -> int:
    if len(energies) < 2:
        return 0
    diffs = np.diff(np.asarray(energies, dtype=np.float64))
    # 等距采样到 _TEMPORAL_BITS 个符号位，长度无关
    idx = np.linspace(0, len(diffs) - 1,
                      num=min(_TEMPORAL_BITS, len(diffs))).round().astype(int)
    signs = (diffs[idx] >= 0).astype(np.uint8)
    value = 0
    for bit in signs:
        value = (value << 1) | int(bit)
    return value


def deduplicate(
    fingerprints: list[Fingerprint],
    *,
    hamming_threshold: int = _DEFAULT_HAMMING,
) -> DedupResult:
    """首次出现为代表；完全重复要求双指纹全等，近似重复看视觉汉明距离。"""

    errors: list[MediaError] = []
    reps: list[int] = []
    dups: list[Duplicate] = []
    for fp in fingerprints:
        match: Fingerprint | None = None
        best_dist = _HASH_BITS + 1
        for rep_idx in reps:
            ref = fingerprints[rep_idx]
            dist = hamming_distance(fp.visual, ref.visual)
            if dist < best_dist:
                best_dist, match = dist, ref
        if match is not None and best_dist <= hamming_threshold:
            exact = (fp.visual == match.visual
                     and fp.temporal == match.temporal)
            dups.append(Duplicate(
                fp.segment_index, match.segment_index, exact, best_dist,
                fp.source_offset))
            if not exact:
                errors.append(MediaError(
                    Stage.FINGERPRINT, ErrorKind.CHECKSUM_MISMATCH,
                    fp.source_offset,
                    f"近似重复：与片段 {match.segment_index} 汉明距离 "
                    f"{best_dist}（门限 {hamming_threshold}）",
                    recovered=True,
                    context={"hamming_distance": best_dist,
                             "representative": match.segment_index}))
        else:
            reps.append(fp.segment_index)
    return DedupResult(fingerprints, dups, reps, errors)
