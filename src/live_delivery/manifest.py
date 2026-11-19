"""HLS / MPEG-DASH 清单解析，建立统一媒体时间基准（秒）。

仅支持本地清单及其相对路径分片；拒绝非法时长、越界时间与外部 URI。
"""
from __future__ import annotations

import math
import re
import xml.etree.ElementTree as ET
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import urlparse

from .errors import ManifestError


@dataclass(frozen=True)
class SegmentRef:
    """清单中的一个分片引用（清单时间基准，单位秒）。"""

    sequence: int
    uri: str
    duration: float  # 声明时长（秒）
    start: float     # 清单媒体时间起点（秒）

    @property
    def end(self) -> float:
        return self.start + self.duration


@dataclass
class Manifest:
    kind: str  # "hls" | "dash"
    path: Path
    base_dir: Path
    target_duration: float
    segments: list[SegmentRef]

    @property
    def total_duration(self) -> float:
        """清单声明总时长：同一序号只计首次出现（重投/冲突条目不重复累计）。"""
        seen: set[int] = set()
        total = 0.0
        for s in self.segments:
            if s.sequence in seen:
                continue
            seen.add(s.sequence)
            total = max(total, s.end)
        return total


def _check_uri(uri: str) -> None:
    """分片引用必须是可解析的相对路径，禁止外部 URI 与越界路径。"""
    if not uri or not uri.strip():
        raise ManifestError("空的分片 URI")
    parsed = urlparse(uri)
    if parsed.scheme or parsed.netloc:
        raise ManifestError(f"拒绝外部 URI: {uri}")
    pure = Path(uri)
    if pure.is_absolute() or ".." in pure.parts:
        raise ManifestError(f"分片引用越界: {uri}")


def _check_duration(value: float, ctx: str) -> float:
    if not math.isfinite(value) or value <= 0:
        raise ManifestError(f"非法时长 {value!r}（{ctx}）")
    return value


def load_manifest(path: str | Path) -> Manifest:
    p = Path(path)
    if not p.is_file():
        raise ManifestError(f"清单不存在: {p}")
    text = p.read_text(encoding="utf-8", errors="replace")
    head = text.lstrip()[:256]
    if head.startswith("#EXTM3U") or p.suffix.lower() == ".m3u8":
        return _parse_hls(text, p)
    if head.startswith("<?xml") or "<MPD" in head or p.suffix.lower() == ".mpd":
        return _parse_dash(text, p)
    raise ManifestError(f"无法识别的清单格式: {p}")


_EXTINF = re.compile(r"^#EXTINF:\s*([0-9][0-9.eE+-]*)")


def _parse_hls(text: str, path: Path) -> Manifest:
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    if not lines or lines[0] != "#EXTM3U":
        raise ManifestError("HLS 清单缺少 #EXTM3U 头")
    target = 0.0
    media_seq = 0
    seq_offset = 0          # 自上次 MEDIA-SEQUENCE 起的分片计数
    pending_dur: float | None = None
    segments: list[SegmentRef] = []
    cursor = 0.0
    for ln in lines[1:]:
        if ln.startswith("#EXT-X-TARGETDURATION:"):
            target = _check_duration(_to_float(ln.split(":", 1)[1], "TARGETDURATION"),
                                     "TARGETDURATION")
        elif ln.startswith("#EXT-X-MEDIA-SEQUENCE:"):
            # 允许出现在清单中部：模拟编码器重启/重试导致的序号回退，
            # 重复序号由 transport 层去重或判冲突。
            media_seq = _to_int(ln.split(":", 1)[1], "MEDIA-SEQUENCE")
            if media_seq < 0:
                raise ManifestError("MEDIA-SEQUENCE 越界（负数）")
            seq_offset = 0
        elif ln.startswith("#EXTINF:"):
            m = _EXTINF.match(ln)
            if not m:
                raise ManifestError(f"无法解析 EXTINF 行: {ln}")
            pending_dur = _check_duration(_to_float(m.group(1), "EXTINF"), "EXTINF")
        elif ln.startswith("#"):
            continue
        else:
            if pending_dur is None:
                raise ManifestError(f"分片缺少 EXTINF 时长: {ln}")
            _check_uri(ln)
            segments.append(SegmentRef(media_seq + seq_offset, ln, pending_dur, cursor))
            seq_offset += 1
            cursor += pending_dur
            pending_dur = None
    if not segments:
        raise ManifestError("清单不含任何分片")
    if target <= 0:
        target = max(s.duration for s in segments)
    return Manifest("hls", path, path.parent, target, segments)


def _parse_dash(text: str, path: Path) -> Manifest:
    try:
        root = ET.fromstring(text)
    except ET.ParseError as exc:
        raise ManifestError(f"MPD 解析失败: {exc}") from exc
    ns = ""
    if root.tag.startswith("{"):
        ns = root.tag.split("}")[0] + "}"
    if root.tag != f"{ns}MPD":
        raise ManifestError("不是有效的 MPD 文档")
    seg_list = root.find(f".//{ns}SegmentList")
    if seg_list is None:
        raise ManifestError("仅支持 SegmentList 形式的 DASH 清单")
    timescale = _to_int(seg_list.get("timescale", "1"), "timescale")
    if timescale <= 0:
        raise ManifestError("timescale 越界（非正数）")
    seg_dur = _check_duration(
        _to_int(seg_list.get("duration", "0"), "duration") / timescale,
        "SegmentList@duration",
    )
    start_number = _to_int(seg_list.get("startNumber", "1"), "startNumber")
    if start_number < 0:
        raise ManifestError("startNumber 越界（负数）")
    segments: list[SegmentRef] = []
    cursor = 0.0
    for i, node in enumerate(seg_list.findall(f"{ns}SegmentURL")):
        uri = node.get("media")
        if uri is None:
            raise ManifestError("SegmentURL 缺少 media 属性")
        _check_uri(uri)
        segments.append(SegmentRef(start_number + i, uri, seg_dur, cursor))
        cursor += seg_dur
    if not segments:
        raise ManifestError("清单不含任何分片")
    return Manifest("dash", path, path.parent, seg_dur, segments)


def _to_float(raw: str, ctx: str) -> float:
    try:
        return float(raw.strip())
    except ValueError as exc:
        raise ManifestError(f"无法解析数值 {raw!r}（{ctx}）") from exc


def _to_int(raw: str, ctx: str) -> int:
    try:
        return int(raw.strip())
    except ValueError as exc:
        raise ManifestError(f"无法解析整数 {raw!r}（{ctx}）") from exc
