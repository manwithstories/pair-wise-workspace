"""Audio: exact boundaries, resampling, layout, loudness and edge cases."""

from __future__ import annotations

import numpy as np
import pytest

from mediacore.audio import render_clip
from mediacore.audio_io import read_wav, render_blend
from mediacore.audio_loudness import (
    SILENCE_LUFS,
    LoudnessMeter,
    gain_for_target,
    measure_loudness,
)
from mediacore.audio_model import AudioBuffer
from mediacore.errors import AudioError
from mediacore.timebase import samples_for_duration


def _tone(rate: int, duration_us: int, freq: float, amplitude: float, channels: int = 2):
    n = samples_for_duration(duration_us, rate)
    t = np.arange(n) / rate
    base = amplitude * np.sin(2 * np.pi * freq * t)
    data = np.stack([base] * channels).astype(np.float32)
    return np.ascontiguousarray(data)


def _buffer(data: np.ndarray, rate: int, layout: str = "stereo") -> AudioBuffer:
    return AudioBuffer(samples=data, sample_rate=rate, layout=layout)  # type: ignore[arg-type]


def test_k_weighting_attenuates_low_frequencies():
    rate = 48_000
    readings = {}
    for freq in (60, 1000):
        audio = _tone(rate, 2_000_000, freq, 0.1)
        readings[freq] = measure_loudness(_buffer(audio, rate)).integrated_lufs
    # 60 Hz must read several LU lower than 1 kHz for equal amplitude.
    assert readings[1000] - readings[60] > 2.5


def test_measurement_is_independent_of_chunking():
    rate = 48_000
    mono = _tone(rate, 2_000_000, 300, 0.1, channels=1)
    results = []
    for chunk in (1024, 4096, 48_000):
        meter = LoudnessMeter(rate, 1)
        for start in range(0, mono.shape[1], chunk):
            meter.feed(mono[:, start : start + chunk])
        results.append(meter.result()[0])
    assert max(results) - min(results) < 1e-6


def test_silence_measures_as_silence_and_gains_zero():
    audio = _buffer(np.zeros((2, 48_000), dtype=np.float32), 48_000)
    stats = measure_loudness(audio)
    assert stats.silent
    assert stats.integrated_lufs == SILENCE_LUFS
    gain, limited, silent = gain_for_target(SILENCE_LUFS, SILENCE_LUFS, -16.0, -1.0)
    assert (gain, limited, silent) == (0.0, False, True)


def test_gain_hits_target_and_respects_ceiling():
    gain, limited, silent = gain_for_target(-24.0, -18.0, -16.0, -1.0)
    assert not limited and not silent
    assert gain == pytest.approx(8.0)

    # A quiet-but-peaky signal: loudness wants +20 dB, ceiling only allows +2.
    gain, limited, _ = gain_for_target(-36.0, -3.0, -16.0, -1.0)
    assert limited
    assert gain == pytest.approx(2.0)


def test_render_clip_hits_exact_sample_count(fixtures):
    for duration in (1_000_000, 1_234_567, 333_333):
        result = render_clip(
            fixtures["clean"],
            f"/tmp/mc_clip_{duration}.wav",
            stream_index=1,
            start_us=0,
            end_us=duration,
            sample_rate=48_000,
            layout="stereo",
            target_lufs=-16.0,
        )
        assert result.samples == samples_for_duration(duration, 48_000)


def test_render_resamples_and_reaches_target(fixtures):
    # Voice track is 44.1 kHz in the source; render at 48 kHz.
    result = render_clip(
        fixtures["clean"],
        "/tmp/mc_resampled.wav",
        stream_index=1,
        start_us=0,
        end_us=1_000_000,
        sample_rate=48_000,
        layout="stereo",
        target_lufs=-16.0,
    )
    output = read_wav(result.path)
    assert output.sample_rate == 48_000
    assert output.frames == 48_000
    post = measure_loudness(output)
    assert abs(post.integrated_lufs - (-16.0)) < 0.5


def test_layout_normalisation(fixtures):
    mono = render_clip(
        fixtures["clean"],
        "/tmp/mc_mono.wav",
        stream_index=2,
        start_us=0,
        end_us=500_000,
        sample_rate=48_000,
        layout="mono",
        target_lufs=-18.0,
    )
    assert read_wav(mono.path).samples.shape[0] == 1

    stereo_from_mono = render_clip(
        fixtures["clean"],
        "/tmp/mc_upmix.wav",
        stream_index=2,
        start_us=0,
        end_us=500_000,
        sample_rate=48_000,
        layout="stereo",
        target_lufs=-18.0,
    )
    assert read_wav(stereo_from_mono.path).samples.shape[0] == 2


def test_render_is_deterministic(fixtures):
    kwargs = dict(
        stream_index=1,
        start_us=0,
        end_us=700_000,
        sample_rate=48_000,
        layout="stereo",
        target_lufs=-16.0,
    )
    a = render_clip(fixtures["clean"], "/tmp/mc_det_a.wav", **kwargs)
    b = render_clip(fixtures["clean"], "/tmp/mc_det_b.wav", **kwargs)
    assert a.sha256 == b.sha256


def test_render_raises_when_source_is_too_short(fixtures):
    with pytest.raises(AudioError) as info:
        render_clip(
            fixtures["truncated"],
            "/tmp/mc_short.wav",
            stream_index=1,
            start_us=0,
            end_us=2_500_000,
            sample_rate=48_000,
            layout="stereo",
            target_lufs=-16.0,
        )
    error = info.value
    assert error.context["expected_samples"] > error.context["available_samples"]


def test_very_short_clip_reports_whole_clip_measurement(fixtures):
    result = render_clip(
        fixtures["clean"],
        "/tmp/mc_tiny.wav",
        stream_index=1,
        start_us=0,
        end_us=150_000,  # 150 ms < 400 ms block
        sample_rate=48_000,
        layout="stereo",
        target_lufs=-16.0,
    )
    assert result.loudness.measurement == "whole_clip"
    assert result.samples == samples_for_duration(150_000, 48_000)


def test_crossfade_length_and_format(fixtures):
    left = render_clip(
        fixtures["clean"], "/tmp/mc_left.wav", stream_index=1,
        start_us=0, end_us=1_200_000, sample_rate=48_000, layout="stereo",
        target_lufs=-16.0,
    )
    right = render_clip(
        fixtures["clean"], "/tmp/mc_right.wav", stream_index=1,
        start_us=1_000_000, end_us=2_000_000, sample_rate=48_000, layout="stereo",
        target_lufs=-16.0,
    )
    blend = render_blend(
        left.path, right.path, "/tmp/mc_blend.wav",
        duration_us=200_000, sample_rate=48_000, curve="equal_power",
    )
    assert blend.samples == samples_for_duration(200_000, 48_000)
    assert read_wav(blend.path).frames == blend.samples
