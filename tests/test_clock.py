"""Clock, wrap, drift and restart unit tests."""

from __future__ import annotations

from fractions import Fraction

import pytest

from mediasync.clock import (
    DEFAULT_WRAP_TICKS,
    TICKS_PER_SECOND,
    build_display_track,
    detect_restarts,
    fit_drift,
    parse_fps,
    samples_from_ticks,
    ticks_from_rate,
    ticks_from_samples,
    unwrap_timestamps,
    wrap_distance,
)
from mediasync.errors import InputError


def test_exact_rational_rates() -> None:
    ntsc = parse_fps("30000/1001")
    assert ntsc == Fraction(30000, 1001)
    assert ticks_from_rate(30000, ntsc) == int(Fraction(30000) / ntsc * TICKS_PER_SECOND)
    assert ticks_from_rate(1, Fraction(30)) == 3000
    assert ticks_from_rate(30, Fraction(30)) == TICKS_PER_SECOND
    assert parse_fps("29.97") == Fraction(2997, 100)


def test_sample_tick_roundtrip() -> None:
    for sample in (0, 1, 479, 48_000, 123_456):
        ticks = ticks_from_samples(sample, 48_000)
        assert abs(samples_from_ticks(ticks, 48_000) - sample) <= 1


@pytest.mark.parametrize("bad", ["", "abc", "0", "-30", "30/0", "1/-5"])
def test_bad_frame_rates_are_rejected(bad: str) -> None:
    with pytest.raises(InputError):
        parse_fps(bad)


def test_wrap_expansion_keeps_small_backwards_jumps() -> None:
    wrap = DEFAULT_WRAP_TICKS
    values = [wrap - 6000, wrap - 3000, 0, 3000, 3000, 1000]
    expanded = unwrap_timestamps(values, wrap=wrap)
    assert expanded[:4] == [wrap - 6000, wrap - 3000, wrap, wrap + 3000]
    # A small backward jump is not a wrap: it stays inside the same epoch.
    assert expanded[4] == wrap + 3000
    assert expanded[5] == wrap + 1000


def test_wrap_rejects_out_of_range_timestamps() -> None:
    with pytest.raises(InputError):
        unwrap_timestamps([DEFAULT_WRAP_TICKS])
    with pytest.raises(InputError):
        unwrap_timestamps([-1])


def test_wrap_distance_is_forward_only() -> None:
    assert wrap_distance(10, 4, 16) == 10
    assert wrap_distance(4, 10, 16) == 6


def test_drift_fit_recovers_a_known_clock_rate() -> None:
    raw = [int(round(index * 3000 * (1.0 + 120e-6))) for index in range(900)]
    ref = [index * 3000 for index in range(900)]
    fit = fit_drift(raw, ref, origin_ticks=0)
    assert fit.ppm == pytest.approx(120.0, abs=1.0)
    assert fit.max_abs_ticks <= 1
    assert fit.samples == 900


def test_drift_fit_needs_variation() -> None:
    with pytest.raises(InputError):
        fit_drift([1, 2, 3], [5, 5, 5], origin_ticks=0)
    with pytest.raises(InputError):
        fit_drift([1], [0], origin_ticks=0)


def test_restart_detection_flags_backward_jump() -> None:
    raw = [index * 3000 for index in range(10)]
    restarted = raw[:5] + [0, 3000, 6000]
    restarts = detect_restarts(restarted, nominal_step_ticks=3000)
    assert len(restarts) == 1
    assert restarts[0].index == 5
    assert restarts[0].kind == "backward-jump"


def test_display_track_is_strictly_monotonic_across_restart() -> None:
    raw = [index * 3000 for index in range(6)] + [0, 3000, 6000]
    track = build_display_track(raw, nominal_step_ticks=3000)
    display = track.display_ticks
    assert len(track.restarts) == 1
    assert all(b > a for a, b in zip(display, display[1:]))
    # Raw timestamps survive unchanged for provenance.
    assert [sample.raw_ticks for sample in track.samples] == raw


def test_display_track_clamps_absurd_forward_gap() -> None:
    raw = [0, 3000, 3000 + TICKS_PER_SECOND * 600]
    track = build_display_track(raw, nominal_step_ticks=3000)
    gaps = [
        b.display_ticks - a.display_ticks
        for a, b in zip(track.samples, track.samples[1:])
    ]
    assert max(gaps) <= max(3000 * 1000, 10 * TICKS_PER_SECOND)


def test_display_track_rejects_non_positive_step() -> None:
    with pytest.raises(InputError):
        build_display_track([0, 1], nominal_step_ticks=0)
