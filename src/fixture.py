"""确定性 fixture 生成器：只写仓库内目录，无外部素材、无网络。

指标设计（与验收期望严格对齐）：
- 12 个叶子条目、3 层嵌套（S0 -> S1 -> S2）
- 素材 A01..A12：A11/A12 缺失、A10 指纹歧义（两份相同内容文件）、其余 9 个可定位
- A03 通过别名链 cam_alias -> roll_cam -> A03 定位
- 多段变速漂移：A02=1 帧、A04=2 帧、A05=0 帧；总漂移 3、最大 2
- V2 轨道制造一对重叠；A09 声明 25fps 制造时间基不一致
"""

from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Tuple

from .model import FP_CHUNK

FIXTURE_ASSETS = [f"A{idx:02d}" for idx in range(1, 13)]
MISSING_IDS = {"A11", "A12"}
AMBIGUOUS_ID = "A10"


def _content_bytes(asset_id: str, size: int) -> bytes:
    """以资产 ID 为种子的确定性字节，保证重复生成逐字节一致。"""
    seed = int(hashlib.sha256(asset_id.encode("utf-8")).hexdigest(), 16)
    state = seed & 0xFFFFFFFF
    out = bytearray()
    while len(out) < size:
        state = (state * 1103515245 + 12345) & 0x7FFFFFFF
        out.append(state & 0xFF)
    return bytes(out[:size])


def _fingerprint(data: bytes) -> Tuple[str, int]:
    return hashlib.sha256(data[:FP_CHUNK]).hexdigest(), len(data)


def _write_media(root: Path) -> dict:
    info = {}
    for asset_id in FIXTURE_ASSETS:
        if asset_id in MISSING_IDS:
            continue
        size = 2048 + (int(asset_id[1:]) * 137)
        data = _content_bytes(asset_id, size)
        fp, actual_size = _fingerprint(data)
        if asset_id == AMBIGUOUS_ID:
            # 同内容、同大小、同指纹的两个路径 -> 歧义，拒绝静默选择
            for rel in ("dupes/a10_alpha.mov", "dupes/a10_beta.mov"):
                p = root / rel
                p.parent.mkdir(parents=True, exist_ok=True)
                p.write_bytes(data)
            info[asset_id] = ("dupes/a10_alpha.mov", fp, actual_size)
        else:
            rel = f"clips/{asset_id.lower()}.mov"
            p = root / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(data)
            info[asset_id] = (rel, fp, actual_size)
    return info


def build_manifest_text(info: dict) -> str:
    lines = ["EXCHANGE version=1 timebase=24/1 generator=conform-fixture"]
    for asset_id in FIXTURE_ASSETS:
        if asset_id in MISSING_IDS:
            # 声明但媒体根目录中不存在 -> 无法定位
            lines.append(f"  ASSET {asset_id} path=lost/{asset_id.lower()}.mov "
                         f"size=1024 fingerprint={'0' * 64} duration=500")
            lines.append("  END")
            continue
        rel, fp, size = info[asset_id]
        tb = " timebase=25/1" if asset_id == "A09" else ""
        lines.append(f"  ASSET {asset_id} path={rel} size={size} "
                     f"fingerprint={fp} duration=500{tb}")
        if asset_id == "A03":
            lines.append("    ALIAS name=cam_alias target=roll_cam")
            lines.append("    ALIAS name=roll_cam target=A03")
        lines.append("  END")
    lines.append("END")
    return "\n".join(lines) + "\n"


