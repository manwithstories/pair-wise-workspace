"""Timestamp repair: wraps, duplicates, reordering, gaps and provenance."""

from __future__ import annotations

from fractions import Fraction

import pytest

from mediacore.clock import (
    DEFAULT_WRAP_PERIOD, StampDecision, TimeBase, align_audio_to_video,
    repair_timestamps,
)


def test_timebase_is_exact():
    tb = TimeBase(1, 90000)
    assert tb.seconds(90001) == Fraction(90001, 90000)
    assert tb.ticks(Fraction(1, 3)) == 30000
    assert TimeBase(1, 25).rescale(25, TimeBase(1, 90000)) == 90000
    with pytest.raises(ValueError):
        TimeBase(0, 1)


def test_wrap_is_restored_without_changing_values_before_it():
    period = DEFAULT_WRAP_PERIOD
    raw = [period - 4500, period - 3000, period - 1500, 0, 1500, 3000]
    repair = repair_timestamps(raw, nominal_step=1500)
    assert repair.wrap_count == 1
    fixed = repair.fixed_ticks()
    assert fixed == sorted(fixed)
    assert fixed[3] - fixed[2] == 1500
    assert [d.fixed - period for d in repair.decisions[3:]] == [0, 1500, 3000]
    assert repair.decisions[3].reason == "wrapped"


def test_short_reorder_run_restores_value_order():
    raw = [0, 1, 3, 2, 4, 5]
    repair = repair_timestamps(raw, nominal_step=1, wrap_period=None)
    assert repair.fixed_ticks() == [0, 1, 2, 3, 4, 5]
    reasons = [d.reason for d in repair.decisions]
    assert reasons.count("reordered") == 2
    assert not any(d.uncertain for d in repair.decisions)


def test_duplicate_keeps_both_occurrences_and_marks_provenance():
    repair = repair_timestamps([0, 1, 1, 2], nominal_step=1,
                               wrap_period=None, strict=False)
    decisions = repair.decisions
    assert decisions[2].reason == "duplicate"
    assert decisions[2].duplicate_of == 1
    assert decisions[2].raw == 1
    assert decisions[2].uncertain
    assert decisions[2].window_lo == decisions[1].fixed


def test_missing_timestamp_is_filled_and_marked_uncertain():
    repair = repair_timestamps([0, 1, None, 3], nominal_step=1, wrap_period=None)
    filled = repair.decisions[2]
    assert filled.raw is None
    assert filled.reason == "missing_filled"
    assert filled.uncertain
    assert filled.window_lo <= filled.fixed <= filled.window_hi
    assert any(i.code == "timestamp_missing" for i in repair.issues)


def test_large_discontinuity_keeps_values_and_marks_interval():
    repair = repair_timestamps([0, 1, 900_000, 900_001], nominal_step=1,
                               wrap_period=None)
    gap = repair.decisions[2]
    assert gap.reason == "gap"
    assert gap.uncertain
    assert (gap.window_lo, gap.window_hi) == (2, 900_000)
    assert repair.uncertain_count == 1


def test_fixed_timeline_is_always_monotonic():
    raw = [5, 3, None, 3, 9, 2, 12, 12, 11]
    repair = repair_timestamps(raw, nominal_step=2, wrap_period=None)
    fixed = repair.fixed_ticks()
    assert all(b > a for a, b in zip(fixed, fixed[1:]))


def test_repair_summary_counts_reasons():
    repair = repair_timestamps([0, 1, 1, None], nominal_step=1,
                               wrap_period=None, strict=False)
    summary = repair.summary()
    assert summary["records"] == 4
    assert summary["repairs"]["duplicate"] == 1
    assert summary["repairs"]["missing_filled"] == 1
    assert summary["uncertain"] == 2


def test_audio_alignment_detects_offset_and_spacing():
    video = [Fraction(i, 25) for i in range(26)]
    audio = [Fraction(i, 10) + Fraction(2, 1000) for i in range(6)]
    result = align_audio_to_video(audio, video, sample_rate=48000)
    assert abs(result["offset_ms"] - 2.0) < 0.5
    assert result["max_alignment_ms"] <= 40.0
    assert result["sample_offset"] == 96


def test_empty_inputs_are_handled():
    repair = repair_timestamps([])
    assert repair.decisions == []
    result = align_audio_to_video([], [], sample_rate=48000)
    assert result["max_alignment_ms"] == 0.0
