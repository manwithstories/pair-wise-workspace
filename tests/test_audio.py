"""Chunked WAV measurement: sample rate, channels, RMS/peak and silence."""

from __future__ import annotations

import numpy as np
import pytest
import soundfile as sf

from mediacore.audio import align_audio, analyze_wav
from mediacore.errors import MediaInputError


@pytest.fixture()
def tone(tmp_path):
    rate = 48000
    seconds = 2.0
    t = np.arange(int(rate * seconds)) / rate
    signal = (0.5 * np.sin(2 * np.pi * 440 * t)).astype(np.float32)
    signal[int(rate * 0.5):int(rate * 0.75)] = 0.0  # 250 ms of silence
    path = tmp_path / "tone.wav"
    sf.write(str(path), signal, rate, subtype="PCM_16")
    return path, signal, rate


def test_header_and_statistics(tone):
    path, signal, rate = tone
    analysis = analyze_wav(path, block_frames=1000)
    assert analysis.sample_rate == rate
    assert analysis.channels == 1
    assert analysis.frames == signal.size
    assert analysis.duration_s == pytest.approx(2.0, rel=0, abs=1e-6)
    # 16-bit quantisation keeps peak and RMS within a small margin
    assert analysis.peak == pytest.approx(0.5, abs=1e-3)
    expected_rms = float(np.sqrt(np.mean(signal.astype(np.float64) ** 2)))
    assert analysis.rms == pytest.approx(expected_rms, rel=1e-3)


def test_silence_survives_block_boundaries(tone):
    path, _, _ = tone
    for block in (777, 1000, 4096, 50000):
        analysis = analyze_wav(path, block_frames=block)
        assert len(analysis.silence_intervals) == 1
        interval = analysis.silence_intervals[0]
        assert interval.start_s == pytest.approx(0.5, abs=1e-3)
        assert interval.end_s == pytest.approx(0.75, abs=1e-3)


def test_stats_do_not_depend_on_block_size(tone):
    path, _, _ = tone
    a = analyze_wav(path, block_frames=333)
    b = analyze_wav(path, block_frames=8192)
    assert a.peak == pytest.approx(b.peak)
    assert a.rms == pytest.approx(b.rms, rel=1e-9)
    assert a.frames == b.frames


def test_silent_file_has_zero_rms_and_floor_dbfs(tmp_path):
    path = tmp_path / "silent.wav"
    sf.write(str(path), np.zeros(4800, dtype=np.float32), 48000, subtype="PCM_16")
    analysis = analyze_wav(path)
    assert analysis.rms == 0.0
    assert analysis.peak_dbfs <= -100.0
    assert analysis.silence_intervals


def test_alignment_with_video_grid(tone):
    path, _, _ = tone
    analysis = analyze_wav(path, block_frames=1024)
    from fractions import Fraction

    video = [Fraction(i, 30) for i in range(61)]
    result = align_audio(analysis, video, tolerance_ms=40.0)
    assert result["max_alignment_ms"] <= 40.0
    assert result["within_tolerance"] is True
    assert result["frame_period_ms"] == pytest.approx(1000 / 30, rel=1e-6)


def test_missing_and_empty_files_raise(tmp_path):
    with pytest.raises(MediaInputError):
        analyze_wav(tmp_path / "missing.wav")
    empty = tmp_path / "empty.wav"
    sf.write(str(empty), np.zeros(0, dtype=np.float32), 48000)
    with pytest.raises(MediaInputError):
        analyze_wav(empty)
    with pytest.raises(MediaInputError):
        analyze_wav(empty, block_frames=0)
