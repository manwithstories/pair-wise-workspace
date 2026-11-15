from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import numpy as np

from audiosplit.detector import DetectorParams, detect
from audiosplit.errors import AudioError
from audiosplit.wavio import WavReader, write_wav_atomic


SAMPLE_RATE = 48000


def _tone(seconds: float, frequency: float = 700.0, amplitude: float = 0.2) -> np.ndarray:
    time = np.arange(int(seconds * SAMPLE_RATE)) / SAMPLE_RATE
    return (amplitude * np.sin(2.0 * np.pi * frequency * time)).astype(np.float32)


def _noise(seconds: float, dbfs: float, seed: int) -> np.ndarray:
    rng = np.random.default_rng(seed)
    values = rng.normal(0.0, 1.0, int(seconds * SAMPLE_RATE))
    values *= 10.0 ** (dbfs / 20.0) / np.sqrt(np.mean(values**2))
    return values.astype(np.float32)


class DetectorTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)

    def tearDown(self) -> None:
        self.temp.cleanup()

    def _detect(self, samples: np.ndarray, **kwargs):
        path = self.root / "input.wav"
        write_wav_atomic(path, SAMPLE_RATE, samples)
        options = {"pad_ms": 0.0}
        options.update(kwargs)
        with WavReader(path) as reader:
            return detect(reader, DetectorParams(**options))

    def test_all_silent_and_all_active(self) -> None:
        silent = self._detect(_noise(2.0, -60.0, 1))
        self.assertEqual(silent.segments, [])
        self.assertEqual(len(silent.silences), 1)
        active = self._detect(_tone(2.0))
        self.assertEqual(len(active.segments), 1)
        self.assertEqual(active.segments[0].start_sample, 0)
        self.assertEqual(active.segments[0].end_sample, 2 * SAMPLE_RATE)

    def test_two_utterances_and_boundary_accuracy(self) -> None:
        samples = np.concatenate(
            [
                _noise(0.5, -55.0, 2),
                _tone(0.5),
                _noise(0.8, -55.0, 3),
                _tone(0.5),
                _noise(0.5, -55.0, 4),
            ]
        )
        result = self._detect(samples)
        self.assertEqual(len(result.segments), 2)
        starts = [0.5 * SAMPLE_RATE, 1.8 * SAMPLE_RATE]
        ends = [1.0 * SAMPLE_RATE, 2.3 * SAMPLE_RATE]
        for segment, expected_start, expected_end in zip(result.segments, starts, ends):
            self.assertLessEqual(abs(segment.start_sample - expected_start), 48)
            self.assertLessEqual(abs(segment.end_sample - expected_end), 48)

    def test_short_silence_merges_and_short_segment_drops(self) -> None:
        near = np.concatenate([_tone(0.4), _noise(0.1, -55.0, 5), _tone(0.4)])
        merged = self._detect(near, min_sil_ms=250)
        self.assertEqual(len(merged.segments), 1)
        split = self._detect(near, min_sil_ms=0)
        self.assertEqual(len(split.segments), 2)
        short = self._detect(_tone(0.05), min_seg_ms=120)
        self.assertEqual(short.segments, [])
        self.assertGreaterEqual(short.dropped_segments, 1)
        padded_short = self._detect(_tone(0.05), min_seg_ms=120, pad_ms=40)
        self.assertEqual(padded_short.segments, [])
        self.assertGreaterEqual(padded_short.dropped_segments, 1)

    def test_noise_floor_tracks_level(self) -> None:
        for target in (-60.0, -45.0):
            with self.subTest(target=target):
                result = self._detect(_noise(2.0, target, 6))
                self.assertEqual(result.segments, [])
                self.assertAlmostEqual(
                    result.thresholds.noise_floor_dbfs, target, delta=2.0
                )

    def test_empty_and_single_sample_errors(self) -> None:
        one = self.root / "one.wav"
        write_wav_atomic(one, SAMPLE_RATE, np.array([0.0], dtype=np.float32))
        with WavReader(one) as reader:
            with self.assertRaises(AudioError) as caught:
                detect(reader, DetectorParams())
        self.assertEqual(caught.exception.code, "audio_too_short")


if __name__ == "__main__":
    unittest.main()

