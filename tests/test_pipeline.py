"""fieldcut pipeline tests: recovery, boundary and failure paths.

Each test builds deterministic synthetic media in a tmp directory, so runs are
reproducible and never touch real input/output locations.
"""

from __future__ import annotations

import threading

import numpy as np
import pytest

from fieldcut.align import align_cameras, cross_correlation_lag
from fieldcut.cli import main as cli_main
from fieldcut.errors import ErrorLog, UsageError
from fieldcut.ladder import encode_representation
from fieldcut.manifest import build_manifest, first_inconsistency
from fieldcut.orchestrator import run_job
from fieldcut.pipeline import (
    BoundedQueue,
    PipelineCancelled,
    PipelineState,
    QUEUE_CLOSED,
    run_stage,
)
from fieldcut.probe import probe_file
from fieldcut.segment import plan_segments
from fieldcut.timebase import (
    DEFAULT_WRAP_MODULUS,
    fit_drift,
    profile_frame_rate,
    unwrap_timestamps,
)

from synthesize import (
    CameraSpec,
    generate_camera,
    parse_defects,
    reference_rgb,
)


def _camera(tmp_path, name, duration=8.0, defects=("normal",), seed=7):
    info = generate_camera(
        CameraSpec(name, duration, tuple(defects), seed),
        str(tmp_path / "src"))
    return info["path"]


def _probes(tmp_path, specs):
    probes = []
    for name, defects in specs:
        path = _camera(tmp_path, name, defects=parse_defects(defects))
        probes.append(probe_file(path, name, ErrorLog()))
    return probes


# --------------------------------------------------------------------------
# timebase: wrap vs regression, drift confidence, VFR
# --------------------------------------------------------------------------

def test_unwrap_distinguishes_wrap_from_regression():
    m = DEFAULT_WRAP_MODULUS
    res = unwrap_timestamps([m - 100, m - 10, 5, 20], m)
    assert res.wrap_indices == [2]
    assert res.values == sorted(res.values)
    back = unwrap_timestamps([1000, 900, 1100])
    assert back.wrap_indices == []
    assert back.regression_indices == [1]
    assert back.values[1] == 900


def test_drift_fit_sign_and_confidence():
    ref = [i * 3750 for i in range(120)]
    capture = [int(r / 1.001) for r in ref]   # slow oscillator -> positive ppm
    fit = fit_drift(capture, ref)
    assert fit.rate_ppm > 500
    assert fit.ci_half_width_ppm >= 0
    assert fit.residual_rms_ms < 1.0


def test_vfr_profile_flags_variable_cadence():
    constant = profile_frame_rate([0, 3750, 7500, 11250])
    assert constant.variable is False
    variable = profile_frame_rate([0, 3750, 7000, 11250])
    assert variable.variable is True


def test_parse_defects_rejects_unknown():
    with pytest.raises(ValueError):
        parse_defects("drift,bogus")


# --------------------------------------------------------------------------
# bounded queue: backpressure and cancellation
# --------------------------------------------------------------------------

def test_bounded_queue_backpressure_blocks_producer():
    q = BoundedQueue(2)
    cancel = threading.Event()
    done = threading.Event()

    def fill():
        try:
            for i in range(5):
                q.put(i, cancel)
        except PipelineCancelled:
            pass
        done.set()

    t = threading.Thread(target=fill)
    t.start()
    t.join(timeout=0.3)
    assert t.is_alive()                      # blocked on the full queue
    assert q.peak_size <= 2
    assert q.get(cancel) == 0
    cancel.set()                            # release any remaining blocked put
    t.join(timeout=1.0)
    assert not t.is_alive()


def test_cancelled_put_raises():
    q = BoundedQueue(1)
    cancel = threading.Event()
    q.put("x", cancel)                  # queue now full
    cancel.set()
    with pytest.raises(PipelineCancelled):
        q.put("y", cancel)             # cancelled -> never blocks, raises


def test_stage_chain_processes_and_closes():
    inq, outq = BoundedQueue(4), BoundedQueue(4)
    state = PipelineState(threading.Event(), ErrorLog())
    run_stage("inc", lambda item, _s: [item + 1], inq, outq, state)
    for v in (1, 2, 3):
        inq.put(v, state.cancel)
    inq.close(state.cancel)
    out = []
    while True:
        item = outq.get(state.cancel)
        if item is QUEUE_CLOSED:
            break
        out.append(item)
    state.shutdown()
    assert out == [2, 3, 4]
    assert not any(th.is_alive() for th in state.threads.values())


# --------------------------------------------------------------------------
# probe: damage, truncation and recovery
# --------------------------------------------------------------------------

def test_probe_detects_bad_packet_and_resyncs(tmp_path):
    path = _camera(tmp_path, "bp", defects=("bad_packet",))
    result = probe_file(path, "bp", ErrorLog())
    video = result.videos[0]
    assert video.damaged_ranges
    # frames after the hole remain decodable (resync, not total loss)
    assert video.frame_decoded > video.packet_count * 0.8


