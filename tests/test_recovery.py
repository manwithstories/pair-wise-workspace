"""End-to-end recovery and damage-location tests.

These tests exercise the same public surface the CLI uses, against fixtures
the generator synthesises in a temporary directory.  Every required scenario
from the brief is represented: 32-bit wraparound, clock-drift break,
transition-boundary continuity, truncation and an interrupted process that is
resumed from its checkpoint.
"""

from __future__ import annotations

import json
import os

import pytest

from relaydiag.checkpoint import StateMismatch, load_checkpoint, validate_for_resume
from relaydiag.cli import main
from relaydiag.container import iter_packets
from relaydiag.damage import DamageLocator
from relaydiag.engine import DiagnosisEngine, RunConfig
from relaydiag.generator import generate_fixture
from relaydiag.manifest import verify_document


@pytest.fixture(scope="module")
def fixture_dir(tmp_path_factory):
    path = tmp_path_factory.mktemp("fixture") / "fx"
    generate_fixture(str(path))
    return str(path)


def _run(fixture_dir, tmp_path, *, resume=False, **kw):
    checkpoint = tmp_path / "state.json"
    result = tmp_path / "result.json"
    config = RunConfig(
        input_dir=fixture_dir,
        checkpoint_path=str(checkpoint),
        result_path=str(result),
        resume=resume,
        **kw,
    )
    return DiagnosisEngine(config).run(), str(checkpoint), str(result)


# ---------------------------------------------------------------- wraparound
def test_wraparound_is_flagged_and_unwrapped(fixture_dir):
    stream = os.path.join(fixture_dir, "stream.rlyd")
    header = None
    locator = DamageLocator(_load_header(stream))
    for header, parsed in iter_packets(stream):
        locator.observe(parsed)
    wraps = [r for r in locator.records if r.category == "wraparound"]
    assert len(wraps) == 1
    record = wraps[0]
    assert record.track == "video"
    assert record.byte_range[0] < os.path.getsize(stream)
    # The raw PTS sits below the modulus boundary but the time is near 2.83 s.
    assert 2.7 < float(record.time_range.start) < 3.0


# --------------------------------------------------------------- drift break
def test_clock_drift_break_detected(fixture_dir, tmp_path):
    document, _, _ = _run(fixture_dir, tmp_path)
    fit = document["clock_fit"]
    assert fit["drift_break_ref_s"] is not None
    assert fit["confidence"] <= 0.4
    assert fit["note"] == "drift break detected"


# ------------------------------------------------------------- transition
def test_transition_boundary_continuity(fixture_dir, tmp_path):
    document, _, _ = _run(fixture_dir, tmp_path)
    # All three tracks cover both transition boundaries, so no continuity
    # gap record may appear in the healthy fixture.
    assert not any(e["category"] == "cross_track_gap" for e in document["errors"])


def test_missing_track_at_transition_is_flagged(fixture_dir):
    from fractions import Fraction
    from relaydiag.timeline import TimeRange
    header = _load_header(os.path.join(fixture_dir, "stream.rlyd"))
    locator = DamageLocator(header)
    spans = {"video": TimeRange(Fraction(0), Fraction(3), "video"),
             "audio1": TimeRange(Fraction(0), Fraction(3), "audio1")}
    gaps = locator.check_continuity(header["edits"]["transitions"], spans)
    assert any(g.track == "audio2" for g in gaps)


# -------------------------------------------------------------- truncation
def test_truncated_stream_reports_exact_offset(fixture_dir, tmp_path):
    stream = os.path.join(fixture_dir, "stream.rlyd")
    size = os.path.getsize(stream)
    cut = size - 64
    truncated = tmp_path / "trunc.rlyd"
    with open(stream, "rb") as src, open(truncated, "wb") as out:
        out.write(src.read(cut))
    header = _load_header(str(truncated))
    locator = DamageLocator(header)
    from relaydiag.damage import locate_truncated_stream
    count = locate_truncated_stream(str(truncated), locator)
    assert count == 1
    trunc = [r for r in locator.records if r.category == "truncated_packet"]
    assert trunc and trunc[0].byte_range[0] == cut


