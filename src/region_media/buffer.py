"""自适应抖动缓冲与乱序窗口。

每个轨道维护一个按包序号排序的到达窗口：
* 窗口宽度随到达抖动（相对传输时延的 EWMA 方差）动态调整，夹在
  ``[MIN_WINDOW_US, MAX_WINDOW_US]`` 之间；
* 落在窗口内的乱序包参与重排输出，窗口外迟到包不静默重排，
  原样以 LATE_OUT_OF_WINDOW 错误退回上层计入不可恢复丢失；
* 重复包（同序号曾被接收，含已播放后的重传）标记 DUPLICATE，
  与「从未到达、迟到的缺口包」严格区分，由恢复模块记账。
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .model import ErrorEvent, ErrorKind, PacketModel, SegmentStatus, TrackKind

MIN_WINDOW_US = 20_000
MAX_WINDOW_US = 2_000_000
DEFAULT_WINDOW_US = 120_000
WARMUP_SAMPLES = 4
EWMA_ALPHA = 0.125


@dataclass
class _TrackBuffer:
    track: TrackKind
    window_us: int = DEFAULT_WINDOW_US
    highest_seq: int = -1
    playhead: int = -1
    _pending: dict[int, PacketModel] = field(default_factory=dict)
    _seen: set[int] = field(default_factory=set)
    _ewma_delay: float = 0.0
    _ewma_var: float = 0.0
    _base_arrival: int | None = None
    _base_pts: int | None = None
    _observed: int = 0
    duplicates: int = 0
    late_drops: int = 0

    def _observe_jitter(self, pkt: PacketModel) -> None:
        """仅对正常接纳包更新时延抖动 EWMA，平滑调整动态窗口。"""
        if self._base_arrival is None:
            self._base_arrival = pkt.arrival_us
            self._base_pts = pkt.pts_us
        else:
            transit = ((pkt.arrival_us - self._base_arrival)
                       - (pkt.pts_us - (self._base_pts or 0)))
            if self._observed == 0:
                self._ewma_delay = float(transit)
            dev = transit - self._ewma_delay
            self._ewma_delay += EWMA_ALPHA * dev
            self._ewma_var = (1 - EWMA_ALPHA) * (
                self._ewma_var + EWMA_ALPHA * dev * dev)
        self._observed += 1
        if self._observed < WARMUP_SAMPLES:
            return
        # 4 倍标准差覆盖约 99.99% 正常抖动；EWMA 平滑防止单样本跳变
        target = int(4 * (self._ewma_var ** 0.5)) + MIN_WINDOW_US
        target = max(MIN_WINDOW_US, min(MAX_WINDOW_US, target))
        self.window_us = int(0.875 * self.window_us + 0.125 * target)


class JitterBuffer:
    """双轨抖动缓冲；push 返回处置结果，drain 按序吐出可播放包。"""

    def __init__(self) -> None:
        self._bufs = {
            TrackKind.AUDIO: _TrackBuffer(TrackKind.AUDIO),
            TrackKind.VIDEO: _TrackBuffer(TrackKind.VIDEO),
        }
        self.errors: list[ErrorEvent] = []

    @property
    def window_us(self) -> dict[str, int]:
        return {k.value: b.window_us for k, b in self._bufs.items()}

    def push(self, pkt: PacketModel) -> SegmentStatus:
        buf = self._bufs[pkt.track]
        # 1) 重复：同序号曾被接收（含已 drain 播放后的重传）
        if pkt.seq in buf._seen:
            buf.duplicates += 1
            return SegmentStatus.DUPLICATE
        # 2) 窗口外迟到：播放头已越过，或乱序跨度超过当前窗口
        if buf.playhead >= 0 and pkt.seq < buf.playhead:
            buf.late_drops += 1
            self.errors.append(ErrorEvent(
                ErrorKind.LATE_OUT_OF_WINDOW,
                f"buffer:{pkt.track.value}:seq{pkt.seq}",
                f"迟到包 seq={pkt.seq} 落后播放头 {buf.playhead}",
                segment_seq=pkt.segment_seq))
            return SegmentStatus.MISSING
        if buf.highest_seq >= 0 and pkt.seq < buf.highest_seq:
            ref = buf._pending.get(buf.highest_seq)
            gap_span = ref.pts_us - pkt.pts_us if ref else 0
            if gap_span > buf.window_us:
                buf.late_drops += 1
                self.errors.append(ErrorEvent(
                    ErrorKind.LATE_OUT_OF_WINDOW,
                    f"buffer:{pkt.track.value}:seq{pkt.seq}",
                    f"乱序跨度 {gap_span}us 超过窗口 {buf.window_us}us",
                    segment_seq=pkt.segment_seq))
                return SegmentStatus.MISSING
        # 3) 正常接纳（含窗口内乱序），参与抖动估计
        buf._pending[pkt.seq] = pkt
        buf._seen.add(pkt.seq)
        buf.highest_seq = max(buf.highest_seq, pkt.seq)
        buf._observe_jitter(pkt)
        return SegmentStatus.RECEIVED

    def drain(self, track: TrackKind) -> list[PacketModel]:
        """输出从播放头起连续可用的包。

        遇缺口停止等待乱序/重传；当缺失序号之后已有更高序号包到达
        （确定缺口），越过等待位置继续输出，使后续包带着序号跳跃交给
        恢复模块补合成包，而不是无限等待一个不会再来的包。
        """
        buf = self._bufs[track]
        out: list[PacketModel] = []
        nxt = buf.playhead + 1
        while nxt <= buf.highest_seq:
            if nxt in buf._pending:
                out.append(buf._pending.pop(nxt))
                buf.playhead = nxt
                nxt += 1
            elif nxt < buf.highest_seq:
                # 确定缺口：其后已有更高序号包，越过等待位置
                buf.playhead = nxt
                nxt += 1
            else:
                break
        return out

    def skip_gap(self, track: TrackKind, seq: int) -> None:
        """恢复模块确认不可恢复后，把缺口序号视为已消耗并推进播放头。"""
        buf = self._bufs[track]
        buf._pending.pop(seq, None)
        if seq > buf.playhead:
            buf.playhead = seq

    def stats(self) -> dict[str, dict[str, int]]:
        return {
            k.value: {
                "window_us": b.window_us,
                "duplicates": b.duplicates,
                "late_drops": b.late_drops,
                "pending": len(b._pending),
                "playhead": b.playhead,
                "highest_seq": b.highest_seq,
            }
            for k, b in self._bufs.items()
        }
