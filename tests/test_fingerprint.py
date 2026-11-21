"""Tests for content fingerprints and timestamp-independent dedup."""

import numpy as np

from archive_media.fingerprint import (
    AudioChunkAccumulator,
    Fingerprint,
    VideoChunkAccumulator,
    find_duplicates,
    jaccard,
)


class _Plane:
    def __init__(self, arr):
        self._arr = arr
        self.line_size = arr.shape[1]

    def __buffer__(self):
        return memoryview(np.ascontiguousarray(self._arr))


class _Frame:
    def __init__(self, width, height, value):
        self.width = width
        self.height = height
        y = np.full((height, width), value, dtype=np.uint8)
        self.planes = [y]


def test_video_accumulator_emits_chunks_and_flush():
    acc = VideoChunkAccumulator(frames_per_chunk=4)
    hashes = []
    for i in range(10):
        hashes += acc.add_frame(_Frame(32, 24, 40 + i * 7))
    hashes += acc.flush()
    assert len(hashes) == 3  # two full chunks + one flushed remainder
    assert all(isinstance(h, str) and len(h) == 32 for h in hashes)


def test_identical_content_matches_different_content_does_not():
    def fp(source, seed_value):
        va = VideoChunkAccumulator(frames_per_chunk=2)
        hashes = []
        for i in range(8):
            hashes += va.add_frame(_Frame(16, 16, seed_value + i))
        hashes += va.flush()
        return Fingerprint(source, hashes, [])

    same_a = fp("a", 100)
    same_b = fp("b", 100)  # identical pixels, different source/timestamps
    different = fp("c", 200)
    assert jaccard(same_a.all_hashes(), same_b.all_hashes()) == 1.0
    assert jaccard(same_a.all_hashes(), different.all_hashes()) < 0.8
    pairs = find_duplicates([same_a, same_b, different])
    matched = {(p["source_a"], p["source_b"]) for p in pairs}
    assert ("a", "b") in matched
    assert not any("c" in pair for pair in matched)


def test_audio_accumulator_distinguishes_waveforms():
    def hashes_for(samples):
        acc = AudioChunkAccumulator(samples_per_chunk=512)
        out = []
        for i in range(8):
            out += acc.add_samples(samples[i * 512 : (i + 1) * 512])
        out += acc.flush()
        return set(out)

    tone = np.sin(np.linspace(0, 50, 4096)).astype(np.float32)
    noise = np.random.default_rng(7).standard_normal(4096).astype(np.float32) * 0.3
    assert hashes_for(tone) != hashes_for(noise)
