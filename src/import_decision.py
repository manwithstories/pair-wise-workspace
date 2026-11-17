"""决策表导入：把 SEQUENCE/TRACK/CLIP/SPEED 块投影为统一模型。

与 :mod:`src.import_manifest` 共用 :mod:`src.format_parser`，
本模块只负责序列侧的语义投影与时间码换算，不包含任何独立语法。
"""

from __future__ import annotations

from pathlib import Path
from fractions import Fraction
from typing import Dict, Optional, Tuple

from .errors import FormatError
from .format_parser import (
    Block, child_blocks, parse_blocks, parse_file, parse_fraction,
    parse_timecode, require_field,
)
from .model import Clip, ExchangeDoc, Sequence, SpeedSegment, TimeBase, Track


def _build_speed(block: Block, tb: TimeBase, source: str) -> SpeedSegment:
    dst_in = parse_timecode(require_field(block, "dst_in", source), tb, block.line_start, source)
    dst_out = parse_timecode(require_field(block, "dst_out", source), tb, block.line_start, source)
    rate = parse_fraction(require_field(block, "rate", source), block.line_start, "rate", source)
    if dst_out <= dst_in:
        raise FormatError("SPEED 区间必须 dst_out > dst_in", block.line_start, source)
    return SpeedSegment(dst_in=dst_in, dst_out=dst_out, rate=rate)


def _build_clip(block: Block, track_id: str, tb: TimeBase,
                source: str, seq_seen: Dict[str, int]) -> Clip:
    clip_id = block.ident
    if not clip_id:
        raise FormatError("CLIP 块缺少片段 ID", block.line_start, source)
    asset_id = block.fields.get("asset")
    seq_ref = block.fields.get("seq")
    if bool(asset_id) == bool(seq_ref):
        raise FormatError(
            f"CLIP {clip_id} 必须且只能指定 asset 或 seq 之一", block.line_start, source)
    dst_in = parse_timecode(require_field(block, "dst_in", source), tb, block.line_start, source)
    dst_out = parse_timecode(require_field(block, "dst_out", source), tb, block.line_start, source)
    if dst_out <= dst_in:
        raise FormatError(f"CLIP {clip_id} 必须 dst_out > dst_in", block.line_start, source)
    src_in = parse_timecode(require_field(block, "src_in", source), tb, block.line_start, source)
    src_out = parse_timecode(require_field(block, "src_out", source), tb, block.line_start, source)

    speed_blocks = child_blocks(block, "SPEED")
    segments = tuple(_build_speed(sb, tb, source) for sb in speed_blocks)
    speed: Optional[Fraction] = None
    if "speed" in block.fields:
        speed = parse_fraction(block.fields["speed"], block.line_start, "speed", source)
        if speed == 0:
            # 零速属业务错误，但导入阶段先保留原值，由时间线展开统一报错并继续。
            pass
    clip_tb = TimeBase.parse(block.fields["timebase"]) if "timebase" in block.fields else tb

    consumed = {"asset", "seq", "dst_in", "dst_out", "src_in", "src_out",
                "speed", "timebase"}
    extra = {k: v for k, v in block.fields.items() if k not in consumed}
    return Clip(
        clip_id=clip_id, dst_in=dst_in, dst_out=dst_out, src_in=src_in,
        src_out=src_out, track_id=track_id, asset_id=asset_id, seq_ref=seq_ref,
        timebase=clip_tb, speed=speed, speed_segments=segments, extra=extra,
    )


def _build_track(block: Block, tb: TimeBase, source: str,
                 seq_seen: Dict[str, int]) -> Track:
    track_id = block.ident or require_field(block, "id", source)
    kind = block.fields.get("kind", "video")
    clips = tuple(
        _build_clip(cb, track_id, tb, source, seq_seen)
        for cb in child_blocks(block, "CLIP")
    )
    extra = {k: v for k, v in block.fields.items() if k not in {"id", "kind"}}
    return Track(track_id=track_id, kind=kind, clips=clips, extra=extra)


def _build_sequence(block: Block, source: str,
                    seq_seen: Dict[str, int]) -> Sequence:
    seq_id = block.ident
    if not seq_id:
        raise FormatError("SEQUENCE 块缺少序列 ID", block.line_start, source)
    if seq_id in seq_seen:
        raise FormatError(f"序列 ID {seq_id} 重复定义", block.line_start, source)
    seq_seen[seq_id] = block.line_start
    tb = TimeBase.parse(require_field(block, "timebase", source))
    tracks = tuple(
        _build_track(tb_block, tb, source, seq_seen)
        for tb_block in child_blocks(block, "TRACK")
    )
    consumed = {"timebase"}
    extra = {k: v for k, v in block.fields.items() if k not in consumed}
    return Sequence(sequence_id=seq_id, timebase=tb, tracks=tracks, extra=extra)


def build_doc(root: Block, source: str = "") -> ExchangeDoc:
    if root.kind != "EXCHANGE":
        raise FormatError("根块必须是 EXCHANGE", root.line_start, source)
    version = require_field(root, "version", source)
    timebase = TimeBase.parse(require_field(root, "timebase", source))
    seq_seen: Dict[str, int] = {}
    sequences = tuple(
        _build_sequence(sb, source, seq_seen)
        for sb in child_blocks(root, "SEQUENCE")
    )
    consumed = {"version", "timebase"}
    extra = {k: v for k, v in root.fields.items() if k not in consumed}
    return ExchangeDoc(version=version, timebase=timebase,
                       assets=(), sequences=sequences, extra=extra)


def import_decision_text(text: str, source: str = "") -> ExchangeDoc:
    return build_doc(parse_blocks(iter(text.splitlines()), source), source)


def import_decision(path: Path) -> ExchangeDoc:
    return build_doc(parse_file(path), str(path))
