"""端到端流水线编排。

数据流：合成源 → PyAV 编码 → 打包/故障注入 → 包修复 → 离屏解码 →
时间线+音频处理 → 分块合成渲染（检查点）→ 清单与统计。
"""

from __future__ import annotations

import hashlib
import json
import shutil
from dataclasses import dataclass, field
from pathlib import Path

import numpy as np

from . import audio as audio_mod
from .checkpoint import CheckpointStore, ensure_disk_space
from .manifest import build_manifest, write_manifest
from .packet_repair import repair_packets
from .render import (
    composite_frame,
    decode_frames,
    encode_source,
    pack_frames,
    psnr_db,
    source_frame,
    unpack_frames,
)
from .simulate import FaultProfile, inject_faults, packetize, sample_timeline
from .timebase import TimeBase
from .timeline import Timeline


class PipelineInterrupted(Exception):
    """模拟中断：已提交块保留，可由后续运行复用。"""


@dataclass
class PipelineConfig:
    out_dir: Path
    task_id: str = "demo"
    seed: int = 41
    fps: int = 24
    size: tuple[int, int] = (320, 180)
    duration_s: float = 12.0
    chunk_frames: int = 48
    bit_rate: int = 400_000
    fault: FaultProfile = field(default_factory=FaultProfile)
    force_rebuild: bool = False
    interrupt_after_chunks: int | None = None
    audio_offset_ticks: int = 0


@dataclass
class PipelineResult:
    stats: dict
    manifest_path: Path
    reused_blocks: int


def _input_digest(timeline: Timeline, config: PipelineConfig) -> str:
    payload = {
        "clips": [c.__dict__ for c in timeline.clips],
        "duration": timeline.duration,
        "fps": config.fps,
        "size": list(config.size),
        "seed": config.seed,
        "bit_rate": config.bit_rate,
    }
    return hashlib.sha256(
        json.dumps(payload, sort_keys=True).encode()
    ).hexdigest()


