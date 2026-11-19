"""切片内容感知指纹与去重（码流域，不依赖切片可独立解码）。

AV1/H.265 切片脱离完整流后缺少参数集与参考帧，无法保证独立解码，
因此指纹直接基于码流结构特征，三种编码通用且对截断鲁棒：
- visual(64 位)：分块字节均值/方差比较（32 位）+ NAL/OBU 类型直方图（16 位）
  + 载荷长度分桶（8 位）+ 字节熵区间（8 位）；
- temporal(32 位)：单元载荷长度沿时间的差分符号序列。
完全重复：双指纹全等；近似重复：visual 汉明距离 <= 配置门限。
代表片段保留首次出现者，重复项记录来源位置与距离。
"""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .bitstream import CODECS, scan_bytes
from .timeline import ErrorKind, MediaError, Stage

_HASH_BITS = 64
_TEMPORAL_BITS = 32
_DEFAULT_HAMMING = 8
_TYPE_BINS = 16
_BLOCK_BITS = 32


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


def _block_bits(payload: bytes) -> list[bool]:
    """等距分块，块均值与全局均值比较 + 块方差比较，共 _BLOCK_BITS 位。"""

    if not payload:
        return [False] * _BLOCK_BITS
    arr = np.frombuffer(payload, dtype=np.uint8).astype(np.float32)
    half = _BLOCK_BITS // 2
    blocks = np.array_split(arr, half)
    means = np.array([b.mean() for b in blocks])
    stds = np.array([b.std() for b in blocks])
    mean_flags = means > arr.mean()
    std_flags = stds > (stds.mean() if stds.size else 0.0)
    return [bool(x) for x in np.concatenate([mean_flags, std_flags])]


def _type_bits(unit_types: list[int]) -> list[bool]:
    counts = [0] * _TYPE_BINS
    for t in unit_types:
        counts[t % _TYPE_BINS] += 1
    median = float(np.median(counts)) if counts else 0.0
    return [c > median for c in counts]


def _length_bins(sizes: list[int]) -> list[bool]:
    if not sizes:
        return [False] * 8
    arr = np.array(sizes, dtype=np.float64)
    quantiles = np.quantile(arr, np.linspace(0, 1, 9))
    total = float(arr.sum())
    # 8 位：各长度量级（按 2 的幂分桶）占比是否超过均分
    flags = []
    for shift in range(8):
        share = arr[(arr >= 2 ** (shift + 4)) &
                    (arr < 2 ** (shift + 5))].sum() / (total + 1e-9)
        flags.append(share > (1.0 / 8))
    return flags


def _entropy_bits(payload: bytes) -> list[bool]:
    if not payload:
        return [False] * 8
    counts = np.bincount(np.frombuffer(payload, dtype=np.uint8),
                         minlength=256).astype(np.float64)
    probs = counts[counts > 0] / counts.sum()
    entropy = float(-(probs * np.log2(probs)).sum())  # 0..8
    # 8 位温度计编码，落在哪个熵区间
    level = min(8, int(entropy))
    return [i < level for i in range(8)]


def compute_fingerprint(
    codec: str,
    payload: bytes,
    segment_index: int,
    source_offset: int,
    end_offset: int,
) -> Fingerprint:
    if codec not in CODECS:
        raise ValueError(f"不支持的编码标识: {codec!r}")
    # 复用扫描器解析单元结构；截断载荷也能产出部分单元特征
    scan = scan_bytes(payload, codec, stream_truncated=False)
    unit_types = [u.unit_type for u in scan.units if u.unit_type >= 0]
    sizes = [u.size for u in scan.units]

    bits = (_block_bits(payload) + _type_bits(unit_types)
            + _length_bins(sizes) + _entropy_bits(payload))
    visual = 0
    for flag in bits[:_HASH_BITS]:
        visual = (visual << 1) | int(flag)

    temporal = _temporal_bits(sizes)
    return Fingerprint(segment_index, visual, temporal, len(scan.units),
                       source_offset, end_offset)


def _temporal_bits(sizes: list[int]) -> int:
    if len(sizes) < 2:
        return 0
    diffs = np.diff(np.asarray(sizes, dtype=np.float64))
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
