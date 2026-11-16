"""Detector edge cases: thresholds, hysteresis, pulses, ramps, boundaries."""

from __future__ import annotations

import unittest

import numpy as np

from helpers import TempWorkspace, fade, noise, silence, tone, write_wav

from audio_segmenter import signal as sig
from audio_segmenter.detector import (
    audible_floor,
    compute_thresholds,
    detect_array,
    estimate_noise_floor,
    exact_region_stats,
    hysteresis_mask,
)
from audio_segmenter.model import DetectionConfig
from audio_segmenter.regions import Interval
from audio_segmenter.verify import verify_result

SR = 48000


class NoiseFloorTests(unittest.TestCase):
    def test_floor_tracks_quiet_tail(self) -> None:
        db = np.array([-70.0, -68.0, -66.0, -10.0, -8.0])
        floor = estimate_noise_floor(db, 15.0)
        self.assertGreaterEqual(floor, -70.0)
        self.assertLess(floor, -60.0)

    def test_digital_silence_does_not_raise_the_floor(self) -> None:
        db = np.array([sig.DB_FLOOR, sig.DB_FLOOR, -12.0, -10.0])
        self.assertEqual(estimate_noise_floor(db, 15.0), sig.DB_FLOOR)
        self.assertGreater(audible_floor(db, 15.0), sig.DB_FLOOR)

    def test_empty_input_is_floored(self) -> None:
        self.assertEqual(estimate_noise_floor(np.array([]), 15.0), sig.DB_FLOOR)

    def test_relative_threshold_never_below_absolute(self) -> None:
        db = np.full(100, -60.0)
        noise_floor, enter, exit_db = compute_thresholds(db, DetectionConfig())
        self.assertEqual(enter, DetectionConfig().threshold_db)
        self.assertAlmostEqual(exit_db, enter - DetectionConfig().hysteresis_db)

    def test_loud_floor_raises_enter_threshold(self) -> None:
        db = np.full(100, -30.0)
        _, enter, _ = compute_thresholds(db, DetectionConfig())
        self.assertGreater(enter, DetectionConfig().threshold_db)


class HysteresisTests(unittest.TestCase):
    def test_brief_dip_inside_hysteresis_band_is_kept(self) -> None:
        db = np.array([-60, -40, -46, -40, -60], dtype=float)
        mask = hysteresis_mask(db, enter_db=-42.0, exit_db=-48.0)
        self.assertTrue(np.all(mask[1:4]))

    def test_dip_below_exit_threshold_closes_run(self) -> None:
        db = np.array([-60, -40, -50, -40, -60], dtype=float)
        mask = hysteresis_mask(db, enter_db=-42.0, exit_db=-48.0)
        self.assertEqual(mask.tolist(), [False, True, False, True, False])

    def test_open_run_reaches_end(self) -> None:
        db = np.array([-60, -40, -39], dtype=float)
        mask = hysteresis_mask(db, enter_db=-42.0, exit_db=-48.0)
        self.assertEqual(mask.tolist(), [False, True, True])


class PulseTests(unittest.TestCase):
    def test_short_pulse_bridges_neighbouring_segments(self) -> None:
        sr = SR
        audio = np.concatenate(
            [
                noise(sr, 0.3, 0.001, seed=1),
                tone(sr, 0.35),
                silence(sr, 0.03),
                tone(sr, 0.35),
                noise(sr, 0.3, 0.001, seed=2),
            ]
        )
        config = DetectionConfig(max_pulse_ms=50.0, min_silence_ms=120.0)
        result = detect_array(audio, sr, config, path="pulse.wav")
        self.assertEqual(len(result.segments), 1, "30 ms gap must be bridged")

    def test_gap_longer_than_min_silence_is_preserved(self) -> None:
        sr = SR
        audio = np.concatenate(
            [
                tone(sr, 0.35),
                silence(sr, 0.25),
                tone(sr, 0.35),
            ]
        )
        result = detect_array(audio, sr, DetectionConfig(), path="gap.wav")
        self.assertEqual(len(result.segments), 2)