def run_pipeline(config: PipelineConfig,
                 timeline: Timeline | None = None) -> PipelineResult:
    tb = TimeBase()
    out_dir = Path(config.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    ensure_disk_space(out_dir, 32 * 1024 * 1024)
    work_dir = out_dir / f".work-{config.task_id}"
    shutil.rmtree(work_dir, ignore_errors=True)
    work_dir.mkdir(parents=True)

    if timeline is None:
        timeline = sample_timeline(tb, config.duration_s)
    timeline.validate()

    rng = np.random.default_rng(config.seed)
    frame_ticks = tb.frame_ticks(config.fps)
    total_frames = int(round(config.duration_s * config.fps))
    width, height = config.size

    # 1) 编码各源素材
    source_ids = sorted(timeline.source_ranges())
    encoded: dict[str, Path] = {}
    for seg_id, sid in enumerate(source_ids):
        path = work_dir / f"src-{seg_id}.mp4"
        encode_source(path, sid, total_frames, config.fps, config.size, tb,
                      config.bit_rate)
        encoded[sid] = path

    # 2) 打包 → 故障注入 → 修复
    all_packets = []
    expected: dict[int, int] = {}
    seq = 0
    seg_of: dict[str, int] = {}
    for seg_id, sid in enumerate(source_ids):
        data = encoded[sid].read_bytes()
        pkts = packetize(seg_id, data, seq, 0)
        expected[seg_id] = len(pkts)
        seg_of[sid] = seg_id
        seq += len(pkts)
        all_packets.extend(pkts)
    stream = inject_faults(all_packets, config.fault, rng)
    repaired, report = repair_packets(stream, expected)

    # 3) 重组完整段并解码；不完整段标记缺失，由合成策略处理
    complete_segments = set(expected) - {r[0] for r in report.missing_ranges}
    sources: dict[str, dict] = {}
    alignment_err_ms = 0.0
    for sid in source_ids:
        seg_id = seg_of[sid]
        if seg_id not in complete_segments:
            continue
        blob = b"".join(
            p.payload for p in repaired if p.segment_id == seg_id
        )
        repaired_path = work_dir / f"repaired-{seg_id}.mp4"
        repaired_path.write_bytes(blob)
        frames = decode_frames(repaired_path, total_frames, tb, config.fps)
        sources[sid] = {f.pts_ticks: f for f in frames}
        for f in frames:
            err = abs(f.pts_ticks - f.index * frame_ticks)
            alignment_err_ms = max(alignment_err_ms, tb.ticks_to_ms(err))

    # 4) 音轨：合成、偏移、统计、落盘
    samples = audio_mod.synth_audio("main-audio", timeline.duration, tb)
    samples = audio_mod.apply_offset(samples, config.audio_offset_ticks, tb)
    a_stats = audio_mod.audio_stats(samples)
    audio_mod.write_wav(out_dir / f"audio-{config.task_id}.wav", samples)

    # 5) 分块合成渲染（检查点可恢复）
    store = CheckpointStore(
        out_dir, config.task_id, _input_digest(timeline, config),
        force_rebuild=config.force_rebuild,
    )
    chunk_ranges = [
        (s, min(s + config.chunk_frames, total_frames))
        for s in range(0, total_frames, config.chunk_frames)
    ]
    reused = 0
    psnr_values: list[float] = []
    frame_records: list[dict] = []
    chunk_metas: list[dict] = []
    for idx, (f0, f1) in enumerate(chunk_ranges):
        chunk_digest = hashlib.sha256(
            json.dumps({
                "range": [f0, f1],
                "repair": report.to_dict(),
                "sources": sorted(sources),
            }, sort_keys=True).encode()
        ).hexdigest()
        if store.is_committed(idx, chunk_digest):
            reused += 1
            frames = unpack_frames(store.chunk_path(idx).read_bytes())
            meta = dict(store.chunks[idx], index=idx)
        else:
            frames = []
            for fi in range(f0, f1):
                ticks = fi * frame_ticks
                img, _notes = composite_frame(timeline, ticks, sources,
                                              config.size)
                frames.append(img)
            meta = dict(
                store.commit_chunk(idx, f0, f1, chunk_digest,
                                   pack_frames(frames)),
                index=idx,
            )
        chunk_metas.append(meta)
        for off, img in enumerate(frames):
            fi = f0 + off
            frame_records.append({
                "index": fi,
                "pts_ticks": fi * frame_ticks,
                "sha256": hashlib.sha256(img.tobytes()).hexdigest(),
            })
            ref, _ = _reference_frame(timeline, fi * frame_ticks, tb,
                                      config.size)
            psnr_values.append(psnr_db(img, ref))
        if config.interrupt_after_chunks is not None and \
                idx + 1 >= config.interrupt_after_chunks:
            raise PipelineInterrupted(
                f"模拟中断：已提交 {idx + 1}/{len(chunk_ranges)} 块"
            )

    # 6) 码率与统计、清单
    total_bytes = sum(p.stat().st_size for p in encoded.values())
    actual_bps = total_bytes * 8 / config.duration_s / max(len(source_ids), 1)
    deviation = abs(actual_bps - config.bit_rate) / config.bit_rate * 100.0
    stats = {
        **report.to_dict(),
        "bitrate_target_bps": config.bit_rate,
        "bitrate_actual_bps": round(actual_bps, 1),
        "bitrate_deviation_pct": round(deviation, 3),
        "frame_count": total_frames,
        "frame_psnr_mean_db": round(float(np.mean(psnr_values)), 3),
        "audio_peak": round(a_stats["peak"], 6),
        "audio_rms": round(a_stats["rms"], 6),
        "audio_silence_intervals_ms": a_stats["silence_intervals_ms"],
        "manifest_alignment_error_ms": round(alignment_err_ms, 6),
        "checkpoint_reused_blocks": reused,
    }
    manifest = build_manifest(
        config.task_id, tb.ticks_per_second, config.fps,
        chunk_metas, frame_records, stats,
    )
    manifest_path = out_dir / "manifest.json"
    write_manifest(manifest_path, manifest)
    shutil.rmtree(work_dir, ignore_errors=True)
    return PipelineResult(stats, manifest_path, reused)


def _reference_frame(timeline: Timeline, ticks: int, tb: TimeBase,
                     size: tuple[int, int]):
    """无传输损耗的参考合成帧（PSNR 基准）。"""
    width, height = size
    layers = timeline.composition_at(ticks)
    if not layers:
        return np.zeros((height, width, 3), dtype=np.uint8), []
    acc = np.zeros((height, width, 3), dtype=np.float64)
    for layer in layers:
        acc += source_frame(layer.source_id, layer.source_ticks, tb,
                            width, height).astype(np.float64) * layer.weight
    return np.clip(acc, 0, 255).astype(np.uint8), []