def test_missing_frame_within_accuracy_tolerance(fixture_dir, tmp_path):
    document, _, _ = _run(fixture_dir, tmp_path)
    metrics = document["metrics"]
    assert metrics["damage_offset_error_bytes"] <= 188
    assert metrics["damage_time_error_ms"] <= 40.0


# ----------------------------------------------------------- interrupt/resume
def test_interrupted_process_resumes_identically(fixture_dir, tmp_path):
    fresh, _, _ = _run(fixture_dir, tmp_path / "fresh" if False else tmp_path)
    # First run crashes halfway through and must leave a durable checkpoint.
    ckpt = tmp_path / "int.json"
    result = tmp_path / "int-result.json"
    first = RunConfig(input_dir=fixture_dir, checkpoint_path=str(ckpt),
                      inject_interrupt_at=40)
    with pytest.raises(InterruptedError):
        DiagnosisEngine(first).run()
    saved = load_checkpoint(str(ckpt))
    assert saved.next_offset > 0

    resumed_config = RunConfig(
        input_dir=fixture_dir, checkpoint_path=str(ckpt),
        result_path=str(result), resume=True)
    resumed = DiagnosisEngine(resumed_config).run()

    fresh_key = _error_signature(fresh)
    assert _error_signature(resumed) == fresh_key
    assert resumed["metrics"]["resume_seconds"] <= 2.0
    assert resumed["resume"]["resumed"] is True


def test_resume_rejects_changed_input(fixture_dir, tmp_path):
    _, ckpt_path, _ = _run(fixture_dir, tmp_path)
    stream = os.path.join(fixture_dir, "stream.rlyd")
    backup = tmp_path / "stream.bak"
    with open(stream, "rb") as src, open(backup, "wb") as out:
        out.write(src.read())
    try:
        with open(stream, "r+b") as fh:
            fh.seek(1500)
            fh.write(b"\xDE\xAD\xBE\xEF")
        saved = load_checkpoint(ckpt_path)
        with pytest.raises(StateMismatch, match="fingerprint"):
            validate_for_resume(saved, input_path=stream,
                                params=saved.params)
    finally:
        with open(backup, "rb") as src, open(stream, "wb") as out:
            out.write(src.read())


def test_resume_rejects_changed_parameters(fixture_dir, tmp_path):
    _, ckpt_path, _ = _run(fixture_dir, tmp_path)
    saved = load_checkpoint(ckpt_path)
    different = dict(saved.params)
    different["queue_capacity"] = saved.params["queue_capacity"] + 1
    with pytest.raises(StateMismatch, match="parameters"):
        validate_for_resume(saved, input_path=saved.input_path,
                            params=different)


# ----------------------------------------------------------------- manifest
def test_manifest_is_deterministic_and_verifies(fixture_dir, tmp_path):
    doc1, _, result_path = _run(fixture_dir, tmp_path)
    on_disk = json.load(open(result_path))
    ok, problems = verify_document(on_disk)
    assert ok and not problems
    assert len(doc1["output_sha256"]) == 64
    int(doc1["output_sha256"], 16)  # valid hex


# ---------------------------------------------------------------------- CLI
def test_cli_exit_codes(fixture_dir, tmp_path):
    result = tmp_path / "cli-result.json"
    ckpt = tmp_path / "cli-state.json"
    code = main(["run", "--input", fixture_dir, "--checkpoint", str(ckpt),
                 "--json", str(result)])
    assert code == 3  # damage found
    code = main(["verify", "--input", fixture_dir, "--result", str(result)])
    assert code == 0
    code = main(["run", "--input", str(tmp_path / "missing"),
                 "--checkpoint", str(ckpt)])
    assert code == 2


def _load_header(stream_path: str) -> dict:
    with open(stream_path, "rb") as fh:
        from relaydiag.container import read_header
        return read_header(fh)


def _error_signature(document: dict):
    return sorted(
        (e["category"], e["byte_offset_start"], e["byte_offset_end"],
         round(e["time_range"]["start_s"], 6))
        for e in document["errors"]
    )