class MinSilenceBoundaryTests(unittest.TestCase):
    """The min-silence threshold is compared in samples, not frames."""

    def _two_bursts(self, gap_seconds: float) -> np.ndarray:
        burst = tone(SR, 1 / 3)
        return np.concatenate(
            [silence(SR, 0.25), burst, silence(SR, gap_seconds), burst, silence(SR, 0.25)]
        )

    def test_gap_below_min_silence_is_merged(self) -> None:
        # 60 ms is far enough below the 120 ms threshold that frame
        # quantisation (at most one hop) cannot flip the decision.
        result = detect_array(
            self._two_bursts(0.060), SR, DetectionConfig(), path="60.wav"
        )
        self.assertEqual(len(result.segments), 1)

    def test_gap_above_min_silence_is_kept(self) -> None:
        result = detect_array(
            self._two_bursts(0.180), SR, DetectionConfig(), path="180.wav"
        )
        self.assertEqual(len(result.segments), 2)

    def test_gap_measurement_error_is_within_one_hop(self) -> None:
        # Padding is applied after the merge decision, so measure with
        # padding disabled to isolate boundary accuracy.
        config = DetectionConfig(padding_ms=0.0)
        result = detect_array(self._two_bursts(0.240), SR, config, path="240.wav")
        self.assertEqual(len(result.segments), 2)
        gap = result.segments[1].start_sample - result.segments[0].end_sample
        hop = int(round(SR * config.hop_ms / 1000.0))
        self.assertLessEqual(abs(gap - int(round(SR * 0.240))), hop)

    def test_padding_shrinks_reported_gap_by_twice_the_budget(self) -> None:
        gap = 0.240
        config = DetectionConfig()
        padded = detect_array(self._two_bursts(gap), SR, config, path="pad.wav")
        tight = detect_array(
            self._two_bursts(gap), SR, DetectionConfig(padding_ms=0.0), path="nopad.wav"
        )
        reported = padded.segments[1].start_sample - padded.segments[0].end_sample
        expected = (
            tight.segments[1].start_sample
            - tight.segments[0].end_sample
            - 2 * int(round(SR * config.padding_ms / 1000.0))
        )
        self.assertEqual(reported, expected)


class RampTests(unittest.TestCase):
    def test_slow_fade_is_retained_not_reported_as_silence(self) -> None:
        sr = SR
        body = fade(tone(sr, 1.2, amp=0.5), sr, 0.30, 0.30)
        audio = np.concatenate([noise(sr, 0.5, 0.001, seed=3), body, noise(sr, 0.5, 0.001, seed=4)])
        true_start = int(round(sr * 0.5))
        true_end = true_start + body.size

        config = DetectionConfig()
        result = detect_array(audio, sr, config, path="fade.wav")
        self.assertEqual(len(result.segments), 1)
        seg = result.segments[0]
        before = abs(seg.start_sample - true_start) / sr
        after = abs(seg.end_sample - true_end) / sr
        self.assertLess(before, 0.10, "fade-in start should be within 100 ms")
        self.assertLess(after, 0.10, "fade-out end should be within 100 ms")

    def test_steady_floor_is_not_absorbed(self) -> None:
        sr = SR
        audio = np.concatenate(
            [
                noise(sr, 0.6, 0.001, seed=5),
                tone(sr, 0.5),
                noise(sr, 0.6, 0.001, seed=6),
            ]
        )
        config = DetectionConfig()
        result = detect_array(audio, sr, config, path="floor.wav")
        self.assertEqual(len(result.segments), 1)
        seg = result.segments[0]
        true_end = int(round(sr * 1.1))
        # Padding and the analysis frame may legitimately extend the boundary;
        # a steady floor must not add anything beyond that budget.
        allowance = int(round(sr * (config.padding_ms + config.frame_ms) / 1000.0))
        self.assertGreaterEqual(seg.start_sample, int(round(sr * 0.6)) - allowance)
        self.assertLessEqual(seg.end_sample, true_end + allowance)


