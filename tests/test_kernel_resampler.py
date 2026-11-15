import math

import numpy as np
import pytest

from audio_pipeline.kernel import (
    KernelLimitError,
    design_kernel,
    get_kernel,
)
from audio_pipeline.resampler import Resampler, output_frame_count, resample_array


def reference_resample(samples: np.ndarray, kernel) -> np.ndarray:
    data = np.asarray(samples, dtype=np.float64)
    mono = data.ndim == 1
    if mono:
        data = data[:, None]
    total = output_frame_count(len(data), kernel)
    result = np.zeros((total, data.shape[1]), dtype=np.float64)
    for n in range(total):
        product = n * kernel.M
        q, phase = divmod(product, kernel.L)
        for tap, offset in enumerate(kernel.offsets):
            index = q - int(offset)
            if 0 <= index < len(data):
                result[n] += kernel.table[phase, tap] * data[index]
    return result[:, 0] if mono else result


def test_kernel_contract_cache_and_limits():
    kernel = design_kernel(44100, 48000)
    assert kernel.L == 160 and kernel.M == 147
    assert kernel.total_taps <= 4096
    assert get_kernel(44100, 48000) is kernel
    np.testing.assert_allclose(kernel.table.sum(axis=1), 1.0, atol=1e-15)
    assert len(kernel.fingerprint) == 24
    assert design_kernel(22050, 48000).L == 320
    with pytest.raises(KernelLimitError):
        design_kernel(8000, 3_000_000)
    with pytest.raises(KernelLimitError):
        design_kernel(44100, 48000, taps=4097)


@pytest.mark.parametrize(
    "fs_in,fs_out", [(8000, 48000), (22050, 48000), (96000, 48000)]
)
def test_streaming_matches_whole_reference_and_length(fs_in, fs_out):
    rng = np.random.default_rng(17)
    samples = rng.uniform(-0.4, 0.4, 137)
    kernel = design_kernel(fs_in, fs_out)
    expected = reference_resample(samples, kernel)
    actual = resample_array(samples, fs_in, fs_out, block_size=13)
    assert abs(len(actual) - len(samples) * kernel.L / kernel.M) <= 1.0
    np.testing.assert_allclose(actual, expected, atol=1e-9, rtol=0)


def test_identity_silence_and_empty_blocks():
    samples = np.linspace(-0.75, 0.75, 100)
    np.testing.assert_array_equal(resample_array(samples, 48000, 48000), samples)
    silence = np.zeros((400, 2))
    assert np.all(resample_array(silence, 96000, 48000, block_size=37) == 0.0)
    stream = Resampler(8000, 48000)
    assert stream.push(np.empty((0, 2))).size == 0
    source = np.stack((np.arange(20.0), np.arange(20.0)), axis=1)
    result = stream.push(source)
    result = np.concatenate((result, stream.flush()))
    assert len(result) == 120


def test_stereo_channels_do_not_mix():
    fs_in, duration = 48000, 1.0
    time = np.arange(int(fs_in * duration)) / fs_in
    samples = np.stack(
        (0.3 * np.sin(2 * np.pi * 300 * time), 0.3 * np.sin(2 * np.pi * 3000 * time)),
        axis=1,
    )
    output = resample_array(samples, fs_in, 44100, block_size=733)
    out_time = np.arange(len(output)) / 44100

    def projection(channel, frequency):
        return abs(np.sum(channel * np.exp(-2j * np.pi * frequency * out_time)))

    left_cross = projection(output[:, 0], 3000) / projection(output[:, 0], 300)
    right_cross = projection(output[:, 1], 300) / projection(output[:, 1], 3000)
    assert 20 * math.log10(max(left_cross, right_cross)) < -120
