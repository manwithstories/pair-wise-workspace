r"""HLS / DASH 清单解析与引用校验。

支持的清单形态（与本地生成器配套，均为标准子集）：
* HLS：master 播放列表（EXT-X-STREAM-INF）引用音频/视频媒体播放列表，
  媒体列表含 EXT-X-TARGETDURATION、EXTINF 分片与 EXT-X-ENDLIST；
* DASH：MPD 内两个 AdaptationSet，使用 SegmentTemplate + SegmentTimeline，
  通过 ``\$Number\$`` 模板生成分片引用。

校验项：路径越界、重复序号冲突、缺失分片引用、序号/时长合理性、
单片字节上限与清单引用数量上限。错误以 :class:`ErrorEvent` 记录并
把对应引用标记为 REJECTED，保证可定位且不静默丢弃。
"""

from __future__ import annotations

import os
import re
from dataclasses import dataclass, field
from xml.etree import ElementTree as ET

from .model import (
    ErrorEvent,
    ErrorKind,
    ManifestFormat,
    ManifestRef,
    MAX_MANIFEST_REFS,
    MAX_SEGMENT_BYTES,
    SegmentStatus,
    TrackKind,
)

MPD_NS = "urn:mpeg:dash:schema:mpd:2011"


class ManifestError(ValueError):
    """清单整体无效（退出码 2 级别），携带定位信息。"""

    def __init__(self, kind: ErrorKind, location: str, detail: str) -> None:
        super().__init__(f"{kind.value} @ {location}: {detail}")
        self.event = ErrorEvent(kind, location, detail)


@dataclass
class ManifestModel:
    fmt: ManifestFormat
    base_dir: str
    refs: list[ManifestRef] = field(default_factory=list)
    errors: list[ErrorEvent] = field(default_factory=list)
    truncated: bool = False
    target_duration_us: int = 0
    input_digest: int = 0  # 清单内容指纹，检查点一致性校验用

    @property
    def rejected(self) -> list[ManifestRef]:
        return [r for r in self.refs if r.rejected]

    def refs_for(self, kind: TrackKind) -> list[ManifestRef]:
        return [r for r in self.refs if r.kind == kind]



def _safe_resolve(base_dir: str, uri: str, location: str) -> str:
    """把清单内相对 URI 解析为受限目录内的绝对路径，拒绝越界。"""
    if uri.startswith(("http://", "https://", "file://")):
        raise ManifestError(ErrorKind.PATH_TRAVERSAL, location,
                            f"仅支持本地分片引用: {uri}")
    if os.path.isabs(uri) or uri.startswith(("/", "\\")):
        raise ManifestError(ErrorKind.PATH_TRAVERSAL, location,
                            f"拒绝绝对路径引用: {uri}")
    norm = os.path.normpath(os.path.join(base_dir, uri))
    base = os.path.normpath(base_dir)
    if os.path.commonpath([norm, base]) != base:
        raise ManifestError(ErrorKind.PATH_TRAVERSAL, location,
                            f"引用越界: {uri}")
    return norm


def _coalesce_refs(
    refs: list[ManifestRef],
    base_dir: str,
    errors: list[ErrorEvent],
    existing: dict[tuple[int, TrackKind], str],
) -> list[ManifestRef]:
    """重复序号冲突、缺失文件、字节/数量上限统一校验。"""
    if len(refs) > MAX_MANIFEST_REFS:
        raise ManifestError(
            ErrorKind.LIMIT_EXCEEDED, "manifest",
            f"引用数量 {len(refs)} 超过上限 {MAX_MANIFEST_REFS}")
    out: list[ManifestRef] = []
    for ref in refs:
        loc = f"manifest:seg#{ref.seq}:{ref.kind.value if ref.kind else '?'}"
        key = (ref.seq, ref.kind or TrackKind.AUDIO)
        if key in existing:
            evt = ErrorEvent(ErrorKind.DUPLICATE_SEQUENCE, loc,
                             f"序号 {ref.seq} 与 {existing[key]} 冲突",
                             segment_seq=ref.seq)
            errors.append(evt)
            ref.rejected = True
            out.append(ref)
            continue
        existing[key] = ref.uri
        abs_path = _safe_resolve(base_dir, ref.uri, loc)
        if not os.path.isfile(abs_path):
            errors.append(ErrorEvent(
                ErrorKind.MISSING_REFERENCE, loc,
                f"分片文件不存在: {ref.uri}", segment_seq=ref.seq))
            ref.rejected = True
        elif os.path.getsize(abs_path) > MAX_SEGMENT_BYTES:
            errors.append(ErrorEvent(
                ErrorKind.LIMIT_EXCEEDED, loc,
                f"分片 {ref.uri} 超过 {MAX_SEGMENT_BYTES} 字节",
                segment_seq=ref.seq))
            ref.rejected = True
        else:
            ref.byte_size = os.path.getsize(abs_path)
        out.append(ref)
    return out


