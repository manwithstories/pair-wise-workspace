"""Bounded queues and byte-limited caches enforce back pressure structurally."""

from __future__ import annotations

import threading
import time

import numpy as np
import pytest

from mediacore.errors import MediaInputError
from mediacore.queues import AudioBlockCache, BoundedQueue, ByteBudget, FrameCache


def test_queue_blocks_producer_until_consumer_drains():
    queue: BoundedQueue[int] = BoundedQueue(max_items=2, name="t")
    queue.put(1)
    queue.put(2)
    with pytest.raises(MediaInputError) as excinfo:
        queue.put_nowait(3)
    assert excinfo.value.code == "queue_full"

    placed = threading.Event()

    def producer():
        queue.put(3)
        placed.set()

    thread = threading.Thread(target=producer, daemon=True)
    thread.start()
    time.sleep(0.05)
    assert not placed.is_set(), "producer must be blocked while the queue is full"
    assert queue.get() == 1
    thread.join(timeout=2.0)
    assert placed.is_set()
    stats = queue.stats()
    assert stats["high_water"] == 2
    assert stats["blocked_puts"] >= 1


def test_close_wakes_blocked_consumer():
    queue: BoundedQueue[int] = BoundedQueue(max_items=1, name="c")
    result = {}

    def consumer():
        result["item"] = queue.get(timeout=5.0)

    thread = threading.Thread(target=consumer, daemon=True)
    thread.start()
    time.sleep(0.02)
    queue.close()
    thread.join(timeout=2.0)
    assert result["item"] is None


def test_byte_budget_denies_over_limit():
    budget = ByteBudget(limit=100)
    assert budget.try_reserve(60)
    assert not budget.try_reserve(60)
    assert budget.denied == 1
    assert budget.used == 60
    budget.release(60)
    assert budget.try_reserve(100)
    with pytest.raises(MediaInputError):
        budget.reserve(1)


def test_frame_cache_evicts_oldest_and_stays_within_limit():
    cache: FrameCache[np.ndarray] = FrameCache(limit_bytes=1000)
    for i in range(10):
        cache.put(i, np.zeros((10, 10), dtype=np.uint8))  # 100 bytes each
    assert len(cache) == 10
    assert cache.get(9) is not None
    cache.put(10, np.zeros((10, 10), dtype=np.uint8))
    assert cache.get(0) is None  # evicted
    assert cache.stats.evicted >= 1
    assert cache.stats.bytes_current <= cache.limit_bytes
    assert cache.stats.bytes_peak <= cache.limit_bytes


def test_oversized_item_is_not_cached():
    cache: FrameCache[np.ndarray] = FrameCache(limit_bytes=100)
    cache.put(0, np.zeros((50, 50), dtype=np.uint8))  # 2500 bytes
    assert len(cache) == 0
    assert cache.stats.evicted == 1


def test_audio_block_cache_uses_same_policy():
    cache: AudioBlockCache[np.ndarray] = AudioBlockCache(limit_bytes=256)
    for i in range(4):
        cache.put(i, np.zeros(64, dtype=np.float32))  # 256 bytes
    assert len(cache) == 1
    assert cache.get(3) is not None
    assert cache.stats.bytes_current <= cache.limit_bytes


def test_invalid_cache_size_is_rejected():
    with pytest.raises(MediaInputError):
        FrameCache(limit_bytes=0)
    with pytest.raises(MediaInputError):
        BoundedQueue(max_items=0)


def test_pipeline_audio_queue_stays_bounded(tmp_path):
    from mediacore.audio import analyze_wav
    from mediacore.fixtures import FixtureSpec, make_fixtures
    from mediacore.pipeline import PipelineConfig, run_pipeline

    spec = FixtureSpec(duration_s=2, bitrate=600_000, seed=5)
    files = make_fixtures(tmp_path / "in", spec)
    config = PipelineConfig(codec="h264", target_bps=600_000, duration_s=2,
                            seed=5, audio_block_frames=4096)
    result = run_pipeline(files["video_path"], files["audio"]["path"],
                          tmp_path / "run", config)
    resource = result.manifest.resource
    assert resource["audio_queue_high_water"] <= resource["audio_queue_max_blocks"]
    assert resource["read_queue_high_water"] <= resource["read_queue_max_items"]
    assert resource["payload_bytes_released"] > 0
    # Audio results must match the plain, single-threaded analysis exactly.
    direct = analyze_wav(files["audio"]["path"], block_frames=4096)
    # manifest.json stores RMS rounded to 6 decimals
    assert result.manifest.audio["rms"] == round(direct.rms, 6)
    assert result.manifest.audio["silence_intervals"] == [
        i.to_dict() for i in direct.silence_intervals]
