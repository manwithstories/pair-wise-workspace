"""演示工作区物料生成：把编码原语组装成流水线可消费的完整工作区。

布局（全部位于指定工作目录内）：
  fixtures/rep<i>.<codec>      干净 Annex B 裸码流
  fixtures/rep<i>.index.json   关键帧索引
  fixtures/rep<i>.color.json   色彩元数据（含可选冲突）
  fixtures/audio.wav           源音频（24kHz 立体声）
  fixtures/diagnostic.corrupt.<codec>  损坏诊断变体（不参与主管线）
"""

from __future__ import annotations

import dataclasses
from pathlib import Path

import numpy as np

from .audio import write_audio
from .bitstream import Codec, scan_bitstream
from .digest import write_json
from .errors import ErrorBook, ErrorCode
from .fixture import (
    RepresentationSpec,
    build_keyframe_index,
    encode_video,
    inject_corrupt_header,
)

CODEC_CYCLE = (Codec.H264, Codec.H265, Codec.AV1)
# 第三个表示层刻意给出冲突色彩，验证冲突保留与裁决留痕。
COLOR_VARIANTS = (
    {"range": "limited", "matrix": "bt709", "transfer": "bt709", "primaries": "bt709"},
    {"range": "limited", "matrix": "bt709", "transfer": "bt709", "primaries": "bt709"},
    {"range": "full", "matrix": "bt2020nc", "transfer": "hlg", "primaries": "bt2020"},
)


@dataclasses.dataclass
class WorkspaceMaterial:
    corrupt_offset_bytes: int | None
    detected_offset_bytes: int | None
    representations: int


def generate_workspace(
    workdir: str | Path,
    representations: int = 3,
    *,
    frames: int = 30,
    fps: int = 30,
    gop: int = 15,
    corrupt_every: int = 0,
) -> WorkspaceMaterial:
    """生成完整工作区，返回损坏注入点与扫描器定位点（误差应 <=1 字节）。"""
    root = Path(workdir)
    (root / "fixtures").mkdir(parents=True, exist_ok=True)
    corrupt_offset: int | None = None
    detected_offset: int | None = None

    for i in range(representations):
        codec = CODEC_CYCLE[i % len(CODEC_CYCLE)]
        spec = RepresentationSpec(
            name=f"rep{i}", codec=codec, frames=frames, fps=fps, gop=gop
        )
        data = encode_video(spec)
        entries = build_keyframe_index(data, codec, spec.timescale, fps, gop)
        (root / "fixtures" / f"rep{i}.{codec.value}").write_bytes(data)
        write_json(
            f"fixtures/rep{i}.index.json",
            root,
            [dataclasses.asdict(e) for e in entries],
        )
        write_json(
            f"fixtures/rep{i}.color.json",
            root,
            COLOR_VARIANTS[i % len(COLOR_VARIANTS)],
        )
        if corrupt_every and i == 0:
            mutated, corrupt_offset = inject_corrupt_header(data, corrupt_every)
            diag_rel = f"fixtures/diagnostic.corrupt.{codec.value}"
            (root / diag_rel).write_bytes(mutated)
            detected_offset = _locate_corruption(mutated, codec, diag_rel)

    # 固定随机种子保证重复生成字节一致 -> 摘要可重复。
    audio = np.random.RandomState(20261121).randn(24000, 2) * 0.05
    write_audio("fixtures/audio.wav", root, audio, 24000)
    return WorkspaceMaterial(
        corrupt_offset_bytes=corrupt_offset,
        detected_offset_bytes=detected_offset,
        representations=representations,
    )


def _locate_corruption(data: bytes, codec: Codec, source: str) -> int | None:
    """用真实扫描器定位损坏变体，返回首个致命损坏的字节偏移。"""
    book = ErrorBook()
    scan_bitstream(data, codec, source=source, errors=book)
    fatal = book.by_code(ErrorCode.BITSTREAM_CORRUPT)
    return fatal[0].offset if fatal else None