def _digest(path: str) -> int:
    h = 1469598103934665603
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(65536), b""):
            for b in chunk:
                h = ((h ^ b) * 1099511628211) & 0xFFFFFFFFFFFFFFFF
    return h


def _parse_hls(path: str) -> ManifestModel:
    base_dir = os.path.dirname(os.path.abspath(path))
    with open(path, encoding="utf-8") as fh:
        text = fh.read()
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    if not lines or not lines[0].startswith("#EXTM3U"):
        raise ManifestError(ErrorKind.TRUNCATED_MANIFEST, path,
                            "缺少 #EXTM3U 头")
    errors: list[ErrorEvent] = []
    # master 播放列表本身不带 ENDLIST，截断判定下沉到媒体列表
    truncated = False
    target_dur = 0
    playlist_files: list[tuple[str, TrackKind]] = []
    pending_info = ""
    for ln in lines[1:]:
        if ln.startswith("#EXT-X-STREAM-INF"):
            pending_info = ln
            continue
        if ln.startswith("#EXT-X-MEDIA"):
            m_type = re.search(r'TYPE=([A-Z]+)', ln)
            m_uri = re.search(r'URI="([^"]+)"', ln)
            if m_type and m_uri:
                kind = TrackKind.AUDIO if m_type.group(1) == "AUDIO" \
                    else TrackKind.VIDEO
                playlist_files.append((m_uri.group(1), kind))
            continue
        if ln.startswith("#"):
            if ln.startswith("#EXT-X-TARGETDURATION"):
                target_dur = int(ln.split(":")[1]) * 1_000_000
            continue
        if pending_info:  # STREAM-INF 的下一行是变体播放列表
            playlist_files.append((ln, TrackKind.VIDEO))
            pending_info = ""
            continue
    refs: list[ManifestRef] = []
    existing: dict[tuple[int, TrackKind], str] = {}
    if playlist_files:
        for pl_uri, kind in playlist_files:
            pl_path = _safe_resolve(base_dir, pl_uri, f"hls:{pl_uri}")
            sub_refs, sub_trunc = _parse_hls_media(
                pl_path, kind, errors, base_dir)
            refs.extend(sub_refs)
            truncated = truncated or sub_trunc
    else:  # 直接传入媒体播放列表
        refs, truncated = _parse_hls_media(
            path, TrackKind.AUDIO, errors, base_dir, skip_header=True)
    if truncated:
        errors.append(ErrorEvent(ErrorKind.TRUNCATED_MANIFEST, path,
                                 "媒体播放列表缺少 #EXT-X-ENDLIST，清单被截断"))
    refs = _coalesce_refs(refs, base_dir, errors, existing)
    model = ManifestModel(ManifestFormat.HLS, base_dir, refs, errors,
                          truncated, target_dur, _digest(path))
    return model


def _parse_hls_media(path: str, kind: TrackKind,
                     errors: list[ErrorEvent], top_base_dir: str,
                     skip_header: bool = False) -> tuple[list[ManifestRef], bool]:
    with open(path, encoding="utf-8") as fh:
        text = fh.read()
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    if not lines or not lines[0].startswith("#EXTM3U"):
        raise ManifestError(ErrorKind.TRUNCATED_MANIFEST, path,
                            "媒体播放列表缺少 #EXTM3U 头")
    pl_dir = os.path.dirname(os.path.abspath(path))
    truncated = "#EXT-X-ENDLIST" not in text
    refs: list[ManifestRef] = []
    seq = 0
    m = re.search(r"#EXT-X-MEDIA-SEQUENCE:(\d+)", text)
    if m:
        seq = int(m.group(1))
    pending_dur = 0
    for ln in lines[1:]:
        if ln.startswith("#EXTINF"):
            pending_dur = int(float(ln.split(":")[1].split(",")[0]) * 1e6)
            continue
        if ln.startswith("#"):
            continue
        if pending_dur <= 0:
            errors.append(ErrorEvent(
                ErrorKind.TRUNCATED_MANIFEST, f"hls:{os.path.basename(path)}",
                f"分片 {ln} 缺少 EXTINF 时长", segment_seq=seq))
        # 分片相对媒体列表自身目录解析，再换算为相对顶层清单的 URI，
        # 使后续越界校验与文件存在性检查统一在顶层基准目录完成
        seg_abs = os.path.normpath(os.path.join(pl_dir, ln))
        seg_rel = os.path.relpath(seg_abs, top_base_dir)
        refs.append(ManifestRef(seq, seg_rel, pending_dur, kind=kind))
        seq += 1
        pending_dur = 0
    return refs, truncated


