"""Resampler kernel tests: accuracy, alias rejection, length, streaming."""

from __future__ import annotations

import math
from array import array

import pytest

from audio_tool.io import AudioBlock
from audio_tool.model import ResampleConfig, UsageError
from audio_tool.resampler import PolyphaseResampler, kaiser_beta_for


def resample(data: array, rate_in: int, rate_out: int, block: int = 4096,
             config: ResampleConfig | None = None) -> array:
    kernel = PolyphaseResampler(config or ResampleConfig(), rate_in, rate_out)
    stream = kernel.stream()
    out: list[float] = []
    for start in range(0, len(data), block):
        chunk = data[start:start + block]
        for produced in stream.push(AudioBlock([chunk], len(chunk))):
            out.extend(produced.planes[0])
    for produced in stream.flush():
        out.extend(produced.planes[0])
    return array("d", out)


def sine(frequency: float, seconds: float, rate: int, amplitude: float = 0.5) -> array:
    frames = int(round(seconds * rate))
    return array("d", [amplitude * math.sin(2 * math.pi * frequency * i / rate)
                       for i in range(frames)])


def dominant_frequency(signal: array, rate: int, skip: int = 4000) -> float:
    mid = signal[skip:len(signal) - skip]
    crossings = sum(1 for i in range(1, len(mid)) if mid[i - 1] < 0 <= mid[i])
    return crossings * rate / len(mid)


@pytest.mark.parametrize("rate_in,rate_out", [
    (44100, 48000), (48000, 44100), (96000, 48000),
    (48000, 96000), (8000, 48000), (48000, 8000), (192000, 44100),
])
def test_length_is_exact(rate_in, rate_out):
    data = sine(1000.0, 0.5, rate_in)
    kernel = PolyphaseResampler(ResampleConfig(), rate_in, rate_out)
    result = resample(data, rate_in, rate_out)
    assert len(result) == kernel.output_length(len(data))


def test_dominant_frequency_uses_zero_crossings():
    signal = sine(500.0, 0.5, 48000, amplitude=0.3)
    assert dominant_frequency(signal, 48000) == pytest.approx(500, abs=1)


@pytest.mark.parametrize("rate_in,rate_out", [
    (44100, 48000), (48000, 44100), (96000, 48000), (48000, 8000),
])
def test_passband_tone_survives(rate_in, rate_out):
    data = sine(1000.0, 0.5, rate_in, amplitude=0.5)
    result = resample(data, rate_in, rate_out)
    # Skip the filter's edge transients, scaled to the output rate.
    edge = max(200, rate_out // 20)
    mid = result[edge:len(result) - edge]
    assert mid, "resampled output has no interior samples"
    peak = max(abs(value) for value in mid)
    assert peak == pytest.approx(0.5, abs=0.01)
    assert dominant_frequency(result, rate_out, skip=edge) == pytest.approx(
        1000.0, abs=2.0)


def test_stopband_is_rejected():
    # 30 kHz at 96 kHz sits above the 48 kHz target's Nyquist.
    data = sine(30000.0, 0.5, 96000, amplitude=0.5)
    result = resample(data, 96000, 48000)
    mid = result[4000:len(result) - 4000]
    peak = max(abs(value) for value in mid)
    assert peak < 1e-3


def test_dc_gain_is_unity():
    data = array("d", [0.25] * 24000)
    result = resample(data, 48000, 44100)
    mid = result[2000:len(result) - 2000]
    assert max(mid) == pytest.approx(0.25, abs=1e-3)
    assert min(mid) == pytest.approx(0.25, abs=1e-3)


def test_identical_rates_are_transparent():
    data = sine(997.0, 0.1, 48000, amplitude=0.3)
    result = resample(data, 48000, 48000)
    assert len(result) == len(data)
    for original, produced in zip(data, result):
        assert produced == pytest.approx(original, abs=1e-6)


def test_multichannel_channels_stay_separate():
    left = sine(500.0, 0.5, 44100, amplitude=0.4)
    right = sine(1200.0, 0.5, 44100, amplitude=0.2)
    kernel = PolyphaseResampler(ResampleConfig(), 44100, 48000)
    stream = kernel.stream()
    produced = stream.push(AudioBlock([left, right], len(left)))
    produced += stream.flush()
    out_left = [v for block in produced for v in block.planes[0]]
    out_right = [v for block in produced for v in block.planes[1]]
    assert max(abs(v) for v in out_left) == pytest.approx(0.4, abs=0.01)
    assert max(abs(v) for v in out_right) == pytest.approx(0.2, abs=0.01)
    # Channels must not bleed: each output's tone matches its own frequency.
    assert dominant_frequency(array("d", out_left), 48000) == pytest.approx(
        500, abs=3)
    assert dominant_frequency(array("d", out_right), 48000) == pytest.approx(
        1200, abs=3)


def test_channels_do_not_mix():
    left = sine(500.0, 0.2, 48000, amplitude=0.4)
    right = array("d", [0.0] * len(left))
    kernel = PolyphaseResampler(ResampleConfig(), 48000, 44100)
    stream = kernel.stream()
    produced = stream.push(AudioBlock([left, right], len(left)))
    produced += stream.flush()
    right_out = [v for block in produced for v in block.planes[1]]
    assert max(abs(v) for v in right_out) < 1e-12


def test_block_size_does_not_change_output():
    data = sine(1000.0, 0.2, 44100)
    reference = None
    for block in (1, 7, 333, 4096, 100000):
        result = resample(data, 44100, 48000, block=block)
        if reference is None:
            reference = result
            continue
        assert len(result) == len(reference)
        worst = max(abs(a - b) for a, b in zip(result, reference))
        assert worst < 1e-9


def test_configurable_cutoff_lowers_passband():
    # With a 6 kHz cutoff a 10 kHz tone must be attenuated.
    config = ResampleConfig(passband_hz=6000.0, half_taps=64)
    data = sine(10000.0, 0.3, 48000, amplitude=0.5)
    result = resample(data, 48000, 96000, config=config)
    mid = result[4000:len(result) - 4000]
    assert max(abs(v) for v in mid) < 0.02


def test_configuration_is_validated():
    with pytest.raises(UsageError):
        PolyphaseResampler(ResampleConfig(half_taps=1), 44100, 48000)
    with pytest.raises(UsageError):
        PolyphaseResampler(ResampleConfig(phases=2), 44100, 48000)
    with pytest.raises(UsageError):
        # passband above the lower Nyquist
        PolyphaseResampler(ResampleConfig(passband_hz=30000.0), 44100, 96000)


def test_kaiser_beta_follows_attenuation():
    assert kaiser_beta_for(20.0) == 0.0
    assert 0.0 < kaiser_beta_for(60.0) < kaiser_beta_for(90.0)


def test_kernel_signature_records_design():
    kernel = PolyphaseResampler(ResampleConfig(half_taps=32, phases=64),
                                44100, 48000)
    signature = kernel.signature()
    assert signature["up"] == 160 and signature["down"] == 147
    assert signature["phases"] == 64
    assert signature["taps"] == 65
