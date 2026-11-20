"""Graph ordering, checkpoint atomicity, report shape and CLI exit codes."""

from __future__ import annotations

import json

import pytest

from mediacore.checkpoint import (
    Checkpoint,
    NodeRecord,
    digest_payload,
    read_checkpoint,
    write_checkpoint,
)
from mediacore.errors import CheckpointError
from mediacore.graph import build_plan
from mediacore.report import Report
from mediacore.timeline import ClipSpec, Timeline, TrackSpec, TransitionSpec


def _timeline_two_clips(overlap=True):
    timeline = Timeline()
    timeline.add_track(TrackSpec(id="a", path="x", kind="audio", timebase="1/48000"))
    timeline.add_clip(ClipSpec(id="c1", track="a", start_us=0, end_us=1_000_000))
    timeline.add_clip(
        ClipSpec(id="c2", track="a", start_us=900_000, end_us=2_000_000)
    )
    if overlap:
        timeline.add_transition(
            TransitionSpec(id="x", left="c1", right="c2", duration_us=100_000)
        )
    timeline.validate()
    return timeline


def test_plan_orders_clips_before_their_transition():
    plan = build_plan(_timeline_two_clips())
    assert plan.order.index("clip:c1") < plan.order.index("transition:x")
    assert plan.order.index("clip:c2") < plan.order.index("transition:x")


def test_plan_tracks_are_independent():
    from mediacore.graph import build_plan as bp

    timeline = _timeline_two_clips(overlap=True)
    timeline.add_track(TrackSpec(id="b", path="y", kind="audio", timebase="1/44100"))
    timeline.add_clip(ClipSpec(id="c3", track="b", start_us=0, end_us=1_000_000))
    plan = bp(timeline)
    plan.fail("clip:c1", "boom")
    # The other track's clip is untouched.
    assert plan.state("clip:c3") == "pending"
    assert "clip:c3" in plan.ready_nodes()


# ---------------------------------------------------------------------------
# checkpoint
# ---------------------------------------------------------------------------


def _record(node_id="clip:c1", **overrides):
    base = dict(
        node_id=node_id,
        kind="clip",
        input_digest="in",
        output_path="/tmp/x.wav",
        output_sha256="out",
        output_samples=10,
    )
    base.update(overrides)
    return NodeRecord(**base)


def test_checkpoint_roundtrip(tmp_path):
    path = tmp_path / "cp.json"
    checkpoint = Checkpoint(path=path, timeline_digest="t", settings_digest="s")
    checkpoint.record(_record())
    write_checkpoint(checkpoint)
    loaded = read_checkpoint(path)
    assert loaded is not None
    assert loaded.records["clip:c1"].output_samples == 10
    assert loaded.payload_digest() == checkpoint.payload_digest()


def test_checkpoint_detects_tampering(tmp_path):
    path = tmp_path / "cp.json"
    checkpoint = Checkpoint(path=path, timeline_digest="t", settings_digest="s")
    checkpoint.record(_record())
    write_checkpoint(checkpoint)
    data = json.loads(path.read_text())
    data["records"]["clip:c1"]["output_samples"] = 999
    path.write_text(json.dumps(data))
    with pytest.raises(CheckpointError) as info:
        read_checkpoint(path)
    assert info.value.recoverable


def test_checkpoint_schema_mismatch_is_recoverable(tmp_path):
    path = tmp_path / "cp.json"
    path.write_text(json.dumps({"schema": 999, "records": {}}))
    with pytest.raises(CheckpointError) as info:
        read_checkpoint(path)
    assert info.value.recoverable


def test_digest_is_order_independent():
    assert digest_payload({"a": 1, "b": 2}) == digest_payload({"b": 2, "a": 1})
    assert digest_payload({"a": 1}) != digest_payload({"a": 2})


# ---------------------------------------------------------------------------
# report
# ---------------------------------------------------------------------------


def test_report_always_contains_acceptance_fields():
    report = Report()
    report.recovery = {
        "recovery_seconds": 0.5,
        "reused_nodes": 2,
        "output_sha256": "a" * 64,
    }
    data = report.to_dict()
    for key in (
        "damage_offset_frames",
        "recovery_seconds",
        "reused_nodes",
        "output_sha256",
        "timeline_drift_us",
    ):
        assert key in data
    assert data["reused_nodes"] == 2


def test_report_coerces_non_finite_floats():
    report = Report()
    report.audio = {"x": {"integrated_lufs": float("-inf")}}
    encoded = report.to_json()
    assert "Infinity" not in encoded
    assert "NaN" not in encoded


def test_report_computes_drift_from_scans():
    report = Report()
    report.scans = {
        "a": {"drift": {"max_abs_us": 1}},
        "b": {"drift": {"max_abs_us": 3}},
    }
    assert report.timeline_drift_us() == 3


# ---------------------------------------------------------------------------
# CLI exit codes
# ---------------------------------------------------------------------------


def test_cli_demo_and_resume_exit_zero(tmp_path, capsys):
    from mediacore.cli import main

    workdir = tmp_path / "demo"
    code = main(
        [
            "demo",
            "--generate",
            "--workdir",
            str(workdir),
            "--interrupt-after",
            "2",
        ]
    )
    assert code == 0
    code = main(["resume", "--workdir", str(workdir), "--verify"])
    assert code == 0
    report = json.loads((workdir / "report.json").read_text())
    assert report["reused_nodes"] >= 2
    assert len(report["output_sha256"]) == 64
    assert report["timeline_drift_us"] <= 21
    assert report["damage_offset_frames"] <= 1


def test_cli_cycle_exits_graph_code(tmp_path, fixtures):
    from mediacore.cli import main
    from mediacore.errors import EXIT_GRAPH

    workdir = tmp_path / "demo"
    workdir.mkdir()
    code = main(
        [
            "demo",
            "--workdir",
            str(workdir),
            "--timeline",
            str(fixtures["timeline_cycle"]),
        ]
    )
    assert code == EXIT_GRAPH


def test_cli_bad_parameter_exits_usage(tmp_path):
    from mediacore.cli import main
    from mediacore.errors import EXIT_USAGE

    code = main(["resume", "--workdir", str(tmp_path / "does-not-exist")])
    assert code == EXIT_USAGE
