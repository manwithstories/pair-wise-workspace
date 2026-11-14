from __future__ import annotations

import numpy as np
import pytest

from mixer.errors import RenderError
from mixer.resample import SincResampler


@pytest.mark.parametrize(
    ("in_rate", "out_rate"),
    [(44100, 48000), (48000, 44100), (96000, 48000), (48000, 96000)],
)
def test_cross_chunk_resampling_is_sample_exact(in_rate: int, out_rate: int) -> None:
    rng = np.random.default_rng(42)
    source = rng.standard_normal((2, 12000)).astype(np.float32)
    target = (source.shape[1] * out_rate + in_rate // 2) // in_rate

    full = SincResampler(in_rate, out_rate, 2)
    full.set_target(target)
    reference = np.concatenate((full.push(source), full.flush()), axis=1)

    chunked = SincResampler(in_rate, out_rate, 2)
    chunked.set_target(target)
    parts = []
    for start in range(0, source.shape[1], 257):
        parts.append(chunked.push(source[:, start : start + 257]))
    parts.append(chunked.flush())
    streamed = np.concatenate(parts, axis=1)

    assert reference.shape == (2, target)
    assert np.array_equal(reference, streamed)
    assert np.all(np.isfinite(streamed))


def test_resampler_rejects_wrong_channel_layout() -> None:
    resampler = SincResampler(48000, 96000, 2)
    resampler.set_target(10)
    with pytest.raises(RenderError):
        resampler.push(np.zeros((1, 10), dtype=np.float32))


def test_bounded_flush_has_same_tail_as_full_flush() -> None:
    source = np.ones((1, 300), dtype=np.float32)
    target = 2400
    full = SincResampler(96000, 48000, 1)
    full.set_target(target)
    reference = np.concatenate((full.push(source), full.flush()), axis=1)

    bounded = SincResampler(96000, 48000, 1)
    bounded.set_target(target)
    parts = [bounded.push(source)]
    while bounded.produced < target:
        parts.append(bounded.flush(127))
    streamed = np.concatenate(parts, axis=1)
    assert streamed.shape == reference.shape
    assert np.array_equal(streamed, reference)
