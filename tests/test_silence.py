"""Tests for silence detection including short-window and tail edges."""

import numpy as np

from archive_media.silence import SilenceDetector


def test_detects_silent_run_with_metrics():
    sr = 1000
    det = SilenceDetector(sr, threshold=0.01, min_duration_s=0.05, window_s=0.02)
    # 40 ms tone, 100 ms silence, 40 ms tone (all frame-aligned).
    tone = np.full(40, 0.3, dtype=np.float32)
    quiet = np.zeros(100, dtype=np.float32)
    t = 0.0
    for chunk in (tone, quiet, tone):
        det.add_frame(t, chunk)
        t += chunk.size / sr
    regions = det.finalize()
    assert len(regions) == 1
    region = regions[0]
    assert region.end_s - region.start_s >= 0.05
    assert region.peak <= 0.01
    assert region.rms <= 0.01
    assert region.sample_count == 100


def test_short_silence_is_rejected():
    sr = 1000
    det = SilenceDetector(sr, threshold=0.01, min_duration_s=0.1, window_s=0.02)
    det.add_frame(0.0, np.full(50, 0.3, dtype=np.float32))
    det.add_frame(0.05, np.zeros(20, dtype=np.float32))  # 20 ms < 100 ms
    det.add_frame(0.07, np.full(50, 0.3, dtype=np.float32))
    assert det.finalize() == []


def test_partial_tail_window_is_evaluated():
    sr = 1000
    det = SilenceDetector(sr, threshold=0.01, min_duration_s=0.005, window_s=0.02)
    # Only 10 ms of silence: shorter than one 20 ms window but must not be lost.
    det.add_frame(0.0, np.zeros(10, dtype=np.float32))
    regions = det.finalize()
    assert len(regions) == 1
    assert regions[0].sample_count == 10


def test_loud_signal_not_flagged():
    sr = 1000
    det = SilenceDetector(sr, threshold=0.01, min_duration_s=0.02)
    det.add_frame(0.0, np.full(200, 0.5, dtype=np.float32))
    assert det.finalize() == []
