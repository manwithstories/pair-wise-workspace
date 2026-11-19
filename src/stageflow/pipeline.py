"""Pipeline orchestration: bounded queue, backpressure, cancel/resume.

Producer thread mixes delivery-rate blocks and writes each to a per-task
temp block file, then pushes the block index onto a bounded queue.  The
consumer (main thread) reads block files, feeds the frame renderer and
audio stats, and checkpoints progress.  A full queue blocks the producer —
that blocking *is* the backpressure; the waited time is recorded and
stalls beyond the threshold are reported to stderr.

Cancellation stops at the last completed block: readers are closed, the
partial block is discarded, and completed block files plus a checkpoint
remain for `--resume`.  Temp state lives in <out>/.work-<job>/ and is
removed after a successful run.
"""

from __future__ import annotations

import json
import queue
import shutil
import sys
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path

import numpy as np
import soundfile as sf

from .clock import MASTER_RATE
from .errors import InterruptedError_
from .ingest import TrackReader
from .mix import Mixer, MixStats
from .render import FrameRenderer
from .timeline import Timeline

SENTINEL = -1


@dataclass
class PipelineConfig:
    out_dir: Path
    target_bitrate: int = 4_000_000
    queue_blocks: int = 3
    block_frames: int = 4096  # delivery-rate frames per block
    consumer_delay_ms: float = 0.0
    stall_threshold_ms: float = 500.0
    on_corrupt: str = "fail"  # "fail" | "mute"
    resume: bool = False
    cancel_after_blocks: int | None = None  # test/demo hook


@dataclass
class PipelineResult:
    stats: MixStats
    renderer: FrameRenderer
    muted_ranges: list[dict[str, object]]
    backpressure_wait_ms: float
    stall_count: int
    blocks: int
    missing_blocks: int
    work_dir: Path
    resumed_from: int = 0


@dataclass
class _Shared:
    wait_ms: float = 0.0
    stall_count: int = 0
    cancelled: bool = False
    error: BaseException | None = None


def _block_path(work_dir: Path, index: int) -> Path:
    return work_dir / f"block_{index:06d}.f32"


def _checkpoint_path(work_dir: Path) -> Path:
    return work_dir / "checkpoint.json"


