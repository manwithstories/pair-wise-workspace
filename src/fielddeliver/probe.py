"""裸码流解析与时间戳规范化。

职责：
1. 校验采集清单必需字段，拒绝时指出轨道与字节偏移；
2. 用 PyAV 扫描 H.264/H.265/AV1(ivf) 裸码流，收集 (偏移, 大小, 关键帧)；
3. 修复时间戳回退、回绕、重复帧与有限丢包；无法推断的区间保留显式缺口。

修复以「帧号」为内部单调单位，再按声明帧率换算为统一微秒时间基准。
"""
from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

import av

from .model import (DEFAULT_LIMITS, ErrorCategory, FrameRecord, Issue,
                    Manifest, PipelineError, TrackDecl, TrackKind,
                    VideoCodec, VideoInfo, frames_to_us, resolve_within_root)

# 相邻原始帧号差值超过该阈值即视为「无法推断的大缺口」，不做插值伪造。
MAX_INFERABLE_SKIP = 3


@dataclass(slots=True)
class RawPacket:
    byte_offset: int
    size: int
    is_keyframe: bool
    raw_pts: int | None      # ivf 携带的帧号；裸 annex-B 为 None


@dataclass(slots=True)
class ProbeResult:
    track: TrackDecl
    packets: list[RawPacket]
    frames: list[FrameRecord]
    video: VideoInfo
    issues: list[Issue]
    truncated: bool


# ---------------------------------------------------------------- 清单校验

_VIDEO_REQUIRED = ("path", "codec", "frame_rate")
_AUDIO_REQUIRED = ("path", "sample_rate", "channels")


def validate_manifest(manifest: Manifest) -> list[Issue]:
    """语义校验；结构性/必需字段错误直接抛 INVALID_INPUT。"""
    issues: list[Issue] = []
    if not manifest.tracks:
        raise PipelineError(ErrorCategory.MANIFEST_MISSING_FIELD,
                            "清单不包含任何轨道")
    if len(manifest.tracks) > manifest.limits["max_tracks"]:
        raise PipelineError(
            ErrorCategory.INPUT_TOO_LARGE,
            f"轨道数 {len(manifest.tracks)} 超过上限 "
            f"{manifest.limits['max_tracks']}")

    ids = set(manifest.tracks)
    total = 0
    for tid, tr in manifest.tracks.items():
        required = _VIDEO_REQUIRED if tr.kind is TrackKind.VIDEO else _AUDIO_REQUIRED
        for key in required:
            if not getattr(tr, key, None):
                raise PipelineError(
                    ErrorCategory.MANIFEST_MISSING_FIELD,
                    f"轨道 {tid!r} 缺少必需字段 {key!r}", track=tid)
        if tr.kind is TrackKind.VIDEO:
            VideoCodec(tr.codec)  # 非法枚举抛 ValueError
            if tr.frame_rate <= 0:
                raise PipelineError(
                    ErrorCategory.MANIFEST_BAD_VALUE,
                    f"轨道 {tid!r} frame_rate 必须为正数", track=tid)
        else:
            if tr.sample_rate <= 0 or tr.channels <= 0:
                raise PipelineError(
                    ErrorCategory.MANIFEST_BAD_VALUE,
                    f"轨道 {tid!r} 采样率/声道数必须为正数", track=tid)
        for dep in tr.depends_on:
            if dep not in ids:
                raise PipelineError(
                    ErrorCategory.DEPENDENCY_MISSING,
                    f"轨道 {tid!r} 依赖的 {dep!r} 不存在", track=tid)
            if dep == tid:
                raise PipelineError(
                    ErrorCategory.DEPENDENCY_CYCLE,
                    f"轨道 {tid!r} 不能依赖自身", track=tid)

        p = resolve_within_root(manifest.root, tr.path)
        if not p.is_file():
            raise PipelineError(
                ErrorCategory.MANIFEST_MISSING_FIELD,
                f"轨道 {tid!r} 的素材文件不存在: {p}", track=tid)
        size = p.stat().st_size
        if size > manifest.limits["max_file_bytes"]:
            raise PipelineError(
                ErrorCategory.INPUT_TOO_LARGE,
                f"轨道 {tid!r} 文件 {size}B 超过单文件上限 "
                f"{manifest.limits['max_file_bytes']}B", track=tid)
        total += size
    if total > manifest.limits["max_total_bytes"]:
        raise PipelineError(
            ErrorCategory.INPUT_TOO_LARGE,
            f"总输入 {total}B 超过上限 "
            f"{manifest.limits['max_total_bytes']}B")
    if manifest.reference_video and manifest.reference_video not in ids:
        raise PipelineError(
            ErrorCategory.DEPENDENCY_MISSING,
            f"reference_video {manifest.reference_video!r} 不存在")
    return issues


