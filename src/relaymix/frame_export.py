"""固定采样窗口的离屏帧序列导出。

* 主时间线按 frame_size 切成等长窗口，末尾允许不完整窗口；
* 每个窗口经 PyAV 以 pcm_s16le 编码为独立 WAV 片段，pts 使用整数
  tick、time_base = 1/master_rate，时间戳与检查点记录一致；
* 片段字节 SHA-256、输入块哈希聚合、采样区间与参数指纹随原子提交落库；
* 内置确定性中断注入（环境变量），用于演示与测试“写到一半进程退出”：
    RELAYMIX_CRASH_AFTER=N   成功提交 N 块后 os._exit
    RELAYMIX_CRASH_PHASE=write|index  在第 RELAYMIX_CRASH_CHUNK 块的
        临时产物阶段 / 索引事务阶段退出
"""

from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path

import av
import numpy as np

from .checkpoint import CheckpointStore, ChunkRecord
from .errors import CheckpointError
from .timebase import TimeBase, quantize_int16
from .timeline import Timeline, parameter_fingerprint

SEGMENT_PREFIX = "chunk_"
SEGMENT_SUFFIX = ".wav"
SIDECAR_SUFFIX = ".json"
CRASH_EXIT_CODE = 130


@dataclass(frozen=True)
class Window:
    index: int
    start_tick: int          # 主刻度，左闭右开
    end_tick: int
    frame_index: int         # 帧序号 = 窗口序号


def iter_windows(output_start: int, output_end: int, frame_size: int):
    """确定性窗口切分；末尾不足 frame_size 时产出不完整窗口。"""
    total = output_end - output_start
    count = (total + frame_size - 1) // frame_size
    for i in range(count):
        start = output_start + i * frame_size
        end = min(start + frame_size, output_end)
        yield Window(index=i, start_tick=start, end_tick=end, frame_index=i)


def segment_name(chunk_index: int) -> str:
    return f"{SEGMENT_PREFIX}{chunk_index:06d}{SEGMENT_SUFFIX}"


