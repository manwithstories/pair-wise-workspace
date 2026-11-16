"""BS.1770-4 loudness tests: reference tones, gating, silence, true peak."""

from __future__ import annotations

import math
from array import array

import pytest

from audio_tool.kweight import (
    KWeighting,
    _highpass_coefficients,
    _shelf_coefficients,
)
from audio_tool.loudness import (
    LoudnessMeter,
    _lufs_from_energy,
    lufs_to_linear,
)
from audio_tool.model import ABSOLUTE_GATE


def measure(channels: int, rate: int, data: list[array], block: int = 8192):
    meter = LoudnessMeter(channels, rate, (1.0,) * channels)
    total = len(data[0])
    for start in range(0, total, block):
        stop = min(start + block, total)
        meter.process([plane[start:stop] for plane in data], stop - start)
    return meter.finish()


def tone(frequency: float, seconds: float, rate: int, amplitude: float) -> array:
    frames = int(round(seconds * rate))
    return array("d", [amplitude * math.sin(2 * math.pi * frequency * i / rate)
                       for i in range(frames)])


def test_kweighting_matches_itu_reference_coefficients():
    b, a = _shelf_coefficients(48000)
    assert b == pytest.approx((1.53512485958697, -2.69169618940638,
                               1.19839281085285), abs=1e-12)
    assert a == pytest.approx((-1.69065929318241, 0.73248077421585), abs=1e-11)
    b, a = _highpass_coefficients(48000)
    assert b == pytest.approx((1.0, -2.0, 1.0))
    assert a == pytest.approx((-1.99004745483398, 0.99007225036621), abs=1e-12)


def test_kweighting_response_shape():
    weighting = KWeighting(1, 48000)
    low = 20 * math.log10(weighting.k_weighted_gain(20.0, 48000))
    mid = 20 * math.log10(weighting.k_weighted_gain(1000.0, 48000))
    high = 20 * math.log10(weighting.k_weighted_gain(10000.0, 48000))
    assert low < -10.0            # high-pass removes rumble
    assert -1.0 < mid < 1.0       # near unity at the reference tone
    assert high > mid + 2.0       # shelf lifts the presence region


def test_reference_tone_lufs():
    # A -20 dBFS 1 kHz stereo sine measures -20 LUFS (BS.1770 conformance).
    data = tone(1000.0, 5.0, 48000, 0.1)
    stats = measure(2, 48000, [data, data])
    assert stats.lufs == pytest.approx(-20.0, abs=0.1)


def test_level_scaling_is_linear_in_db():
    loud = tone(1000.0, 3.0, 48000, 0.2)
    quiet = tone(1000.0, 3.0, 48000, 0.02)
    a = measure(1, 48000, [loud]).lufs
    b = measure(1, 48000, [quiet]).lufs
    assert a - b == pytest.approx(20.0, abs=0.05)


def test_duplicating_a_mono_signal_to_stereo_raises_loudness_by_3lu():
    # BS.1770 sums channel energies, so two identical channels are +3.01 LU
    # relative to the single channel.  This is the standard's behaviour, not a
    # bug: doubling the channels doubles the total acoustic power.
    data = tone(1000.0, 3.0, 48000, 0.1)
    mono = measure(1, 48000, [data]).lufs
    stereo = measure(2, 48000, [data, data]).lufs
    assert stereo - mono == pytest.approx(3.0103, abs=0.01)


def test_relative_gate_ignores_quiet_passages():
    rate = 48000
    loud = tone(1000.0, 4.0, rate, 0.2)
    silence = array("d", [0.0] * (rate * 4))
    # A loud passage surrounded by silence must measure close to the loud part.
    combined = silence + loud + silence
    gated = measure(1, rate, [combined])
    pure = measure(1, rate, [loud])
    assert gated.lufs == pytest.approx(pure.lufs, abs=0.5)