def run_pipeline(timeline: Timeline, cfg: PipelineConfig) -> PipelineResult:
    d = timeline.delivery
    total_samples = int(d.duration_ticks * d.sample_rate // MASTER_RATE)
    total_blocks = max(1, (total_samples + cfg.block_frames - 1) // cfg.block_frames)
    work_dir = cfg.out_dir / f".work-{timeline.job}"
    cfg.out_dir.mkdir(parents=True, exist_ok=True)

    completed = _load_checkpoint(work_dir, timeline, cfg) if cfg.resume else 0
    if completed == 0 and work_dir.exists():
        shutil.rmtree(work_dir)
    work_dir.mkdir(parents=True, exist_ok=True)

    readers = {
        t.id: TrackReader(t, timeline.resolve(t), on_corrupt=cfg.on_corrupt)
        for t in timeline.tracks
    }
    for r in readers.values():
        r.open()
    mixer = Mixer(timeline, readers)
    renderer = FrameRenderer(d, cfg.out_dir / f"{timeline.job}.mp4", cfg.target_bitrate)
    q: queue.Queue[int] = queue.Queue(maxsize=max(1, cfg.queue_blocks))
    shared = _Shared()
    cancel = threading.Event()

    def produce() -> None:
        try:
            for i in range(completed, total_blocks):
                if cancel.is_set():
                    break
                if cfg.cancel_after_blocks is not None and i >= completed + cfg.cancel_after_blocks:
                    cancel.set()
                    break
                start_tick = d.start_tick + i * cfg.block_frames * MASTER_RATE // d.sample_rate
                frames = min(cfg.block_frames, total_samples - i * cfg.block_frames)
                data = mixer.mix_block(int(start_tick), frames)
                data.astype("<f4").tofile(_block_path(work_dir, i))
                t0 = time.monotonic()
                q.put(i)  # bounded: blocks here while the consumer is behind
                waited = (time.monotonic() - t0) * 1000.0
                shared.wait_ms += waited
                if waited > cfg.stall_threshold_ms:
                    shared.stall_count += 1
                    print(f"stall: producer blocked {waited:.0f}ms at block {i}",
                          file=sys.stderr)
        except BaseException as e:  # deliver to consumer
            shared.error = e
        finally:
            q.put(SENTINEL)

    renderer.open()
    stats = MixStats()
    consumed = 0
    try:
        for i in range(completed):
            _consume(i, work_dir, d, renderer, stats, cfg)
            consumed += 1
        producer = threading.Thread(target=produce, name="stageflow-producer", daemon=True)
        producer.start()
        while True:
            idx = q.get()
            if idx == SENTINEL:
                break
            _consume(idx, work_dir, d, renderer, stats, cfg)
            consumed += 1
            _write_checkpoint(work_dir, timeline, cfg, consumed)
        producer.join()
        if shared.error is not None:
            raise shared.error
        if cancel.is_set() or consumed < total_blocks:
            _write_checkpoint(work_dir, timeline, cfg, consumed)
            raise InterruptedError_(
                f"cancelled after {consumed}/{total_blocks} blocks; "
                f"re-run with --resume to continue from block {consumed}"
            )
        renderer.flush(total_samples)
        renderer.close()
        _assemble_wav(work_dir, cfg.out_dir / f"{timeline.job}.wav", d, total_blocks,
                      cfg.block_frames, total_samples)
        shutil.rmtree(work_dir)
    except BaseException:
        renderer.close()
        raise
    finally:
        for r in readers.values():
            r.close()

    # Merge mix stats accumulated during production with consumption stats.
    stats.max_align_error_ticks = max(
        stats.max_align_error_ticks, mixer.stats.max_align_error_ticks
    )
    muted = [
        {"track": m.track_id, "start_sample": m.start_sample, "frames": m.frames,
         "reason": m.reason}
        for r in readers.values() for m in r.muted_ranges
    ]
    return PipelineResult(
        stats=stats,
        renderer=renderer,
        muted_ranges=muted,
        backpressure_wait_ms=shared.wait_ms,
        stall_count=shared.stall_count,
        blocks=total_blocks,
        missing_blocks=len(muted),
        work_dir=work_dir,
        resumed_from=completed,
    )


def _consume(
    index: int, work_dir: Path, delivery, renderer: FrameRenderer,
    stats: MixStats, cfg: PipelineConfig,
) -> None:
    if cfg.consumer_delay_ms > 0:
        time.sleep(cfg.consumer_delay_ms / 1000.0)
    path = _block_path(work_dir, index)
    raw = np.fromfile(path, dtype="<f4")
    data = raw.reshape(-1, delivery.channels).astype(np.float32)
    stats.update(data)
    renderer.push(data)


def _assemble_wav(
    work_dir: Path, out_path: Path, delivery, total_blocks: int,
    block_frames: int, total_samples: int,
) -> None:
    """Stream block files into the delivery WAV (bounded memory)."""
    with sf.SoundFile(
        out_path, "w", samplerate=delivery.sample_rate,
        channels=delivery.channels, subtype="FLOAT",
    ) as out:
        for i in range(total_blocks):
            raw = np.fromfile(_block_path(work_dir, i), dtype="<f4")
            out.write(raw.reshape(-1, delivery.channels).astype(np.float32))


def _cfg_fingerprint(timeline: Timeline, cfg: PipelineConfig) -> dict[str, object]:
    d = timeline.delivery
    return {
        "job": timeline.job,
        "block_frames": cfg.block_frames,
        "delivery": [d.sample_rate, d.channels, d.fps, d.start_tick, d.duration_ticks],
    }


def _write_checkpoint(
    work_dir: Path, timeline: Timeline, cfg: PipelineConfig, completed: int
) -> None:
    payload = _cfg_fingerprint(timeline, cfg)
    payload["completed_blocks"] = completed
    _checkpoint_path(work_dir).write_text(json.dumps(payload), encoding="utf-8")


def _load_checkpoint(work_dir: Path, timeline: Timeline, cfg: PipelineConfig) -> int:
    path = _checkpoint_path(work_dir)
    if not path.exists():
        return 0
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        return 0
    expected = _cfg_fingerprint(timeline, cfg)
    if any(payload.get(k) != v for k, v in expected.items()):
        return 0  # stale state from a different configuration: start clean
    completed = int(payload.get("completed_blocks", 0))
    for i in range(completed):
        if not _block_path(work_dir, i).exists():
            return 0
    return completed