def input_hash_for_range(timeline: Timeline, start_tick: int, end_tick: int) -> str:
    """聚合覆盖区间的溯源信息：块哈希 + 裁剪后的采样区间 + 填充标记。"""
    entries = []
    for span in timeline.spans:
        lo = max(span.master_start, start_tick)
        hi = min(span.master_end, end_tick)
        if lo >= hi:
            continue
        entries.append(
            {
                "track": span.track,
                "block_id": span.block_id,
                "content_hash": span.content_hash,
                "range": [lo, hi],
                "filled": span.filled,
                "fill_reason": span.fill_reason,
            }
        )
    blob = json.dumps(entries, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(blob).hexdigest()


def encode_segment(path: Path, pcm_int16: np.ndarray, rate: int,
                   start_tick: int, channels: int) -> None:
    """PyAV 离屏编码：pcm_s16le / WAV，pts 为整数 tick。"""
    if pcm_int16.ndim == 1:
        pcm_int16 = pcm_int16[:, None]
    planar = np.ascontiguousarray(pcm_int16.T)  # (channels, samples) s16
    layout = av.AudioLayout(channels)
    container = av.open(str(path), "w", format="wav")
    try:
        stream = container.add_stream("pcm_s16le", rate=rate)
        stream.layout = layout
        stream.time_base = TimeBase(rate).fraction()
        frame = av.AudioFrame.from_ndarray(planar, format="s16p", layout=layout)
        frame.sample_rate = rate
        frame.pts = start_tick
        frame.time_base = stream.time_base
        for packet in stream.encode(frame):
            container.mux(packet)
        for packet in stream.encode(None):
            container.mux(packet)
    finally:
        container.close()


def _crash_point(chunk_index: int, phase: str) -> None:
    """按环境变量在确定性位置模拟进程退出（不跑 finally 之外的清理）。"""
    crash_phase = os.environ.get("RELAYMIX_CRASH_PHASE")
    crash_chunk = os.environ.get("RELAYMIX_CRASH_CHUNK")
    if crash_phase == phase and crash_chunk is not None and int(crash_chunk) == chunk_index:
        os._exit(CRASH_EXIT_CODE)


def export_timeline(timeline: Timeline, store: CheckpointStore, *,
                    algo_version: str, interrupt_after: int | None = None) -> dict:
    """逐窗口导出并原子提交；已提交窗口跳过，返回本次运行摘要。"""
    manifest = timeline.manifest
    params_fp = parameter_fingerprint(manifest)
    committed_before = store.committed_indices()
    windows = list(
        iter_windows(manifest.output_start, manifest.output_end, manifest.frame_size)
    )
    crash_after = os.environ.get("RELAYMIX_CRASH_AFTER")
    newly_committed: list[int] = []

    for win in windows:
        if win.index in committed_before:
            continue
        rel_lo = win.start_tick - manifest.output_start
        rel_hi = win.end_tick - manifest.output_start
        pcm = quantize_int16(timeline.buffer[rel_lo:rel_hi])

        name = segment_name(win.index)
        final_path = store.segments_dir / name
        tmp_path = store.segments_dir / f".tmp-{win.index:06d}-{name}"

        # 阶段 1：临时产物（崩溃只留下可覆盖的 .tmp，索引不可见）。
        _crash_point(win.index, "write")
        encode_segment(tmp_path, pcm, manifest.master_rate, win.start_tick,
                       manifest.output_channels)
        seg_hash = hashlib.sha256(tmp_path.read_bytes()).hexdigest()
        os.replace(tmp_path, final_path)

        sidecar = store.segments_dir / f"{name}{SIDECAR_SUFFIX}"
        sidecar_payload = {
            "chunk_index": win.index,
            "sample_range": [win.start_tick, win.end_tick],
            "frame_index": win.frame_index,
            "input_hash": input_hash_for_range(
                timeline, win.start_tick, win.end_tick
            ),
            "params_fingerprint": params_fp,
            "algo_version": algo_version,
            "spans": [
                s.__dict__
                for s in timeline.spans
                if s.master_end > win.start_tick and s.master_start < win.end_tick
            ],
        }
        sidecar.write_text(
            json.dumps(sidecar_payload, sort_keys=True, indent=2), encoding="utf-8"
        )

        # 阶段 2：索引原子事务（崩溃后该窗口无提交记录，恢复时重算）。
        _crash_point(win.index, "index")
        record = ChunkRecord(
            chunk_index=win.index,
            start_tick=win.start_tick,
            end_tick=win.end_tick,
            frame_index=win.frame_index,
            segment_file=name,
            segment_hash=seg_hash,
            input_hash=sidecar_payload["input_hash"],
            params_fingerprint=params_fp,
            algo_version=algo_version,
        )
        store.commit_chunk(record, final_path.read_bytes())
        newly_committed.append(win.index)

        if interrupt_after is not None and len(newly_committed) >= interrupt_after:
            # demo 中断：真实进程退出，SQLite 与片段均已落盘。
            os._exit(CRASH_EXIT_CODE)
        if crash_after is not None and len(newly_committed) >= int(crash_after):
            os._exit(CRASH_EXIT_CODE)

    if len(store.committed_indices()) == len(windows):
        store.mark_finished()
    return {
        "total_chunks": len(windows),
        "committed_now": newly_committed,
        "resumed_chunks": len(committed_before),
        "finished": store.is_finished(),
    }


def assert_fingerprints(store: CheckpointStore, expected_params: str,
                        expected_algo: str) -> None:
    """恢复后对已提交片段做指纹一致性核验（只读）。"""
    for rec in store.fetch_chunks():
        if rec.params_fingerprint != expected_params:
            raise CheckpointError(
                f"chunk {rec.chunk_index} params fingerprint mismatch",
                error_code="params_fingerprint_mismatch",
                chunk_index=rec.chunk_index,
                recompute_from_tick=rec.start_tick,
            )
        if rec.algo_version != expected_algo:
            raise CheckpointError(
                f"chunk {rec.chunk_index} algorithm version mismatch",
                error_code="algo_version_mismatch",
                chunk_index=rec.chunk_index,
                recompute_from_tick=rec.start_tick,
            )
