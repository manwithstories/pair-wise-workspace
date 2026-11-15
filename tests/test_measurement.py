import numpy as np
import pytest

from audio_pipeline.loudness import DB_FLOOR, integrated_loudness
from audio_pipeline.normalize import compute_gain
from audio_pipeline.truepeak import (
    ClipCounter,
    count_clipping,
    plateau_mask,
    true_peak,
)


def test_k_weighted_loudness_and_block_invariance():
    rate = 48000
    time = np.arange(rate * 2) / rate
    tone = 0.5 * np.sin(2 * np.pi * 1000 * time)
    stereo = np.stack((tone, tone), axis=1)
    loudness = integrated_loudness(stereo, rate, block_size=4096)
    assert loudness == pytest.approx(-6.057, abs=0.05)
    for block_size in (997, 4096, 50000):
        assert integrated_loudness(stereo, rate, block_size) == pytest.approx(
            loudness, abs=1e-9
        )
    silence = integrated_loudness(np.zeros((rate, 2)), rate)
    assert silence == DB_FLOOR


def test_true_peak_oversampling_and_short_signal():
    rate = 48000
    time = np.arange(rate) / rate
    full_scale = np.sin(2 * np.pi * 1000 * time)
    assert true_peak(full_scale) == pytest.approx(0.0, abs=0.01)
    high_frequency = np.sin(2 * np.pi * 15000 * time)
    measured = true_peak(high_frequency, block_size=777)
    assert measured >= 20 * np.log10(np.max(np.abs(high_frequency)))
    assert true_peak(np.zeros(100)) == DB_FLOOR


def test_clipping_threshold_and_plateau_counts_cross_blocks():
    samples = np.zeros((400, 2))
    samples[100:220, 0] = 0.999
    assert count_clipping(samples) == 120
    assert count_clipping(samples, block_size=37) == 120

    flat = np.zeros((400, 1))
    flat[50:60, 0] = 0.995
    counter = ClipCounter()
    for start in range(0, len(flat), 3):
        counter.process(flat[start : start + 3])
    assert counter.finalize() == 10
    counter = ClipCounter()
    for index in range(len(flat)):
        counter.process(flat[index : index + 1])
    assert counter.finalize() == 10
    assert int(plateau_mask(flat).sum()) == 10
    assert count_clipping(np.full((4, 1), 0.998)) == 4


def test_gain_plan_caps_and_true_peak_backoff_are_auditable():
    capped = compute_gain(-60.0, -40.0, target_lufs=-16.0)
    assert capped.gain_db == 12.0
    assert capped.limited_by_gain_cap

    limited = compute_gain(-27.1, -6.0, target_lufs=-16.0)
    assert limited.limited_by_true_peak
    assert limited.gain_db == pytest.approx(5.0, abs=2e-6)
    assert limited.rounds[0].action == "true_peak_backoff"
    assert limited.rounds[-1].action == "accepted"
    assert -6.0 + limited.gain_db <= -1.0
    assert len(limited.rounds) <= 5

    quiet = compute_gain(DB_FLOOR, DB_FLOOR)
    assert quiet.gain_db == 0.0
    loud = compute_gain(-5.0, -3.0, target_lufs=-16.0)
    assert loud.gain_db == pytest.approx(-11.0)