def test_absolute_gate_rejects_very_quiet_material():
    data = tone(1000.0, 3.0, 48000, 1e-5)
    stats = measure(1, 48000, [data])
    assert stats.lufs is None
    assert stats.digital_silence is False


def test_digital_silence_is_explainable():
    silence = array("d", [0.0] * 48000)
    stats = measure(2, 48000, [silence, silence])
    assert stats.lufs is None
    assert stats.digital_silence is True
    assert stats.peak == 0.0
    assert stats.rms == 0.0
    payload = stats.to_dict()
    assert payload["lufs"] is None
    assert payload["rms_dbfs"] == -240.0     # finite, not -inf/NaN
    assert payload["true_peak_dbtp"] == -240.0


def test_very_short_material_still_measures():
    data = tone(1000.0, 0.05, 48000, 0.1)     # 50 ms, below one 400 ms block
    stats = measure(1, 48000, [data])
    assert stats.lufs is not None
    assert stats.frames == len(data)


def test_block_size_does_not_change_loudness():
    rate = 48000
    data = array("d", [(0.1 if (i // rate) % 2 else 0.03)
                       * math.sin(2 * math.pi * 440 * i / rate)
                       for i in range(rate * 4)])
    reference = None
    for block in (16384, 4096, 999, 64, 7):
        stats = measure(1, rate, [data], block=block)
        if reference is None:
            reference = stats.lufs
            continue
        assert stats.lufs == pytest.approx(reference, abs=1e-9)


def test_rms_and_peak_are_reported():
    data = tone(1000.0, 1.0, 48000, 0.5)
    stats = measure(1, 48000, [data])
    assert stats.peak == pytest.approx(0.5, abs=0.002)
    assert stats.rms == pytest.approx(0.5 / math.sqrt(2), abs=0.002)


def test_clipping_counts_full_scale_samples():
    rate = 48000
    data = array("d", [0.0] * 100)
    data.extend([1.0] * 10)
    data.extend([-1.0] * 5)
    data.extend([0.5] * 100)
    stats = measure(1, rate, [data])
    assert stats.clipped_samples == 15


def test_true_peak_exceeds_sample_peak_between_samples():
    # A tone offset so its crest falls between samples: TP > sample peak.
    rate = 48000
    data = array("d", [math.sin(2 * math.pi * 997 * i / rate + 1.0)
                       for i in range(rate)])
    stats = measure(1, rate, [data])
    assert stats.true_peak > stats.peak
    assert stats.true_peak == pytest.approx(1.0, abs=0.01)


def test_true_peak_never_underreads_sample_peak():
    rate = 48000
    data = array("d", [0.9 * math.sin(2 * math.pi * 100 * i / rate)
                       for i in range(rate)])
    stats = measure(1, rate, [data])
    # Original samples are candidates, so true peak can never under-read the
    # sample peak.
    assert stats.true_peak >= stats.peak


def test_lufs_energy_roundtrip():
    for lufs in (-60.0, -23.0, -16.0, -3.0):
        energy = lufs_to_linear(lufs)
        assert _lufs_from_energy(energy) == pytest.approx(lufs, abs=1e-9)


def test_absolute_gate_constant():
    # -70 LUFS means -70 = -0.691 + 10*log10(E), so E = 10^(-69.309/10).
    assert 10 * math.log10(ABSOLUTE_GATE) == pytest.approx(-69.309, abs=1e-9)
    assert _lufs_from_energy(ABSOLUTE_GATE) == pytest.approx(-70.0, abs=1e-9)


def test_momentary_and_short_term_maxima_are_reported():
    rate = 48000
    quiet = tone(1000.0, 1.0, rate, 0.01)
    loud = tone(1000.0, 1.0, rate, 0.3)
    stats = measure(1, rate, [quiet + loud])
    assert stats.momentary_max is not None
    assert stats.short_term_max is not None
    assert stats.momentary_max > stats.lufs
