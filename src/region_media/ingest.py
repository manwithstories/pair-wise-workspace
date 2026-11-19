"""PyAV 解复用、回绕/断流识别与传输条件注入。

数据面：
* :func:`demux_segment` 用 PyAV 打开本地分片，按轨道解复用为
  :class:`PacketModel`；原始时间戳先经 :class:`MediaClock` 做 33 位
  回绕解算，回绕与真实时钟突变在此区分并写入错误模型；
* :class:`NetworkConditioner` 在解复用与缓冲之间模拟跨节点传输：
  乱序、突发丢包、重复分片、节点时钟漂移与截断，全部基于确定性
  随机种子，保证夹具与测试可复现。
"""

from __future__ import annotations

import os
import random
from dataclasses import dataclass, field

import av

from .model import (
    ErrorEvent,
    ErrorKind,
    MediaClock,
    PacketModel,
    SegmentStatus,
    TrackKind,
    WrapResult,
    now_us,
)


@dataclass
class NetworkProfile:
    """传输故障注入参数（比例为 0..1）。"""

    seed: int = 0
    reorder_rate: float = 0.0
    loss_rate: float = 0.0
    burst_loss_max: int = 0  # 突发丢包最大连续包数
    duplicate_rate: float = 0.0
    drift_ppm: float = 0.0  # 节点时钟相对媒体时钟的漂移
    jitter_us: int = 0
    truncate_packets: int = 0  # 截断：只保留前 N 个包，0 表示不截断


@dataclass
class IngestStats:
    packets_in: int = 0
    packets_out: int = 0
    lost: int = 0
    duplicated: int = 0
    truncated: bool = False
    wrap_events: int = 0
    jump_events: int = 0
    errors: list[ErrorEvent] = field(default_factory=list)


def _stream_kind(stream: av.stream.Stream) -> TrackKind | None:
    if stream.type == "audio":
        return TrackKind.AUDIO
    if stream.type == "video":
        return TrackKind.VIDEO
    return None


def demux_segment(path: str, segment_seq: int,
                  clocks: dict[TrackKind, MediaClock],
                  stats: IngestStats) -> list[PacketModel]:
    """解复用单个分片；损坏包/空轨写入 stats.errors 而不抛出。"""
    packets: list[PacketModel] = []
    counters = {TrackKind.AUDIO: 0, TrackKind.VIDEO: 0}
    try:
        container = av.open(path)
    except (av.AVError, OSError) as exc:
        stats.errors.append(ErrorEvent(
            ErrorKind.CORRUPT_PACKET, f"ingest:seg#{segment_seq}",
            f"分片无法打开 {os.path.basename(path)}: {exc}",
            segment_seq=segment_seq))
        return packets
    with container:
        streams = [s for s in container.streams if _stream_kind(s) is not None]
        if not streams:
            stats.errors.append(ErrorEvent(
                ErrorKind.EMPTY_TRACK, f"ingest:seg#{segment_seq}",
                "分片不包含任何音视频流", segment_seq=segment_seq))
            return packets
        seen_any = {TrackKind.AUDIO: False, TrackKind.VIDEO: False}
        try:
            for pkt in container.demux(streams):
                if pkt.dts is None or pkt.pts is None:
                    continue
                kind = _stream_kind(pkt.stream)
                if kind is None:
                    continue
                seen_any[kind] = True
                stats.packets_in += 1
                tb = pkt.time_base
                raw_pts = int(pkt.pts)
                dts_us = int(pkt.dts * tb * 1_000_000)
                dur_us = int((pkt.duration or 0) * tb * 1_000_000)
                # 节点时钟：以轨道原始时钟刻度做 33 位回绕解算，
                # 解回绕后再由时钟自身换算为统一微秒时间线
                timescale = int(round(1.0 / float(tb)))
                clock = clocks.setdefault(
                    kind, MediaClock(timescale, kind))
                wrapped_pts, wrap_res = clock.unwrap(raw_pts)
                if wrap_res == WrapResult.WRAPPED:
                    stats.wrap_events += 1
                    stats.errors.append(ErrorEvent(
                        ErrorKind.TIMESTAMP_WRAP,
                        f"ingest:seg#{segment_seq}:{kind.value}",
                        f"33 位时间戳回绕 raw={raw_pts}",
                        segment_seq=segment_seq))
                elif wrap_res == WrapResult.JUMP:
                    stats.jump_events += 1
                    stats.errors.append(ErrorEvent(
                        ErrorKind.CLOCK_JUMP,
                        f"ingest:seg#{segment_seq}:{kind.value}",
                        f"时钟突变 raw={raw_pts}",
                        segment_seq=segment_seq))
                seq = counters[kind]
                counters[kind] += 1
                packets.append(PacketModel(
                    track=kind, seq=seq, segment_seq=segment_seq,
                    raw_pts=raw_pts, pts_us=wrapped_pts,
                    dts_us=dts_us, duration_us=dur_us,
                    arrival_us=now_us(), size_bytes=pkt.size or 0,
                    keyframe=bool(pkt.is_keyframe)))
        except av.AVError as exc:
            stats.errors.append(ErrorEvent(
                ErrorKind.CORRUPT_PACKET, f"ingest:seg#{segment_seq}",
                f"解复用中断: {exc}", segment_seq=segment_seq))
        for kind, seen in seen_any.items():
            if not seen and any(_stream_kind(s) == kind for s in streams):
                stats.errors.append(ErrorEvent(
                    ErrorKind.EMPTY_TRACK, f"ingest:seg#{segment_seq}",
                    f"{kind.value} 轨无有效包", segment_seq=segment_seq))
    return packets


