"""缓冲与分段下载模拟器。

下载以固定小时间块（默认 0.1s）推进，每个时间块从轨迹读取瞬时带宽，
因此同一段下载可以跨越带宽阶跃、抖动与突发零带宽。丢包通过重传额外
字节 + 重试延迟建模，传播延迟在段首计入。缓冲消耗、增长、下溢与
恢复在同一时间轴上逐块结算。
"""

from __future__ import annotations

import math
from dataclasses import dataclass

from .trace import Trace
from .types import (
    BitrateLevel,
    PlaybackState,
    SimulationError,
    ThroughputSample,
)


@dataclass(frozen=True, slots=True)
class BufferConfig:
    """缓冲模拟器参数。"""

    segment_duration_s: float = 2.0
    target_buffer_s: float = 10.0
    underflow_threshold_s: float = 3.0
    max_buffer_s: float = 30.0
    chunk_seconds: float = 0.1          # 下载推进时间块
    max_stall_seconds: float = 12.0     # 连续零带宽容忍上限，超过判下载失败
    max_retries_per_chunk: int = 3

    def validate(self) -> None:
        if self.segment_duration_s <= 0:
            raise SimulationError("segment_duration_s 必须为正数")
        if not (0.0 < self.underflow_threshold_s <= self.target_buffer_s):
            raise SimulationError("需满足 0 < underflow_threshold <= target_buffer")
        if self.target_buffer_s > self.max_buffer_s:
            raise SimulationError("target_buffer 不能大于 max_buffer")
        if self.chunk_seconds <= 0 or self.max_stall_seconds <= 0:
            raise SimulationError("chunk_seconds / max_stall_seconds 必须为正数")


