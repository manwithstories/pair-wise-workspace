"""端到端流水线编排：清单 -> 解复用 -> 时钟 -> 缓冲 -> 恢复 -> 同步。

处理按分片顺序推进，每 ``chunk_size`` 个分片在一个 SQLite 事务里
落一次检查点；检查点贯穿全阶段，中断后凭输入摘要/配置/进度三重
校验从最后一个已提交分块续跑，已计数的分片不会重复进入指标。

指标模块与处理阶段共享同一 :class:`PipelineCounters` 与错误模型，
保证终端摘要与内部计数口径完全一致。
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field

from .buffer import JitterBuffer
from .checkpoint import CheckpointError, CheckpointStore, RunConfig
from .clock import ClockDomain
from .generator import build_plan
from .ingest import (
    IngestStats,
    NetworkConditioner,
    NetworkProfile,
    demux_segment,
    unwrap_packets,
)
from .manifest import ManifestModel, parse_manifest
from .metrics import PipelineCounters
from .model import (
    ErrorEvent,
    ErrorKind,
    MediaClock,
    PacketModel,
    SegmentStatus,
    TrackKind,
    now_us,
)
from .recovery import RecoveryEngine
from .sync import SyncAnalyzer


@dataclass
class PipelineConfig:
    manifest_path: str
    index_path: str
    chunk_size: int = 16
    resume: bool = True
    scenario: str = "normal"
    seed: int = 13812


def _profile_for(scenario: str, plan) -> NetworkProfile:
    """场景计划 -> 传输注入参数；wrap 作用于音频轨。"""
    extra = plan.extra
    return NetworkProfile(
        seed=plan.seed,
        reorder_rate=extra.get("reorder_rate", 0.0),
        loss_rate=extra.get("loss_rate", 0.0),
        burst_loss_max=extra.get("burst_loss_max", 0),
        duplicate_rate=0.08 if scenario in ("reorder", "loss") else 0.0,
        drift_ppm=extra.get("drift_ppm", 0.0),
        jitter_us=8000 if scenario in ("reorder", "drift") else 2000,
        wrap_track=TrackKind.AUDIO if "wrap_at_packet" in extra else None,
        wrap_at_packet=extra.get("wrap_at_packet", -1),
    )


class Pipeline:
    def __init__(self, config: PipelineConfig) -> None:
        self.config = config
        self.manifest: ManifestModel | None = None
        self.store: CheckpointStore | None = None
        self.counters = PipelineCounters()
        self.clocks: dict[TrackKind, MediaClock] = {}
        self.domain = ClockDomain()
        self.buffer = JitterBuffer()
        self.recovery = RecoveryEngine()
        self.ingest_stats = IngestStats()
        self.sync = SyncAnalyzer(self.domain)
        self._conditioner: NetworkConditioner | None = None
        self.resumed = False
        # 每轨跨分片全局包序号：分片内 seq 会从 0 重置，缓冲去重必须
        # 基于全局序号，否则后续分片的低序号包会被误判为重复
        self._global_seq: dict[TrackKind, int] = {
            TrackKind.AUDIO: 0, TrackKind.VIDEO: 0}
        self._wrap_tracks: set[TrackKind] = set()

    # ---- 生命周期 --------------------------------------------------
    def open(self) -> None:
        self.manifest = parse_manifest(self.config.manifest_path)
        plan = build_plan(self.config.scenario, self.config.seed)
        profile = _profile_for(self.config.scenario, plan)
        self._conditioner = NetworkConditioner(profile)
        if profile.wrap_track is not None:
            self._wrap_tracks.add(profile.wrap_track)
        self.store = CheckpointStore(self.config.index_path)
        run_cfg = RunConfig(
            self.config.manifest_path, self.config.chunk_size,
            plan.seed, self.manifest.input_digest)
        is_new = self.store.initialize_run(run_cfg)
        self.resumed = not is_new
        if self.resumed:
            self._restore_state()

    def _restore_state(self) -> None:
        """续跑：载入历史计数器与错误，已提交分片不再处理。"""
        assert self.store is not None
        merged = self.store.load_counters()
        self.counters.packets_total = merged.get("packets_total", 0)
        self.counters.packets_lost = merged.get("packets_lost", 0)
        self.counters.packets_recovered = merged.get("packets_recovered", 0)
        self.counters.duplicates = merged.get("duplicates", 0)
        self.counters.late_drops = merged.get("late_drops", 0)
        self.counters.rejected_segments = merged.get("rejected_segments", 0)
        self.counters.wrap_events = merged.get("wrap_events", 0)
        self.counters.jump_events = merged.get("jump_events", 0)
        self.counters.errors = self.store.load_errors()

    def close(self) -> None:
        if self.store is not None:
            self.store.close()

    # ---- 主循环 ----------------------------------------------------
    def run(self) -> dict:
        assert self.manifest is not None and self.store is not None
        start_seq = self.store.resume_from() + 1
        # 按分片序号去重：同一 seq 的音/视频引用归并为一个处理单元
        seqs = sorted({r.seq for r in self.manifest.refs})
        pending = [s for s in seqs if s >= start_seq]
        # 全量续跑：所有分片均已提交，直接还原首次完整摘要，保证两次
        # 运行结果逐项一致，不会因内存样本清空而把 P95 等指标归零。
        if not pending:
            stored = self.store.load_summary()
            if stored is not None:
                stored["checkpoint_resumed"] = True
                return stored
        chunk_rows: list[tuple] = []
        chunk_errors: list[ErrorEvent] = []
        chunk_counters = self._zero_counters()
        processed = 0
        for seq in pending:
            refs = [r for r in self.manifest.refs if r.seq == seq]
            row = self._process_segment(seq, refs, chunk_errors,
                                        chunk_counters)
            chunk_rows.append(row)
            processed += 1
            if processed % self.config.chunk_size == 0:
                self._commit_chunk(seq, chunk_counters, chunk_rows,
                                   chunk_errors)
                chunk_rows, chunk_errors = [], []
                chunk_counters = self._zero_counters()
        if chunk_rows:
            self._commit_chunk(seqs[-1] if seqs else start_seq - 1,
                               chunk_counters, chunk_rows, chunk_errors)
        self.domain.estimate()
        summary = self._summary()
        self.store.save_summary(summary)
        return summary

    # ---- 单分片处理 ------------------------------------------------
    def _process_segment(self, seq: int, refs, chunk_errors, cc) -> tuple:
        assert self.manifest is not None and self._conditioner is not None
        seg_errors: list[ErrorEvent] = []
        segment_packets = 0
        checksum = 0
        for ref in refs:
            if ref.rejected:
                cc["rejected_segments"] += 1
                self.counters.rejected_segments += 1
                continue
            path = os.path.join(self.manifest.base_dir, ref.uri)
            # 独立容器不解跨容器时钟；原始刻度保留给注入层回绕解算
            raw = demux_segment(path, seq, None, self.ingest_stats)
            if not raw:
                # 损坏/空轨：demux 已记录错误，分片标记拒绝
                cc["rejected_segments"] += 1
                self.counters.rejected_segments += 1
                continue
            injected = self._conditioner.apply(raw, self.ingest_stats)
            if self._wrap_tracks:
                # 仅回绕场景需要时钟解算（注入改写了 raw_pts）；
                # 普通场景 demux 的 pts_us 已是容器保留的连续时间戳
                resolved = unwrap_packets(
                    injected, self.clocks, self.ingest_stats,
                    {TrackKind.AUDIO: 1000, TrackKind.VIDEO: 90000})
            else:
                resolved = injected
            # 全局序号必须保留丢包造成的空洞：以丢包前的原始 demux 包
            # 数确定各轨分片基准，幸存包按原始 seq 加基准映射，缺失包的
            # 序号自然空缺，供缓冲/恢复识别缺口而非重新连续编号。
            base_by_track = {}
            for p in raw:
                if p.track not in base_by_track:
                    base_by_track[p.track] = self._global_seq[p.track]
            for pkt in resolved:
                global_seq = base_by_track[pkt.track] + pkt.seq
                pkt = PacketModel(
                    track=pkt.track, seq=global_seq,
                    segment_seq=pkt.segment_seq, raw_pts=pkt.raw_pts,
                    pts_us=pkt.pts_us, dts_us=pkt.dts_us,
                    duration_us=pkt.duration_us, arrival_us=pkt.arrival_us,
                    size_bytes=pkt.size_bytes, keyframe=pkt.keyframe,
                    synthetic=pkt.synthetic, transit_us=pkt.transit_us)
                self._route_packet(pkt, cc, seg_errors)
                segment_packets += 1
                checksum = ((checksum ^ (pkt.pts_us & 0xFF))
                            * 1099511628211) & 0xFFFFFFFFFFFFFFFF
            # 推进各轨全局基准到下一分片（按丢包前的原始包数，含空洞）
            for track, base in base_by_track.items():
                orig_count = sum(1 for p in raw if p.track == track)
                self._global_seq[track] = base + orig_count
            if checksum >= 1 << 63:
                checksum -= 1 << 64
        # 同步评估本分片已恢复时间线
        self._drain_and_sync(cc, seg_errors)
        chunk_errors.extend(seg_errors)
        return (seq, SegmentStatus.RECEIVED.value, segment_packets,
                cc["recovered_in_chunk"], checksum)

    def _route_packet(self, pkt, cc, seg_errors) -> None:
        status = self.buffer.push(pkt)
        cc["packets_total"] += 1
        self.counters.packets_total += 1
        if status == SegmentStatus.DUPLICATE:
            self.recovery.note_duplicate(pkt)
            cc["duplicates"] += 1
            self.counters.duplicates += 1
        elif status == SegmentStatus.MISSING:
            cc["late_drops"] += 1
            self.counters.late_drops += 1

    def _drain_and_sync(self, cc, seg_errors) -> None:
        # drain 越过确定缺口后，后续包带着序号跳跃输出，recovery 的
        # handle_packet 检测跳跃并按周期残差补合成包；无法可靠推断时
        # 标记不可恢复，全程只在 recovery 一处计数，杜绝重复。
        timeline: list = []
        for track in (TrackKind.AUDIO, TrackKind.VIDEO):
            for pkt in self.buffer.drain(track):
                timeline.extend(self._recover_and_anchor(track, pkt, cc))
        self.sync.feed(timeline)

    def _recover_and_anchor(self, track, pkt, cc) -> list:
        recovered = self.recovery.handle_packet(pkt)
        out = []
        for rp in recovered:
            self.domain.anchor(track, rp.pts_us, rp.arrival_us)
            cc["latency"].append(rp.transit_us)
            if rp.synthetic:
                cc["recovered_in_chunk"] += 1
                self.counters.packets_recovered += 1
            out.append(rp)
        return out

    # ---- 检查点 ----------------------------------------------------
    def _commit_chunk(self, last_seq, cc, rows, errors) -> None:
        assert self.store is not None
        # drain 已自动越过确定缺口；缓冲中剩余的是仍在等待乱序/重传的
        # 合法 pending 包，不在此强制跳过，避免把可恢复等待误判为丢失。
        self.counters.packets_lost += self.ingest_stats.lost
        cc["packets_lost"] = self.ingest_stats.lost
        cc["wrap_events"] = self.ingest_stats.wrap_events
        cc["jump_events"] = self.ingest_stats.jump_events
        self.counters.wrap_events = self.ingest_stats.wrap_events
        self.counters.jump_events = self.ingest_stats.jump_events
        self.counters.latency_samples_us.extend(cc["latency"])
        self.counters.errors.extend(errors)
        # 入库计数器剔除 latency 大数组（仅保留聚合量），避免膨胀
        persist_cc = {k: (v if not isinstance(v, list) else len(v))
                      for k, v in cc.items()}
        chunk_id = last_seq // max(1, self.config.chunk_size)
        self.store.commit_chunk(chunk_id, last_seq, persist_cc, rows, errors)

    @staticmethod
    def _zero_counters() -> dict:
        return {"packets_total": 0, "packets_lost": 0,
                "packets_recovered": 0, "recovered_in_chunk": 0,
                "duplicates": 0, "late_drops": 0,
                "rejected_segments": 0, "wrap_events": 0,
                "jump_events": 0, "latency": []}

    def _summary(self) -> dict:
        from .metrics import build_summary
        # 同步恢复引擎口径，保证恢复率分母与实际缺口判定一致
        self.counters.gap_missing = self.recovery.stats.missing
        self.counters.gap_unrecoverable = self.recovery.stats.unrecoverable
        report = self.sync.finalize()
        return build_summary(self.counters, report, self.resumed)


def run_pipeline(config: PipelineConfig) -> dict:
    pipe = Pipeline(config)
    try:
        pipe.open()
        return pipe.run()
    finally:
        pipe.close()