def build_decision_text() -> str:
    lines = ["EXCHANGE version=1 timebase=24/1 generator=conform-fixture"]
    lines.append("  SEQUENCE S0 timebase=24/1")
    lines.append("    TRACK V1")
    lines.append("      CLIP c0a seq=S1 dst_in=0 dst_out=240 src_in=0 src_out=240")
    lines.append("      END")
    lines.append("      CLIP c0b seq=S3 dst_in=240 dst_out=320 src_in=0 src_out=80")
    lines.append("      END")
    lines.append("    END")
    lines.append("  END")

    lines.append("  SEQUENCE S1 timebase=24/1")
    lines.append("    TRACK V1")
    lines.append("      CLIP c1a seq=S2 dst_in=0 dst_out=150 src_in=0 src_out=150")
    lines.append("      END")
    lines.append("      CLIP c1b asset=A02 dst_in=150 dst_out=200 src_in=10 src_out=60")
    lines.append("        SPEED dst_in=0 dst_out=15 rate=3/2")
    lines.append("        SPEED dst_in=15 dst_out=31 rate=3/2")
    lines.append("        SPEED dst_in=31 dst_out=50 rate=3/2")
    lines.append("      END")
    lines.append("    END")
    lines.append("  END")

    lines.append("  SEQUENCE S2 timebase=24/1")
    lines.append("    TRACK V1")
    lines.append("      CLIP c2a asset=A01 dst_in=0 dst_out=40 src_in=0 src_out=40")
    lines.append("      END")
    lines.append("      CLIP c2b asset=A04 dst_in=40 dst_out=100 src_in=5 src_out=65")
    lines.append("        SPEED dst_in=0 dst_out=17 rate=3/2")
    lines.append("        SPEED dst_in=17 dst_out=34 rate=3/2")
    lines.append("        SPEED dst_in=34 dst_out=51 rate=3/2")
    lines.append("        SPEED dst_in=51 dst_out=68 rate=3/2")
    lines.append("        SPEED dst_in=68 dst_out=100 rate=3/2")
    lines.append("      END")
    lines.append("      CLIP c2c asset=A05 dst_in=100 dst_out=150 src_in=0 src_out=50")
    lines.append("        SPEED dst_in=0 dst_out=15 rate=3/2")
    lines.append("        SPEED dst_in=15 dst_out=30 rate=3/2")
    lines.append("        SPEED dst_in=30 dst_out=50 rate=3/2")
    lines.append("      END")
    lines.append("    END")
    lines.append("  END")

    lines.append("  SEQUENCE S3 timebase=24/1")
    lines.append("    TRACK V1")
    lines.append("      CLIP c3a asset=A06 speed=-1/1 dst_in=0 dst_out=30 "
                 "src_in=80 src_out=50")
    lines.append("      END")
    lines.append("      CLIP c3b asset=A07 dst_in=30 dst_out=55 src_in=0 src_out=25")
    lines.append("      END")
    lines.append("    END")
    lines.append("    TRACK V2")
    lines.append("      CLIP c3c asset=A08 dst_in=0 dst_out=20 src_in=0 src_out=20")
    lines.append("      END")
    lines.append("      CLIP c3d asset=A09 dst_in=10 dst_out=40 src_in=0 src_out=30")
    lines.append("      END")
    lines.append("      CLIP c3e asset=A03 dst_in=40 dst_out=60 src_in=0 src_out=20")
    lines.append("      END")
    lines.append("      CLIP c3f asset=A10 dst_in=60 dst_out=70 src_in=0 src_out=10")
    lines.append("      END")
    lines.append("      CLIP c3g asset=A11 dst_in=70 dst_out=76 src_in=0 src_out=6")
    lines.append("      END")
    lines.append("      CLIP c3h asset=A12 dst_in=76 dst_out=80 src_in=0 src_out=4")
    lines.append("      END")
    lines.append("    END")
    lines.append("  END")
    lines.append("END")
    return "\n".join(lines) + "\n"


def generate_fixture(base_dir: Path) -> Tuple[Path, Path, Path]:
    base_dir.mkdir(parents=True, exist_ok=True)
    media_root = base_dir / "media"
    media_root.mkdir(exist_ok=True)
    info = _write_media(media_root)

    decision = base_dir / "decision.txt"
    manifest = base_dir / "manifest.txt"
    decision.write_text(build_decision_text(), encoding="utf-8", newline="\n")
    manifest.write_text(build_manifest_text(info), encoding="utf-8", newline="\n")
    return decision, manifest, media_root