def test_probe_detects_truncation_and_keeps_head(tmp_path):
    path = _camera(tmp_path, "tr", duration=12.0,
                   defects=("truncated", "wrap"))
    result = probe_file(path, "tr", ErrorLog())
    video = result.videos[0]
    assert video.truncated is True
    assert video.readable_end_ticks > 0
    assert video.wrap_indices                  # wrap still reconstructed


# --------------------------------------------------------------------------
# alignment + segmentation
# --------------------------------------------------------------------------

def test_alignment_marks_damaged_camera_review_ranges(tmp_path):
    probes = _probes(tmp_path, [
        ("a", "normal"), ("b", "truncated,bad_packet")])
    alignment = align_cameras(probes, ErrorLog())
    damaged = next(c for c in alignment.cameras if c.camera == "b")
    assert damaged.review_ranges
    healthy = next(c for c in alignment.cameras if c.camera == "a")
    assert not healthy.review_ranges


def test_segment_plan_common_cuts_and_damaged_flag(tmp_path):
    probes = _probes(tmp_path, [
        ("a", "normal"), ("b", "truncated,bad_packet")])
    log = ErrorLog()
    alignment = align_cameras(probes, log)
    plan = plan_segments(probes, alignment, log)
    assert plan.segments
    assert all(s.end_ticks > s.start_ticks for s in plan.segments)
    assert any(s.damaged for s in plan.segments)


def test_cross_correlation_lag_sign():
    sr = 44100
    rng = np.random.default_rng(0)
    ref = rng.normal(0, 1, sr // 4).astype(np.float32)
    delayed = np.concatenate([np.zeros(31, np.float32), ref])[: len(ref)]
    assert cross_correlation_lag(delayed, ref, sr) == 31


# --------------------------------------------------------------------------
# end-to-end orchestration, resume and manifest
# --------------------------------------------------------------------------

def _run(tmp_path, name, defects, duration, cameras, bitrates=(800000,)):
    cams = []
    for i in range(cameras):
        d = "normal" if i == 0 else defects
        path = _camera(tmp_path, f"{name}{i}", duration=duration,
                       defects=parse_defects(d), seed=50 + i)
        cams.append((f"{name}{i}", path))
    out = str(tmp_path / f"out-{name}")
    return run_job(cams, out, list(bitrates), reference_rgb), out


def test_healthy_job_produces_consistent_manifest(tmp_path):
    result, _ = _run(tmp_path, "ok", "drift,wrap,vfr", 12.0, 2)
    assert result.manifest.ok
    assert first_inconsistency(result.manifest) is None
    assert all(a.continuous for a in result.audio)
    for rep in result.representations:
        errs = [abs(m.bitrate_error_pct) for m in rep.segments
                if m.actual_bps]
        assert max(errs) <= 15.0


def test_damaged_job_recovers_segments_and_flags_ranges(tmp_path):
    result, _ = _run(tmp_path, "dam", "truncated,bad_packet,wrap",
                     12.0, 2)
    assert result.recovered_segments >= 1
    assert any(s.damaged for s in result.plan.segments)
    # recoverable region still yields a usable manifest
    assert result.manifest.ok


def test_resume_reuses_valid_segments(tmp_path):
    result1, out = _run(tmp_path, "res", "normal", 8.0, 1)
    before = result1.recovered_segments
    result2, _ = _run(tmp_path, "res", "normal", 8.0, 1)
    assert result2.manifest.ok
    assert result2.recovered_segments == before


def test_psnr_uncomputable_reason_on_empty_segment(tmp_path):
    path = _camera(tmp_path, "p", defects=("normal",))
    probe = probe_file(path, "p", ErrorLog())
    log = ErrorLog()
    alignment = align_cameras([probe], log)
    plan = plan_segments([probe], alignment, log)
    rep = encode_representation(
        probe, alignment, plan, 0, 800000,
        str(tmp_path / "enc"), log, reference_rgb)
    assert all(m.psnr_db is not None or m.psnr_reason
               for m in rep.segments)


# --------------------------------------------------------------------------
# CLI exit codes
# --------------------------------------------------------------------------

def test_cli_usage_error_returns_1(tmp_path):
    code = cli_main(["demo", "--output", str(tmp_path / "o"),
                     "--duration", "4", "--cameras", "1",
                     "--inject", "not_a_defect"])
    assert code == 1


def test_cli_demo_success_returns_0(tmp_path):
    code = cli_main(["demo", "--output", str(tmp_path / "o"),
                     "--duration", "8", "--cameras", "2",
                     "--bitrates", "800000", "--inject", "drift,wrap"])
    assert code == 0


def test_cli_process_missing_file_returns_1(tmp_path):
    code = cli_main(["process", "--output", str(tmp_path / "o"),
                     "--input", str(tmp_path / "missing.mp4")])
    assert code == 1