class BufferSimulator:
    """维护缓冲水位与播放时钟，逐段模拟下载。"""

    def __init__(self, trace: Trace, config: BufferConfig | None = None):
        self._cfg = config or BufferConfig()
        self._cfg.validate()
        self._trace = trace
        self.buffer_s = self._cfg.target_buffer_s  # 起播缓冲即为目标水位
        self.playback_time_s = 0.0
        self.wall_time_s = 0.0
        self.total_underflow_s = 0.0
        self.protection_time_s = 0.0
        self.in_protection = False

    # ------------------------------------------------------------------
    def playback_state(self) -> PlaybackState:
        return PlaybackState(
            buffer_seconds=self.buffer_s,
            target_buffer_s=self._cfg.target_buffer_s,
            underflow_threshold_s=self._cfg.underflow_threshold_s,
            in_protection=self.in_protection,
            playback_time_s=self.playback_time_s,
            total_underflow_s=self.total_underflow_s,
            protection_time_s=self.protection_time_s,
        )

    def download_segment(self, index: int, level: BitrateLevel):
        """下载一个分段，返回 (样本, 下载耗时, 下溢时长, 失败信息)。"""
        cfg = self._cfg
        need_bytes = level.segment_bytes(cfg.segment_duration_s)
        if need_bytes <= 0:
            raise SimulationError(f"档位 {level.index} 分段字节数非正，码率配置非法")

        got_bytes = 0
        sent_bytes = 0
        underflow_s = 0.0
        stall_s = 0.0
        loss_accum = 0.0
        loss_weight = 0
        failure: str | None = None
        start_wall = self.wall_time_s

        # 段首传播延迟（RTT 近似，毫秒转秒），期间播放继续消耗缓冲
        first = self._safe_sample(self.wall_time_s)
        rtt_s = 2.0 * first.latency_ms / 1000.0
        underflow_s += self._consume(rtt_s)
        self.wall_time_s += rtt_s

        while got_bytes < need_bytes:
            sample = self._safe_sample(self.wall_time_s)
            chunk_bw = sample.bandwidth_kbps  # kbit/s
            loss_accum += sample.loss_rate
            loss_weight += 1

            if chunk_bw <= 0.0:
                # 零带宽：不传输，播放仍在消耗；累计连续停滞
                stall_s += cfg.chunk_seconds
                underflow_s += self._consume(cfg.chunk_seconds)
                self.wall_time_s += cfg.chunk_seconds
                if stall_s >= cfg.max_stall_seconds:
                    failure = (
                        f"第 {index} 段连续零带宽 {stall_s:.1f}s 超过容忍上限 "
                        f"{cfg.max_stall_seconds:.1f}s，下载失败"
                    )
                    break
                continue
            stall_s = 0.0

            # 该时间块物理层信道容量（kbit/s -> byte/s），也是线上流量上限：
            # 无论内部重传几次，一个时间块最多发满 cap，重传挤占的是有效交付
            cap_bytes = chunk_bw * 1000.0 / 8.0 * cfg.chunk_seconds
            remaining = need_bytes - got_bytes
            delivered = self._send_with_retries(cap_bytes, remaining, sample.loss_rate)
            got_bytes += delivered
            sent_bytes += int(round(min(cap_bytes, max(remaining, 0.0))))
            underflow_s += self._consume(cfg.chunk_seconds)
            self.wall_time_s += cfg.chunk_seconds

        download_s = self.wall_time_s - start_wall
        if failure is None:
            # 下载成功：分段入播放缓冲，封顶 max_buffer
            self.buffer_s = min(
                self.buffer_s + cfg.segment_duration_s, cfg.max_buffer_s
            )
        self._update_protection(download_s, underflow_s > 0.0)

        avg_loss = loss_accum / loss_weight if loss_weight else 0.0
        # 吞吐分母剔除段首 RTT（传播延迟不承载数据，否则系统性低估带宽）
        transfer_s = max(download_s - rtt_s, cfg.chunk_seconds)
        # 有效吞吐 goodput = 成功交付的 payload / 传输时间；丢包/重传通过
        # 拉长下载时间与减少交付自然压低 goodput，这是 ABR 决策关心的量
        throughput = self._throughput(got_bytes, transfer_s, index, avg_loss,
                                      wire_bytes=sent_bytes)
        return throughput, download_s, underflow_s, failure

    # ------------------------------------------------------------------
    def _send_with_retries(self, cap_bytes: float, remaining: float, loss_rate: float):
        """模拟一个时间块内的丢包重传，返回本块成功交付的字节数。

        字节按整数处理以消除浮点残留：首轮发送 min(cap, 待发)，成功部分
        按 (1-loss) 交付，丢失部分在同一块内重传，重传轮数受
        max_retries_per_chunk 约束（重传挤占同块信道，不额外增加线上流量）。
        重传耗尽仍未交付的字节永久丢失，需由后续时间块重新传输——因此高
        丢包率通过拉长整体下载时间压低有效吞吐，而不是在块内被兜底补回。
        每块至少交付 1 字节（信道非零时），保证下载循环必然终止。
        """
        success = 1.0 - loss_rate
        pending = int(min(cap_bytes, remaining))
        if pending <= 0:
            return 0.0
        delivered = 0
        for _ in range(self._cfg.max_retries_per_chunk + 1):
            if pending <= 0:
                break
            ok = int(round(pending * success))
            delivered += ok
            pending -= ok
        # 信道非零时保证最小进度，规避极端丢包 + 取整导致的零交付死循环
        if delivered <= 0:
            delivered = 1
        return float(delivered)

    def _consume(self, elapsed_s: float) -> float:
        """在一个时间窗内推进播放，返回该窗内缓冲耗尽（真实下溢）的时长。

        缓冲够撑满整窗时全部消耗、零下溢；缓冲在窗中途耗尽时，
        播放时钟只走到耗尽点，剩余时间计为下溢。
        """
        if self.buffer_s >= elapsed_s:
            self.buffer_s -= elapsed_s
            self.playback_time_s += elapsed_s
            return 0.0
        underflow = elapsed_s - self.buffer_s
        self.playback_time_s += self.buffer_s
        self.buffer_s = 0.0
        self.total_underflow_s += underflow
        return underflow

    def _update_protection(self, elapsed_s: float, had_underflow: bool) -> None:
        """根据缓冲水位与下溢事件维护保护状态及保护时长累计。"""
        if had_underflow or self.buffer_s < self._cfg.underflow_threshold_s:
            # 下载过程中已发生下溢，或结束时水位仍低于阈值：进入/保持保护
            self.in_protection = True
            self.protection_time_s += elapsed_s
        elif self.in_protection and self.buffer_s >= self._cfg.target_buffer_s:
            # 仅当重新爬回目标水位才解除保护（滞后恢复）
            self.in_protection = False

    def _throughput(self, delivered_bytes: int, transfer_s: float, index: int,
                    loss: float, wire_bytes: int):
        if transfer_s <= 0.0 or not math.isfinite(transfer_s):
            raise SimulationError(f"第 {index} 段下载耗时非法: {transfer_s!r}")
        kbps = delivered_bytes * 8.0 / 1000.0 / transfer_s
        if not math.isfinite(kbps) or kbps < 0.0:
            raise SimulationError(f"第 {index} 段计算出非法吞吐 {kbps!r}")
        return ThroughputSample(
            segment_index=index,
            throughput_kbps=kbps,
            duration_s=transfer_s,
            bytes_sent=int(wire_bytes),
            loss_rate=loss,
        )

    def _safe_sample(self, at_s: float):
        try:
            return self._trace.sample_at(at_s)
        except Exception as exc:  # 轨迹耗尽等统一包装为可诊断模拟错误
            raise SimulationError(f"分段下载读取轨迹失败（t={at_s:.3f}s）: {exc}") from exc