class NetworkConditioner:
    """确定性传输故障注入，顺序：截断 -> 突发丢包 -> 重复 -> 乱序 -> 漂移/抖动。"""

    def __init__(self, profile: NetworkProfile) -> None:
        self.profile = profile
        self._rng = random.Random(profile.seed)
        self._drop_budget = 0

    def _should_drop(self) -> bool:
        p = self.profile
        if self._drop_budget > 0:
            self._drop_budget -= 1
            return True
        if p.burst_loss_max and self._rng.random() < p.loss_rate / 2:
            self._drop_budget = self._rng.randrange(p.burst_loss_max)
            return True
        return self._rng.random() < p.loss_rate

    def apply(self, packets: list[PacketModel],
              stats: IngestStats) -> list[PacketModel]:
        p = self.profile
        out: list[PacketModel] = []
        src = packets
        if p.truncate_packets and len(src) > p.truncate_packets:
            src = src[:p.truncate_packets]
            stats.truncated = True
            stats.errors.append(ErrorEvent(
                ErrorKind.TRUNCATED_MANIFEST, "network:conditioner",
                f"分片截断保留 {len(src)}/{len(packets)} 包"))
        drift_factor = 1.0 + p.drift_ppm / 1_000_000.0
        base = now_us()
        for pkt in src:
            if self._should_drop():
                stats.lost += 1
                continue
            # 漂移作用在到达墙钟上（媒体时间戳不变，由 clock 模块估计）
            arrival = base + int((pkt.pts_us * drift_factor)
                                 + self._rng.uniform(-p.jitter_us, p.jitter_us))
            moved = PacketModel(
                track=pkt.track, seq=pkt.seq, segment_seq=pkt.segment_seq,
                raw_pts=pkt.raw_pts, pts_us=pkt.pts_us, dts_us=pkt.dts_us,
                duration_us=pkt.duration_us, arrival_us=arrival,
                size_bytes=pkt.size_bytes, keyframe=pkt.keyframe)
            out.append(moved)
            if p.duplicate_rate and self._rng.random() < p.duplicate_rate:
                stats.duplicated += 1
                out.append(PacketModel(**{**moved.__dict__,
                                          "arrival_us": arrival + 1}))
        if p.reorder_rate:
            self._reorder(out)
        stats.packets_out += len(out)
        return out

    def _reorder(self, packets: list[PacketModel]) -> None:
        """以相邻交换为主的局部乱序，避免破坏全部时间线结构。"""
        rng = self._rng
        for i in range(1, len(packets)):
            if rng.random() < self.profile.reorder_rate:
                j = max(0, i - rng.randint(1, 3))
                packets[i], packets[j] = packets[j], packets[i]


def fingerprint(packets: list[PacketModel]) -> int:
    """包序列内容指纹，供检查点续跑一致性校验。"""
    h = 1469598103934665603
    for pkt in packets:
        word = (pkt.seq << 40) ^ (int(pkt.pts_us) & 0xFFFFFFFFFF)
        for _ in range(8):
            h = ((h ^ (word & 0xFF)) * 1099511628211) & 0xFFFFFFFFFFFFFFFF
            word >>= 8
    return h


__all__ = ["NetworkProfile", "NetworkConditioner", "IngestStats",
           "demux_segment", "fingerprint", "SegmentStatus"]
