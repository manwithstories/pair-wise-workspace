"""素材清单导入：把 ASSET 块投影为统一模型 :class:`Asset`。

与 :mod:`src.import_decision` 共用 :mod:`src.format_parser`，
本模块只负责 ASSET/ALIAS 的语义投影，不包含任何独立语法。
"""

from __future__ import annotations

from pathlib import Path
from typing import Dict, List, Optional

from .errors import FormatError
from .format_parser import Block, child_blocks, parse_blocks, parse_file, parse_int, require_field
from .model import Alias, Asset, ExchangeDoc, TimeBase


def _build_asset(block: Block, source: str) -> Asset:
    asset_id = block.ident
    if not asset_id:
        raise FormatError("ASSET 块缺少素材 ID", block.line_start, source)
    rel_path = require_field(block, "path", source)
    size = parse_int(require_field(block, "size", source), block.line_start, "size", source)
    fingerprint = require_field(block, "fingerprint", source)
    fp_offset = parse_int(block.fields.get("fp_offset", "0"), block.line_start, "fp_offset", source)
    fp_length: Optional[int] = None
    if "fp_length" in block.fields:
        fp_length = parse_int(block.fields["fp_length"], block.line_start, "fp_length", source)
    duration = None
    if "duration" in block.fields:
        duration = parse_int(block.fields["duration"], block.line_start, "duration", source)
    tb = TimeBase.parse(block.fields["timebase"]) if "timebase" in block.fields else None

    aliases: List[Alias] = []
    for ab in child_blocks(block, "ALIAS"):
        name = require_field(ab, "name", source)
        target = ab.fields.get("target", asset_id)
        aliases.append(Alias(alias=name, target_id=target))

    consumed = {"path", "size", "fingerprint", "fp_offset", "fp_length",
                "duration", "timebase"}
    extra = {k: v for k, v in block.fields.items() if k not in consumed}
    return Asset(
        asset_id=asset_id, rel_path=rel_path, size=size, fingerprint=fingerprint,
        fp_offset=fp_offset, fp_length=fp_length, duration_frames=duration,
        timebase=tb, aliases=tuple(aliases), extra=extra,
    )


def build_doc(root: Block, source: str = "") -> ExchangeDoc:
    if root.kind != "EXCHANGE":
        raise FormatError("根块必须是 EXCHANGE", root.line_start, source)
    version = require_field(root, "version", source)
    timebase = TimeBase.parse(require_field(root, "timebase", source))

    assets: List[Asset] = []
    seen: Dict[str, int] = {}
    for ab in child_blocks(root, "ASSET"):
        asset = _build_asset(ab, source)
        if asset.asset_id in seen:
            raise FormatError(
                f"素材 ID {asset.asset_id} 重复定义", ab.line_start, source)
        seen[asset.asset_id] = ab.line_start
        assets.append(asset)

    consumed = {"version", "timebase"}
    extra = {k: v for k, v in root.fields.items() if k not in consumed}
    return ExchangeDoc(version=version, timebase=timebase,
                       assets=tuple(assets), sequences=(), extra=extra)


def import_manifest_text(text: str, source: str = "") -> ExchangeDoc:
    return build_doc(parse_blocks(iter(text.splitlines()), source), source)


def import_manifest(path: Path) -> ExchangeDoc:
    """支持大清单：parse_file 逐行流式读取。"""
    return build_doc(parse_file(path), str(path))