class BoundaryTests(TempWorkspace):
    def test_all_silent_file_has_no_segments(self) -> None:
        audio = noise(SR, 1.0, 0.0005, seed=7)
        result = detect_array(audio, SR, DetectionConfig(), path="quiet.wav")
        self.assertEqual(result.segments, [])
        self.assertEqual(len(result.silences), 1)
        self.assertEqual(result.total_silence_samples, audio.size)

    def test_all_loud_file_is_one_segment(self) -> None:
        audio = tone(SR, 1.0)
        result = detect_array(audio, SR, DetectionConfig(), path="loud.wav")
        self.assertEqual(len(result.segments), 1)
        self.assertEqual(result.total_silence_samples, 0)

    def test_segments_are_ordered_and_disjoint(self) -> None:
        audio = np.concatenate(
            [
                noise(SR, 0.2, 0.001, seed=8),
                tone(SR, 0.3),
                silence(SR, 0.3),
                tone(SR, 0.3),
                noise(SR, 0.2, 0.001, seed=9),
            ]
        )
        result = detect_array(audio, SR, DetectionConfig(), path="multi.wav")
        self.assertEqual(len(result.segments), 2)
        self.assertEqual(verify_result(result).passed, True)
        for left, right in zip(result.segments, result.segments[1:]):
            self.assertLess(left.end_sample, right.end_sample)

    def test_segment_shorter_than_minimum_is_dropped(self) -> None:
        audio = np.concatenate(
            [noise(SR, 0.3, 0.001, seed=10), tone(SR, 0.04), noise(SR, 0.3, 0.001, seed=11)]
        )
        result = detect_array(audio, SR, DetectionConfig(), path="blip.wav")
        self.assertEqual(result.segments, [])

    def test_padding_expands_into_neighbouring_silence(self) -> None:
        audio = np.concatenate([silence(SR, 0.4), tone(SR, 0.4), silence(SR, 0.4)])
        tight = detect_array(audio, SR, DetectionConfig(padding_ms=0.0), path="p0.wav")
        padded = detect_array(audio, SR, DetectionConfig(padding_ms=40.0), path="p40.wav")
        self.assertLess(padded.segments[0].start_sample, tight.segments[0].start_sample)
        self.assertGreater(padded.segments[0].end_sample, tight.segments[0].end_sample)

    def test_padding_never_crosses_neighbours(self) -> None:
        audio = np.concatenate(
            [silence(SR, 0.2), tone(SR, 0.3), silence(SR, 0.2), tone(SR, 0.3), silence(SR, 0.2)]
        )
        result = detect_array(audio, SR, DetectionConfig(padding_ms=500.0), path="pbig.wav")
        self.assertEqual(len(result.segments), 2)
        self.assertLessEqual(result.segments[0].end_sample, result.segments[1].start_sample)


class StatsTests(TempWorkspace):
    def test_region_stats_match_direct_measurement(self) -> None:
        rng = np.random.default_rng(12)
        audio = (rng.standard_normal(20000) * 0.25).astype(np.float32)
        stats = exact_region_stats(
            lambda: iter([audio]), [Interval(1234, 9876)]
        )
        direct = audio[1234:9876]
        expected_rms = float(np.sqrt(np.mean(direct.astype(np.float64) ** 2)))
        self.assertAlmostEqual(stats[0][1], expected_rms, places=6)
        self.assertAlmostEqual(stats[0][0], float(np.abs(direct).max()), places=6)

    def test_wav_roundtrip_segment_values(self) -> None:
        audio = np.concatenate([silence(SR, 0.2), tone(SR, 0.4), silence(SR, 0.2)])
        path = self.path("roundtrip.wav")
        write_wav(path, audio, SR, 2)
        from audio_segmenter.pipeline import analyze_file

        analysis = analyze_file(path, DetectionConfig())
        self.assertEqual(analysis.status, "ok")
        seg = analysis.result.segments[0]
        self.assertGreater(seg.peak, 0.3)
        self.assertGreater(seg.rms, 0.2)
        self.assertLess(seg.rms, seg.peak)


if __name__ == "__main__":
    unittest.main()