# ---------------------------------------------------------------- 裸流扫描

def scan_packets(path: str, codec: VideoCodec) -> tuple[list[RawPacket], bool]:
    """扫描裸码流，返回包列表与截断标志。

    ivf(AV1) 自带帧时间戳；annex-B(H.264/H.265) 无 PTS，raw_pts=None。
    解码中途报错视为截断，保留此前已扫描的包。
    """
    packets: list[RawPacket] = []
    truncated = False
    fh = open(path, "rb")
    try:
        container = av.open(fh, format=codec.pyav_format)
        stream = container.streams.video[0]
        try:
            for pkt in container.demux(stream):
                if pkt.pos is None or pkt.size <= 0:
                    continue
                packets.append(RawPacket(
                    byte_offset=int(pkt.pos), size=int(pkt.size),
                    is_keyframe=bool(pkt.is_keyframe),
                    raw_pts=int(pkt.pts) if pkt.pts is not None else None))
        except (av.FFmpegError, av.error.InvalidDataError):
            truncated = True
        finally:
            container.close()
    finally:
        fh.close()

    file_size = os.path.getsize(path)
    if packets and packets[-1].byte_offset + packets[-1].size < file_size:
        # 尾部存在解析器无法消费的残余字节，判定为截断。
        truncated = True
    return packets, truncated


# ---------------------------------------------------------------- 时间戳修复

def _issue(track: str, cat: ErrorCategory, msg: str,
           offset: int | None = None, pts: int | None = None,
           recovered: bool | None = None, detail: dict | None = None) -> Issue:
    return Issue(category=cat, track=track, message=msg,
                 byte_offset=offset, pts_us=pts, recovered=recovered,
                 detail=detail or {})


