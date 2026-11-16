"""Resource-contract tests: streaming equals one-shot, constant memory, speed."""

from __future__ import annotations

import time
import tracemalloc
import unittest

import numpy as np

from helpers import TempWorkspace, noise, tone, write_wav

from audio_segmenter.model import DetectionConfig
from audio_segmenter.pipeline import analyze_file
from audio_segmenter import signal as sig

SR = 48000


class StreamingEquivalenceTests(unittest.TestCase):
    def test_block_boundaries_do_not_change_results(self) -> None:
        rng = np.random.default_rng(21)
        audio = (rng.standard_normal(50000) * 0.2).astype(np.float32)
        grid = sig.FrameGrid(SR, 20.0, 10.0)
        one_shot = sig.extract_features(audio, grid)
        for chunk in (1, 7, 997, 4096, 65536):
            acc = sig.FeatureAccumulator(grid)
            for offset in range(0, audio.size, chunk):
                acc.push(audio[offset : offset + chunk])
            streamed = acc.finish()
            self.assertEqual(streamed.frame_count, one_shot.frame_count)
            self.assertTrue(np.array_equal(streamed.starts, one_shot.starts))
            self.assertTrue(np.allclose(streamed.sum_square, one_shot.sum_square))
            self.assertTrue(np.allclose(streamed.peak, one_shot.peak))

    def test_stereo_is_downmixed_not_truncated(self) -> None:
        left = tone(SR, 0.2, amp=0.4)
        right = -left
        stereo = np.stack([left, right], axis=1)
        mono = sig.merge_channels(stereo)
        self.assertLess(float(np.abs(mono).max()), 1e-6)
        self.assertEqual(mono.size, left.size)


class MemoryContractTests(TempWorkspace):
    def test_million_sample_file_uses_less_memory_than_the_audio(self) -> None:
        sr = SR
        samples = sr * 25  # 1.2M samples, ~4.8 MB as float32
        path = self.path("million.wav")
        half = 12000
        blocks = []
        rng = np.random.default_rng(22)
        for offset in range(0, samples, half):
            count = min(half, samples - offset)
            block = (rng.standard_normal(count) * 0.002).astype(np.float32)
            if (offset // half) % 2 == 0:
                block += tone(sr, count / sr, amp=0.3)[:count]
            blocks.append(block)
        write_wav(path, np.concatenate(blocks), sr, 2)

        tracemalloc.start()
        started = time.time()
        analysis = analyze_file(path, DetectionConfig())
        elapsed = time.time() - started
        _, peak = tracemalloc.get_traced_memory()
        tracemalloc.stop()

        self.assertEqual(analysis.status, "ok")
        self.assertIsNotNone(analysis.result)
        self.assertGreater(len(analysis.result.segments), 5)
        audio_bytes = samples * np.dtype(np.float32).itemsize
        self.assertLess(peak, audio_bytes, "peak allocation must stay below full audio size")
        self.assertLess(elapsed, 10.0, "million-sample analysis should finish quickly")


if __name__ == "__main__":
    unittest.main()
