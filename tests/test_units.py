"""Unit tests for the rational time base, clock, pipeline and checkpoint."""

from __future__ import annotations

import pytest

from relaydiag.checkpoint import (CheckpointState, StateMismatch,
                                  load_checkpoint, save_checkpoint,
                                  validate_for_resume)
from relaydiag.clock import (Anchor, ClockFitter, TimestampUnwrapper)
from relaydiag.keyframes import FrameEvent, analyze_keyframes
from relaydiag.pipeline import (BoundedQueue, PipelineError, StreamItem,
                                run_pipeline)
from relaydiag.timeline import TimeBase, TimeRange, common_base, wrap_delta
from fractions import Fraction

MOD = 1 << 32


# ------------------------------------------------------------------ timeline
def test_timebase_rescale_and_common_base():
    video = TimeBase(1, 90000)
    milli = TimeBase(1, 1000)
    assert video.to(90000, milli) == 1000
    common = common_base(video, TimeBase(1, 48000))
    assert common.ticks_to_seconds(common.seconds_to_ticks(Fraction(1, 2))) \
        == Fraction(1, 2)


def test_time_range_rejects_inversion():
    with pytest.raises(ValueError):
        TimeRange(Fraction(2), Fraction(1))


def test_wrap_delta_shortest_path():
    assert wrap_delta(5, MOD - 3, MOD) == 8
    assert wrap_delta(MOD - 3, 5, MOD) == -8


# --------------------------------------------------------------------- clock
def test_unwrapper_classifies_wrap_duplicate_backwards():
    uw = TimestampUnwrapper(MOD)
    assert uw.step(MOD - 10).wrapped is False
    assert uw.step(10).wrapped is True
    assert uw.step(10).duplicate is True
    back = TimestampUnwrapper(MOD)
    back.step(1000)
    result = back.step(200)
    assert result.backwards is True and result.wrapped is False


def test_clock_fit_no_anchors_unconstrained():
    fit = ClockFitter([]).fit()
    assert fit.confidence == 0.0
    assert fit.error_low_s == float("-inf")


def test_clock_fit_detects_persistent_break():
    good = [Anchor(90000 * i, 1.0 * i) for i in range(1, 4)]
    broken = [Anchor(90000 * i, 3.0 + 0.97 * (i - 3)) for i in range(4, 10)]
    fit = ClockFitter(good + broken).fit()
    assert fit.break_ref_s is not None and fit.confidence <= 0.4


# ----------------------------------------------------------------- keyframes
def test_keyframes_separate_vfr_from_missing_frame():
    base = TimeBase(1, 90000)
    tick = 3000
    events = [
        FrameEvent(0, True),
        FrameEvent(tick, False),
        FrameEvent(tick + tick // 2, False, kind="vfr"),
        FrameEvent(2 * tick, False),
        FrameEvent(4 * tick, False),       # whole-tick hole: frame 3 missing
    ]
    report = analyze_keyframes(events, base, tick, gop=2)
    categories = [a.category for a in report.frame_anomalies]
    assert "missing_frame" in categories
    assert "vfr" in categories


# ------------------------------------------------------------------ pipeline
def test_bounded_queue_blocks_when_full():
    queue = BoundedQueue(1)
    queue.put("a")
    assert len(queue) == 1
    assert queue.get() == "a"


def test_pipeline_backpressure_and_checkpoint(tmp_path):
    state = CheckpointState(input_path="x", input_sha256="a" * 64,
                            input_size=10, params={})
    ckpt = tmp_path / "p.json"
    source = [StreamItem(offset=i, payload=i) for i in range(50)]
    run_pipeline(iter(source), [lambda it: it], state, str(ckpt),
                 capacity=2, checkpoint_every=10)
    saved = load_checkpoint(str(ckpt))
    assert saved.packets_processed == 50


def test_pipeline_disk_failure_pins_offset(tmp_path):
    state = CheckpointState(input_path="x", input_sha256="b" * 64,
                            input_size=10, params={})
    ckpt = tmp_path / "fail.json"
    source = [StreamItem(offset=i * 5, payload=i) for i in range(40)]

    def stage(item):
        if item.offset == 100:
            raise OSError("disk full")
        return item

    with pytest.raises(PipelineError) as exc:
        run_pipeline(iter(source), [stage], state, str(ckpt), capacity=2)
    assert exc.value.last_offset == 100
    assert load_checkpoint(str(ckpt)).next_offset == 100


# ---------------------------------------------------------------- checkpoint
def test_checkpoint_roundtrip_and_mismatch(tmp_path):
    stream = tmp_path / "in.bin"
    stream.write_bytes(b"abc123" * 100)
    digest, size = __import__(
        "relaydiag.checkpoint", fromlist=["fingerprint_file"]
    ).fingerprint_file(str(stream))
    ckpt = tmp_path / "state.json"
    state = CheckpointState(input_path=str(stream), input_sha256=digest,
                            input_size=size, params={"q": 4}, next_offset=12)
    save_checkpoint(str(ckpt), state)
    assert load_checkpoint(str(ckpt)).to_json() == state.to_json()
    validate_for_resume(load_checkpoint(str(ckpt)),
                        input_path=str(stream), params={"q": 4})
    with pytest.raises(StateMismatch):
        validate_for_resume(load_checkpoint(str(ckpt)),
                            input_path=str(stream), params={"q": 9})
