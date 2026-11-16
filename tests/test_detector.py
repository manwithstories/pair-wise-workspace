"""Unit tests for wavio / framing / detector / refine (pure-logic layer)."""
from __future__ import annotations

import unittest
import wave
from pathlib import Path
from tempfile import TemporaryDirectory

import numpy as np

from audio_slicer import detector, framing, refine, wavio


def _frame_db(noise_db, events, frame_len=480, total_frames=2000):
    db = np.full(total_frames, noise_db)
    for start, end, level in events:
        db[start:end] = level
    return db, frame_len


class NoiseFloorTest(unittest.TestCase):
    def test_adapts_to_studio_floor(self):
        rng = np.random.RandomState(7)
        for floor in (-70.0, -58.0, -45.0):
            db = floor + rng.randn(4000) * 0.4
            db[1000:1400] = floor + 30.0
            estimate = detector.estimate_noise_floor(db)
            self.assertAlmostEqual(estimate, floor, delta=1.0)

    def test_median_filter_rejects_spikes(self):
        db = np.full(200, -60.0)
        db[100] = -10.0
        smoothed = detector.median_filter(db, kernel=5)
        self.assertAlmostEqual(smoothed[100], -60.0, places=6)


class DetectEventsTest(unittest.TestCase):
    def test_hysteresis_ignores_wobble(self):
        # One frame dips between exit and enter thresholds mid-event.
        db, flen = _frame_db(-58.0, [(100, 200, -30.0)])
        db[150] = -51.5
        segments, th = detector.detect_events(
            db, flen, 2000 * flen, 48000, noise_floor_db=-58.0)
        self.assertEqual(len(segments), 1)
        self.assertEqual(th["enter_threshold_dbfs"], -50.0)
        self.assertEqual(th["exit_threshold_dbfs"], -53.0)

    def test_short_gap_merges_and_long_gap_splits(self):
        db, flen = _frame_db(-58.0, [(100, 150, -30.0), (160, 210, -30.0),
                                     (500, 560, -30.0)])
        segments, _ = detector.detect_events(
            db, flen, 2000 * flen, 48000, min_silence_ms=200.0,
            noise_floor_db=-58.0)
        self.assertEqual(len(segments), 2)
        self.assertEqual(segments[0].start_sample, 100 * flen)
        self.assertEqual(segments[0].end_sample, 210 * flen)

    def test_short_event_dropped(self):
        db, flen = _frame_db(-58.0, [(100, 105, -30.0), (500, 560, -30.0)])
        segments, _ = detector.detect_events(
            db, flen, 2000 * flen, 48000, min_event_ms=150.0,
            noise_floor_db=-58.0)
        self.assertEqual(len(segments), 1)
        self.assertEqual(segments[0].ordinal, 1)


class InvariantTest(unittest.TestCase):
    def test_conservation_and_overlap_detected(self):
        segs = [detector.Segment(1, 0, 100), detector.Segment(2, 90, 200)]
        sils = [detector.Silence(1, 200, 300)]
        problems = detector.check_invariants(segs, sils, 300)
        self.assertTrue(any("overlap" in p for p in problems))

    def test_clean_tiling_passes(self):
        segs = [detector.Segment(1, 10, 50), detector.Segment(2, 80, 120)]
        sils = detector.compute_silences(segs, 150)
        self.assertEqual(detector.check_invariants(segs, sils, 150), [])
        total = sum(s.duration_samples for s in sils)
        self.assertEqual(total, 150 - 80)


class WavioTest(unittest.TestCase):
    def _write(self, path, samples, rate, sampwidth, channels=1):
        with wave.open(str(path), "wb") as out:
            out.setnchannels(channels)
            out.setsampwidth(sampwidth)
            out.setframerate(rate)
            out.writeframes(samples)

    def test_24bit_roundtrip(self):
        with TemporaryDirectory() as tmp:
            path = Path(tmp) / "t.wav"
            ints = np.array([0, 100000, -100000, 8388607, -8388608],
                            dtype=np.int32)
            raw = np.frombuffer(ints.astype("<i4").tobytes(), np.uint8)
            self._write(path, raw.reshape(-1, 4)[:, :3].tobytes(), 48000, 3)
            data, rate = wavio.read_wav(path)
            self.assertEqual(rate, 48000)
            self.assertEqual(data.shape, (5,))
            self.assertAlmostEqual(float(data[1]), 100000 / 8388608.0, places=6)
            self.assertAlmostEqual(float(data[4]), -1.0, places=6)

    def test_stereo_downmix_mean(self):
        with TemporaryDirectory() as tmp:
            path = Path(tmp) / "s.wav"
            left = np.full(100, 0.5, dtype=np.float32)
            right = np.full(100, -0.25, dtype=np.float32)
            stereo = np.stack([left, right], axis=1).reshape(-1)
            pcm = np.round(stereo * 32767.0).astype("<i2")
            self._write(path, pcm.tobytes(), 44100, 2, channels=2)
            data, _ = wavio.read_wav(path)
            self.assertAlmostEqual(float(data[0]), 0.125, places=4)

    def test_rejects_bad_inputs(self):
        with TemporaryDirectory() as tmp:
            bad = Path(tmp) / "x.txt"
            bad.write_text("nope")
            with self.assertRaises(wavio.WavFormatError):
                wavio.read_wav(bad)
            odd = Path(tmp) / "odd.wav"
            self._write(odd, b"\x00" * 200, 4000, 2)
            with self.assertRaises(wavio.WavFormatError):
                wavio.read_wav(odd)

    def test_safe_resolve_confines(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            inside = wavio.safe_resolve(root / "a.wav", root)
            self.assertTrue(str(inside).startswith(str(root)))
            with self.assertRaises(wavio.WavFormatError):
                wavio.safe_resolve(root / ".." / "escape.wav", root)


class RefineTest(unittest.TestCase):
    def test_fade_included_and_reported(self):
        rate = 48000
        rng = np.random.RandomState(3)
        noise = rng.randn(rate * 2).astype(np.float32) * 10 ** (-58 / 20)
        fade = int(rate * 0.05)
        body = int(rate * 0.2)
        env = np.concatenate([
            0.5 * (1 - np.cos(np.pi * np.arange(fade) / fade)),
            np.ones(body),
            0.5 * (1 + np.cos(np.pi * np.arange(fade) / fade)),
        ]).astype(np.float32)
        event = env * 0.2 * np.sin(
            2 * np.pi * 440 * np.arange(len(env)) / rate).astype(np.float32)
        start = rate // 2
        noise[start:start + len(event)] += event
        frame_db, flen = framing.frame_dbfs(noise, rate, 10.0)
        floor = detector.estimate_noise_floor(frame_db)
        candidates, th = detector.detect_events(
            frame_db, flen, noise.shape[0], rate, noise_floor_db=floor)
        self.assertEqual(len(candidates), 1)
        refined = refine.refine_segments(
            noise, rate, candidates, floor, th["enter_threshold_dbfs"])
        seg = refined[0]
        self.assertLessEqual(seg.start_sample, start + flen)
        self.assertGreater(seg.fade_in_samples, 0)
        self.assertGreaterEqual(seg.end_sample, start + len(event) - flen)
        sils = detector.compute_silences(refined, noise.shape[0])
        self.assertEqual(
            detector.check_invariants(refined, sils, noise.shape[0]), [])


if __name__ == "__main__":
    unittest.main()
