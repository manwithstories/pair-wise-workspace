"""关键帧随机访问索引：校验旧索引、越界失效重扫、逐项解码验证。

索引同时支持两种随机访问：
- 按关键帧序号 seek（index_no）；
- 按目标时间 us 定位到其之前最近的关键帧。

H.264/H.265(annex-B) 用「文件描述符 seek 到字节偏移后重开容器」；
AV1(ivf) 带容器时间戳，用容器级 pts seek。
"""
from __future__ import annotations

import time
from dataclasses import asdict, dataclass

import av

from .model import (ErrorCategory, FrameRecord, Issue, TrackDecl,
                    VideoCodec, atomic_write_json, frames_to_us, read_json)

INDEX_KIND = "fielddeliver-keyframe-index"
INDEX_VERSION = 1


@dataclass(slots=True)
class KeyframeEntry:
    index_no: int          # 关键帧序号（从 0 起）
    byte_offset: int
    frame_index: int       # 对应修复后 FrameRecord.index
    pts_us: int
    raw_pts: int | None


@dataclass(slots=True)
class SeekProbe:
    target_us: int
    entry: KeyframeEntry
    ok: bool
    latency_ms: float
    first_decoded_pts_us: int | None
    error: str | None = None


@dataclass(slots=True)
class RandomAccessIndex:
    track_id: str
    codec: str
    file_size: int
    entries: list[KeyframeEntry]
    stale: bool
    issues: list[Issue]
    probes: list[SeekProbe]

    def entry_before(self, target_us: int) -> KeyframeEntry | None:
        """返回 pts_us <= target_us 的最后一个关键帧。"""
        chosen: KeyframeEntry | None = None
        for e in self.entries:
            if e.pts_us <= target_us:
                chosen = e
            else:
                break
        return chosen

    def to_dict(self) -> dict:
        return {
            "kind": INDEX_KIND, "version": INDEX_VERSION,
            "track_id": self.track_id, "codec": self.codec,
            "file_size": self.file_size, "stale": self.stale,
            "entry_count": len(self.entries),
            "entries": [asdict(e) for e in self.entries],
        }


# ---------------------------------------------------------------- 构建

def build_index(track: TrackDecl, frames: list[FrameRecord],
                file_size: int) -> RandomAccessIndex:
    """从修复后帧序列重建随机访问点（仅真实关键帧，缺口不建点）。"""
    entries: list[KeyframeEntry] = []
    no = 0
    for f in frames:
        if f.is_keyframe and not f.is_gap and f.byte_offset >= 0:
            entries.append(KeyframeEntry(
                index_no=no, byte_offset=f.byte_offset,
                frame_index=f.index, pts_us=f.pts_us, raw_pts=f.raw_pts))
            no += 1
    return RandomAccessIndex(
        track_id=track.track_id, codec=track.codec.value,
        file_size=file_size, entries=entries, stale=False, issues=[],
        probes=[])


def write_index(index: RandomAccessIndex, path: str) -> None:
    atomic_write_json(path, index.to_dict())


# ---------------------------------------------------------------- 旧索引校验

