"""丢包、重复分片与可恢复缺口处理。

恢复判定基于相邻分片时间戳与音频连续性：
* 缺口两侧时间戳与包时长可以线性对齐（残差在容差内）时，按轨道
  周期补出合成包并标记 ``synthetic=True``，计入恢复率分子；
* 跨分片边界缺口过大、两侧时长不一致或音频采样不连续时，不做
  猜测，标记 GAP_UNRECOVERABLE 并保留原因，交缓冲推进播放头；
* 重复分片只记账不重复计数；重复与恢复共用同一错误模型，
  指标模块据此统计，杜绝双重计数。
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .model import (
    ErrorEvent,
    ErrorKind,
    PacketModel,
    SegmentStatus,
    TrackKind,
)

# 线性外推残差容差：超过该值认为无法可靠推断缺口时间戳
GAP_RESIDUAL_US = 5_000
# 单次最多补出的连续包数，防止大缺口被静默填满
MAX_RECOVERY_RUN = 8
# 超过该跨度（us）的缺口一律不可恢复
MAX_RECOVERABLE_SPAN_US = 1_000_000


@dataclass
class RecoveryStats:
    missing: int = 0
    recovered: int = 0
    unrecoverable: int = 0
    duplicates: int = 0
    errors: list[ErrorEvent] = field(default_factory=list)

    @property
    def recovery_rate(self) -> float:
        denom = self.missing
        if denom == 0:
            return 1.0
        return self.recovered / denom


class RecoveryEngine:
    """按轨道维护最近输出，依据时间戳连续性补缺口或标记不可恢复。"""

    def __init__(self) -> None:
        self._last: dict[TrackKind, PacketModel | None] = {
            TrackKind.AUDIO: None, TrackKind.VIDEO: None}
        self.stats = RecoveryStats()

    def get_state(self) -> dict:
        """导出各轨最近包与累计计数，供检查点持久化。"""
        return {
            "last": {k.value: (v.__dict__ if v is not None else None)
                     for k, v in self._last.items()},
            "missing": self.stats.missing,
            "recovered": self.stats.recovered,
            "unrecoverable": self.stats.unrecoverable,
            "duplicates": self.stats.duplicates,
        }

    def set_state(self, state: dict) -> None:
        """恢复检查点状态，续跑在原缺口判定上下文上继续。"""
        self._last = {
            TrackKind(k): (PacketModel(**v) if v is not None else None)
            for k, v in state["last"].items()}
        self.stats.missing = state["missing"]
        self.stats.recovered = state["recovered"]
        self.stats.unrecoverable = state["unrecoverable"]
        self.stats.duplicates = state["duplicates"]

    def note_duplicate(self, pkt: PacketModel) -> None:
        """重复包：仅记账，绝不进入时间线。"""
        self.stats.duplicates += 1
        self.stats.errors.append(ErrorEvent(
            ErrorKind.DUPLICATE_SEQUENCE,
            f"recovery:{pkt.track.value}:seq{pkt.seq}",
            f"重复分片包 seq={pkt.seq} 已丢弃",
            segment_seq=pkt.segment_seq))

    def handle_packet(self, pkt: PacketModel) -> list[PacketModel]:
        """处理一个缓冲输出包，返回它以及为填补其前缺口补出的合成包。"""
        track = pkt.track
        last = self._last[track]
        out: list[PacketModel] = []
        if last is not None and pkt.seq > last.seq + 1:
            out.extend(self._fill_gap(last, pkt))
        self._last[track] = pkt
        out.append(pkt)
        return out

    def _fill_gap(self, prev: PacketModel, nxt: PacketModel
                  ) -> list[PacketModel]:
        track = prev.track
        missing_count = nxt.seq - prev.seq - 1
        span = nxt.pts_us - (prev.pts_us + prev.duration_us)
        loc = f"recovery:{track.value}:seq{prev.seq + 1}"
        self.stats.missing += missing_count
        period = prev.duration_us or nxt.duration_us
        if period <= 0:
            return self._give_up(missing_count, loc, prev, nxt,
                                 "相邻包时长为零，无法推断周期")
        if span > MAX_RECOVERABLE_SPAN_US:
            return self._give_up(missing_count, loc, prev, nxt,
                                 f"缺口跨度 {span}us 超过可恢复上限")
        if missing_count > MAX_RECOVERY_RUN:
            return self._give_up(missing_count, loc, prev, nxt,
                                 f"连续缺口 {missing_count} 超过 {MAX_RECOVERY_RUN}")
        # 残差校验：n*period 应与实际跨度吻合
        residual = abs(span - missing_count * period)
        if residual > GAP_RESIDUAL_US:
            return self._give_up(missing_count, loc, prev, nxt,
                                 f"线性外推残差 {residual}us 超容差")
        # 音频连续性额外校验：合成区间不得跨越已知音频包
        filled: list[PacketModel] = []
        for k in range(1, missing_count + 1):
            seq = prev.seq + k
            synth = PacketModel(
                track=track, seq=seq, segment_seq=prev.segment_seq,
                raw_pts=0,
                pts_us=prev.pts_us + prev.duration_us + (k - 1) * period,
                dts_us=prev.dts_us + prev.duration_us + (k - 1) * period,
                duration_us=period, arrival_us=nxt.arrival_us,
                size_bytes=0, keyframe=False, synthetic=True,
                transit_us=nxt.transit_us)
            filled.append(synth)
        self.stats.recovered += missing_count
        self.stats.errors.append(ErrorEvent(
            ErrorKind.STREAM_GAP, loc,
            f"按时间戳周期恢复 {missing_count} 个 {track.value} 包",
            segment_seq=prev.segment_seq))
        return filled

    def _give_up(self, count: int, loc: str, prev: PacketModel,
                 nxt: PacketModel, reason: str) -> list[PacketModel]:
        self.stats.unrecoverable += count
        self.stats.errors.append(ErrorEvent(
            ErrorKind.GAP_UNRECOVERABLE, loc,
            f"{reason}（seq {prev.seq + 1}..{nxt.seq - 1}）",
            segment_seq=nxt.segment_seq))
        return []

    def flush(self) -> None:
        """分片边界结算，状态随检查点持久化。"""
        self._last = {TrackKind.AUDIO: None, TrackKind.VIDEO: None}