def _parse_dash(path: str) -> ManifestModel:
    base_dir = os.path.dirname(os.path.abspath(path))
    with open(path, "rb") as fh:
        raw = fh.read()
    try:
        root = ET.fromstring(raw)
    except ET.ParseError as exc:
        raise ManifestError(ErrorKind.TRUNCATED_MANIFEST, path,
                            f"MPD XML 解析失败: {exc}") from exc
    errors: list[ErrorEvent] = []
    ns = {"m": MPD_NS}
    if root.tag != f"{{{MPD_NS}}}MPD":
        raise ManifestError(ErrorKind.TRUNCATED_MANIFEST, path, "非 MPD 根节点")
    truncated = root.get("type", "static") == "dynamic"
    if truncated:
        errors.append(ErrorEvent(ErrorKind.TRUNCATED_MANIFEST, path,
                                 "动态 MPD：分片列表可能不完整"))
    refs: list[ManifestRef] = []
    existing: dict[tuple[int, TrackKind], str] = {}
    for adap in root.findall(".//m:AdaptationSet", ns):
        kind = TrackKind.AUDIO if adap.get("contentType") == "audio" \
            else TrackKind.VIDEO
        tmpl = adap.find("m:SegmentTemplate", ns)
        if tmpl is None:
            raise ManifestError(ErrorKind.TRUNCATED_MANIFEST, path,
                                f"{kind.value} 缺少 SegmentTemplate")
        start = int(tmpl.get("startNumber", "1"))
        timescale = int(tmpl.get("timescale", "1000000"))
        media_tmpl = tmpl.get("media", "")
        timeline = tmpl.find("m:SegmentTimeline", ns)
        n = start
        if timeline is None:
            raise ManifestError(ErrorKind.TRUNCATED_MANIFEST, path,
                                "缺少 SegmentTimeline")
        for s in timeline.findall("m:S", ns):
            repeats = int(s.get("r", "0")) + 1
            dur_us = int(s.get("d", "0")) * 1_000_000 // timescale
            for _ in range(repeats):
                uri = media_tmpl.replace("$Number$", str(n)).replace(
                    "$RepresentationID$", tmpl.get("id", kind.value))
                refs.append(ManifestRef(n, uri, dur_us, kind=kind))
                n += 1
    refs = _coalesce_refs(refs, base_dir, errors, existing)
    return ManifestModel(ManifestFormat.DASH, base_dir, refs, errors,
                         truncated, 0, _digest(path))


def parse_manifest(path: str) -> ManifestModel:
    """根据扩展名分派 HLS / DASH 解析。"""
    if not os.path.isfile(path):
        raise ManifestError(ErrorKind.MISSING_REFERENCE, path, "清单文件不存在")
    lower = path.lower()
    if lower.endswith((".m3u8", ".m3u")):
        return _parse_hls(os.path.abspath(path))
    if lower.endswith(".mpd"):
        return _parse_dash(os.path.abspath(path))
    raise ManifestError(ErrorKind.TRUNCATED_MANIFEST, path,
                        "无法识别的清单扩展名（支持 .m3u8/.mpd）")


def is_rejected(ref: ManifestRef) -> bool:
    return _REJECT_FLAGS.get(id(ref), False)


def mark_rejected(ref: ManifestRef) -> None:
    ref.rejected = True


__all__ = ["ManifestError", "ManifestModel", "parse_manifest",
           "is_rejected", "mark_rejected", "SegmentStatus"]