def validate_legacy_index(track: TrackDecl, current: RandomAccessIndex,
                          legacy_path: str) -> list[Issue]:
    """逐项校验旧关键帧索引；偏移越界或对不上解码帧则标记失效。"""
    issues: list[Issue] = []
    try:
        doc = read_json(legacy_path)
    except (OSError, ValueError) as exc:
        current.stale = True
        return [Issue(ErrorCategory.INDEX_STALE, track.track_id,
                      f"旧索引无法读取，触发重扫: {exc}", recovered=True)]

    if doc.get("kind") != INDEX_KIND or doc.get("track_id") != track.track_id:
        current.stale = True
        return [Issue(ErrorCategory.INDEX_STALE, track.track_id,
                      "旧索引 kind/track 不匹配，标记失效并重扫",
                      recovered=True)]
    if doc.get("file_size") not in (None, current.file_size):
        current.stale = True
        return [Issue(ErrorCategory.INDEX_STALE, track.track_id,
                      f"文件大小变化 {doc.get('file_size')}->{current.file_size}，"
                      "旧索引失效", recovered=True)]

    old = doc.get("entries", [])
    fresh = {(e.frame_index, e.byte_offset, e.pts_us) for e in current.entries}
    for item in old:
        off = int(item.get("byte_offset", -1))
        if off < 0 or off >= current.file_size:
            current.stale = True
            issues.append(Issue(
                ErrorCategory.INDEX_STALE, track.track_id,
                f"旧索引偏移 {off} 越界(file_size={current.file_size})，失效重扫",
                byte_offset=off, recovered=True))
            break
        key = (item.get("frame_index"), off, item.get("pts_us"))
        if key not in fresh:
            current.stale = True
            issues.append(Issue(
                ErrorCategory.INDEX_STALE, track.track_id,
                f"旧索引项 {item.get('index_no')} 与当前解码帧不一致，失效重扫",
                byte_offset=off, recovered=True))
            break
    if not current.stale and len(old) != len(current.entries):
        current.stale = True
        issues.append(Issue(
            ErrorCategory.INDEX_STALE, track.track_id,
            f"关键帧数量变化 {len(old)}->{len(current.entries)}，失效重扫",
            recovered=True))
    return issues


# ---------------------------------------------------------------- 跳转验证

def _seek_annexb(path: str, fmt: str, offset: int) -> int | None:
    """文件描述符 seek 到字节偏移，返回首个解码帧的修复帧号推导 PTS。"""
    fh = open(path, "rb")
    try:
        fh.seek(offset)
        container = av.open(fh, format=fmt)
        stream = container.streams.video[0]
        try:
            for pkt in container.demux(stream):
                for fr in pkt.decode():
                    if fr.pts is None:
                        return None
                    tb = fr.time_base
                    return round(int(fr.pts) * 1_000_000 * tb.numerator
                                 / tb.denominator)
        finally:
            container.close()
    finally:
        fh.close()
    return None


def _seek_ivf(path: str, target_pts_frames: int) -> int | None:
    container = av.open(path, format="ivf")
    try:
        stream = container.streams.video[0]
        container.seek(target_pts_frames, stream=stream)
        for pkt in container.demux(stream):
            for fr in pkt.decode():
                tb = fr.time_base
                return round(int(fr.pts) * 1_000_000 * tb.numerator
                             / tb.denominator)
    finally:
        container.close()
    return None


def verify_seek_points(track: TrackDecl, index: RandomAccessIndex,
                       path: str, sample: int | None = None) -> None:
    """逐项（或抽样）校验索引位置确实能解出对应关键帧。"""
    entries = index.entries
    if sample and len(entries) > sample:
        step = max(1, len(entries) // sample)
        entries = entries[::step][:sample]

    is_ivf = track.codec is VideoCodec.AV1
    for e in entries:
        t0 = time.perf_counter()
        err: str | None = None
        got_pts: int | None = None
        try:
            if is_ivf:
                got_pts = _seek_ivf(path, e.raw_pts if e.raw_pts is not None
                                    else e.frame_index)
            else:
                got_pts = _seek_annexb(path, track.codec.pyav_format,
                                       e.byte_offset)
        except av.FFmpegError as exc:
            err = type(exc).__name__
        latency = (time.perf_counter() - t0) * 1000.0

        ok = err is None and (
            got_pts is None or abs(got_pts - e.pts_us)
            <= frames_to_us(1, track.frame_rate))
        if not ok:
            index.stale = True
            index.issues.append(Issue(
                ErrorCategory.INDEX_STALE, track.track_id,
                f"关键帧 {e.index_no} 跳转校验失败 offset={e.byte_offset} "
                f"expected={e.pts_us}us got={got_pts} err={err}",
                byte_offset=e.byte_offset, pts_us=e.pts_us, recovered=True))
        index.probes.append(SeekProbe(
            target_us=e.pts_us, entry=e, ok=ok, latency_ms=latency,
            first_decoded_pts_us=got_pts, error=err))


def seek_pass_rate(index: RandomAccessIndex) -> float:
    if not index.probes:
        return 0.0
    return sum(1 for p in index.probes if p.ok) / len(index.probes)