def normalize_timestamps(track: TrackDecl, packets: list[RawPacket]
                         ) -> tuple[list[FrameRecord], list[Issue]]:
    """把扫描包修复为单调帧序列；缺口处插入 is_gap 记录，绝不伪造帧。"""
    issues: list[Issue] = []
    frames: list[FrameRecord] = []
    rate = track.frame_rate
    wrap = track.pts_wrap_modulus

    prev_raw: int | None = None
    out_index = 0
    for pkt in packets:
        raw = pkt.raw_pts
        inferred_raw: int | None = None

        if raw is None:
            # annex-B：按输出顺序赋连续帧号。
            inferred_raw = 0 if prev_raw is None else prev_raw + 1
        elif prev_raw is not None:
            delta = raw - prev_raw
            wrapped = (wrap is not None and raw < prev_raw
                       and (wrap - prev_raw + raw) < wrap // 2)
            if wrapped:
                # 回绕优先判定：raw 跳到低值，按模还原单调序列。
                inferred_raw = prev_raw + (wrap - prev_raw + raw)
                issues.append(_issue(
                    track.track_id, ErrorCategory.TIMESTAMP_REPAIRED,
                    f"PTS 回绕 {prev_raw}->{raw} (mod={wrap}) 已修复",
                    pkt.byte_offset, recovered=True))
            elif raw == prev_raw:
                # 重复帧：保留首帧，跳过重复，不重复计数。
                issues.append(_issue(
                    track.track_id, ErrorCategory.DUPLICATE_FRAME,
                    f"重复帧 raw_pts={raw}，丢弃重复包",
                    pkt.byte_offset, recovered=True))
                continue
            elif delta < 0:
                # 回退（非回绕）：时钟跳变，按顺序位置重映射。
                inferred_raw = prev_raw + 1
                issues.append(_issue(
                    track.track_id, ErrorCategory.TIMESTAMP_REPAIRED,
                    f"时间戳回退 {prev_raw}->{raw}，按顺序重映射",
                    pkt.byte_offset, recovered=True))
            elif 1 < delta <= MAX_INFERABLE_SKIP:
                # 有限丢包：可推断缺失帧号，但不伪造，先补显式缺口。
                for missing in range(prev_raw + 1, raw):
                    frames.append(FrameRecord(
                        index=out_index, byte_offset=-1, size=0,
                        is_keyframe=False,
                        pts_us=frames_to_us(missing, rate),
                        raw_pts=missing, is_gap=True))
                    out_index += 1
                    issues.append(_issue(
                        track.track_id, ErrorCategory.PACKET_LOSS_RECOVERED,
                        f"有限丢包：推断缺失帧 {missing}，保留显式缺口",
                        pts=frames_to_us(missing, rate), recovered=True))
                inferred_raw = raw
            elif delta > MAX_INFERABLE_SKIP:
                # 无法推断的大区间：显式缺口覆盖整个空洞，不插值。
                gap_start = frames_to_us(prev_raw + 1, rate)
                gap_end = frames_to_us(raw, rate)
                frames.append(FrameRecord(
                    index=out_index, byte_offset=-1, size=0,
                    is_keyframe=False, pts_us=gap_start,
                    raw_pts=prev_raw + 1, is_gap=True))
                out_index += 1
                issues.append(_issue(
                    track.track_id, ErrorCategory.TIMESTAMP_GAP,
                    f"无法推断的时间戳缺口 {delta} 帧 "
                    f"({gap_start}..{gap_end}us)，保留显式缺口",
                    pts=gap_start, recovered=False,
                    detail={"gap_us": gap_end - gap_start}))
                inferred_raw = raw
            else:
                inferred_raw = raw
        else:
            inferred_raw = raw

        frames.append(FrameRecord(
            index=out_index, byte_offset=pkt.byte_offset, size=pkt.size,
            is_keyframe=pkt.is_keyframe,
            pts_us=frames_to_us(inferred_raw, rate), raw_pts=inferred_raw))
        out_index += 1
        prev_raw = inferred_raw

    return frames, issues


# ---------------------------------------------------------------- 入口

def probe_video(track: TrackDecl, root: str) -> ProbeResult:
    """供调度层调用：先把清单相对路径解析到 root 内再探测。"""
    track.path = str(resolve_within_root(root, track.path))
    packets, truncated = scan_packets(track.path, track.codec)
    if not packets:
        raise PipelineError(
            ErrorCategory.TRUNCATED_STREAM,
            f"轨道 {track.track_id!r} 未扫描到任何数据包（空/损坏码流）",
            track=track.track_id)

    frames, issues = normalize_timestamps(track, packets)
    if truncated:
        issues.append(_issue(
            track.track_id, ErrorCategory.TRUNCATED_STREAM,
            "码流尾部截断：残余字节无法解析",
            offset=packets[-1].byte_offset + packets[-1].size,
            recovered=False))

    # 解码探测：确认分辨率/帧率并统计真正可解码帧。
    width = height = 0
    decoded = 0
    decode_fail = 0
    fh = open(path, "rb")
    try:
        container = av.open(fh, format=track.codec.pyav_format)
        stream = container.streams.video[0]
        try:
            for pkt in container.demux(stream):
                try:
                    for fr in pkt.decode():
                        width, height = fr.width, fr.height
                        decoded += 1
                except av.FFmpegError:
                    decode_fail += 1
        finally:
            container.close()
    finally:
        fh.close()
    if decode_fail:
        issues.append(_issue(
            track.track_id, ErrorCategory.DECODE_FAILURE,
            f"{decode_fail} 个包解码失败（解码器错误隐藏/丢帧）",
            recovered=False))

    key_count = sum(1 for f in frames if f.is_keyframe and not f.is_gap)
    gaps = sum(1 for f in frames if f.is_gap)
    last_pts = frames[-1].pts_us if frames else 0
    frame_dur_us = round(1_000_000 / track.frame_rate)
    info = VideoInfo(
        width=width, height=height, frame_rate=track.frame_rate,
        duration_us=last_pts + frame_dur_us,
        packet_count=len(packets), frame_count=len(frames) - gaps,
        keyframe_count=key_count, gaps=gaps)
    return ProbeResult(track=track, packets=packets, frames=frames,
                       video=info, issues=issues, truncated=truncated)


def probe_video(track: TrackDecl, root: str) -> ProbeResult:
    """供调度层调用：先把清单相对路径解析到 root 内再探测。"""
    track.path = str(resolve_within_root(root, track.path))
    return probe_video_track(track)
