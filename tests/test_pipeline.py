"""End-to-end tests: success, loss, wrap, truncation and cycle scenarios.

Each test builds its own fixtures from code — no external media is referenced.
"""

from __future__ import annotations

import json
from fractions import Fraction
from pathlib import Path

import pytest

from mediasync.cli import main
from mediasync.clock import TICKS_PER_SECOND, unwrap_timestamps
from mediasync.errors import PlanError
from mediasync.fixtures import (
    FixtureConfig,
    build_video_fixture,
    corrupt_index,
    make_timestamps,
    truncate_stream,
)
from mediasync.ingest import Limits, load_video_track
from mediasync.jitter import TransportPacket, dejitter, JitterConfig
from mediasync.pipeline import PipelineConfig, run_pipeline
from mediasync.report import load_manifest, verify_manifest
from mediasync.timeline import Interval, build_timeline


def scenario_manifest(tmp_path: Path, **overrides) -> dict:
    config = PipelineConfig(seed=17, frames=60, target_bps=2_000_000, **overrides)
    result = run_pipeline(config)
    path = result.manifest.write(tmp_path / "manifest.json")
    return load_manifest(path)


def test_success_scenario_exits_zero(tmp_path: Path) -> None:
    out = tmp_path / "success.json"
    code = main(
        [
            "demo",
            "--seed", "17",
            "--loss", "0.0",
            "--jitter-ms", "10",
            "--target-bps", "2000000",
            "--frames", "60",
            "--out", str(out),
        ]
    )
    assert code == 0
    payload = load_manifest(out)
    assert payload["status"] == "PASS"
    assert payload["transport"]["loss"] == 0
    assert payload["transport"]["unrecoverable"] == 0
    assert payload["video"]["frames"] > 0
    assert abs(payload["bitrate"]["deviation"]) <= 0.05


def test_loss_scenario_recovers_within_budget(tmp_path: Path) -> None:
    payload = scenario_manifest(tmp_path, loss=0.1, jitter_ms=45)
    transport = payload["transport"]
    assert transport["loss"] > 0
    assert transport["recovered"] + transport["unrecoverable"] == transport["loss"]
    assert transport["unrecoverable"] == 0
    assert payload["status"] == "PASS"


def test_unrecoverable_loss_exits_three(tmp_path: Path) -> None:
    payload = scenario_manifest(tmp_path, loss=0.1, retransmit_attempts=0)
    assert payload["transport"]["unrecoverable"] > 0
    assert payload["gaps"], "unrecoverable holes must be written to the manifest"
    existing = tmp_path / "manifest.json"
    code = main(["verify", str(existing)])
    assert code == 3


def test_wrap_scenario_generates_reversible_timestamps(tmp_path: Path) -> None:
    wrap = 1 << 33
    entries = make_timestamps(12, Fraction(30), wrap_at=4)
    wire = [entry["pts_ticks"] for entry in entries]
    assert any(value > wrap // 2 for value in wire)
    unwrapped = unwrap_timestamps(wire)
    expected = [entry["unwrapped_ticks"] for entry in entries]
    offsets = {actual - target for actual, target in zip(unwrapped, expected)}
    assert len(offsets) == 1, "the wrap must expand to a single constant epoch"


def test_wrap_fixture_keeps_display_monotonic(tmp_path: Path) -> None:
    config = FixtureConfig(seed=5, frames=24, fps=Fraction(30), codec="h264", target_bps=400_000)
    fixture = build_video_fixture(config=config, root=tmp_path, track_id="wrap", wrap_at=10)
    video = load_video_track(fixture.index_path, limits=Limits())
    from mediasync.clock import build_display_track

    track = build_display_track(
        [packet.pts_ticks for packet in video.packets], nominal_step_ticks=3000
    )
    assert not track.restarts
    display = track.display_ticks
    assert all(b > a for a, b in zip(display, display[1:]))


def test_truncated_index_scenario_uses_boundary_repair(tmp_path: Path) -> None:
    config = FixtureConfig(seed=7, frames=40, fps=Fraction(30), codec="h264", target_bps=500_000)
    fixture = build_video_fixture(tmp_path, config, track_id="trunc")
    keep = int(fixture.size_bytes * 0.8)
    truncated = truncate_stream(fixture.path, tmp_path / "trunc-cut.h264", keep)
    index_path = tmp_path / "trunc-cut.index.json"
    corrupt_index(
        fixture.index_path,
        index_path,
        lambda payload: payload.update({"stream": truncated.name}),
    )
    video = load_video_track(index_path, limits=Limits(), allow_truncated=True)
    assert video.truncated_indices

    from mediasync.pipeline import build_intervals
    from mediasync.recover import RecoveryConfig, recover_interval, recover_timeline
    from mediasync.timeline import Timeline

    usable_packets = [
        TransportPacket(
            seq=record.seq,
            arrival_ms=record.index * 33,
            data=b"",
            keyframe=record.keyframe,
            pts_ticks=record.pts_ticks,
            duration_ticks=record.duration_ticks,
        )
        for record in video.packets
        if record.index not in video.truncated_indices
    ]
    intervals = build_intervals(video, 1.0)
    timeline = Timeline()
    recoveries = {}
    cfg = RecoveryConfig(seed=1)
    for item in intervals:
        timeline.add(item)
        recoveries[item.interval_id] = recover_interval(
            item,
            ordered=usable_packets,
            missing=[],
            truncated_seqs=video.truncated_indices,
            config=cfg,
        )
    report = recover_timeline(timeline, recoveries)
    assert report.repairs, "a truncated index must produce an explicit repair record"
    assert all(report.repairs), "every repair record must describe real damage"
    assert report.gaps, "the damaged tail must be written as a gap"
    assert any(
        gap.reason in ("truncated-index-boundary", "unrecoverable-tail")
        for gap in report.gaps
    )
    assert not any(segment.complete for segment in report.segments[-1:]), (
        "the tail segment must never be reported complete"
    )


def test_truncated_stream_without_allowance_is_rejected(tmp_path: Path) -> None:
    config = FixtureConfig(seed=7, frames=20, fps=Fraction(30), codec="h264", target_bps=400_000)
    fixture = build_video_fixture(tmp_path, config, track_id="cut")
    truncated = truncate_stream(fixture.path, tmp_path / "cut.h264", fixture.size_bytes // 2)
    index_path = tmp_path / "cut.index.json"
    corrupt_index(
        fixture.index_path,
        index_path,
        lambda payload: payload.update({"stream": truncated.name}),
    )
    with pytest.raises(Exception):
        load_video_track(index_path, limits=Limits())


def test_cycle_scenario_is_rejected() -> None:
    first = Interval("a", "cam", 0, 1000, depends_on=("b",))
    second = Interval("b", "cam", 1000, 2000, depends_on=("a",))
    with pytest.raises(PlanError) as excinfo:
        build_timeline([[first, second]])
    assert "cycle" in str(excinfo.value)


def test_restart_scenario_keeps_raw_and_display_separate(tmp_path: Path) -> None:
    config = FixtureConfig(seed=3, frames=24, fps=Fraction(30), codec="h264", target_bps=400_000)
    fixture = build_video_fixture(tmp_path, config, track_id="restart", restart_at=12)
    video = load_video_track(fixture.index_path, limits=Limits())
    raw = [packet.pts_ticks for packet in video.packets]
    assert raw[12:14] == sorted(raw[12:14]), "restarted timestamps come from a fresh clock"
    assert raw[12] < raw[11], "the fixture must actually model a restart"
    from mediasync.clock import build_display_track

    track = build_display_track(raw, nominal_step_ticks=3000)
    assert track.restarts
    assert all(
        b > a for a, b in zip(track.display_ticks, track.display_ticks[1:])
    )


def test_verify_rejects_tampered_manifest(tmp_path: Path) -> None:
    payload = scenario_manifest(tmp_path, loss=0.0)
    path = tmp_path / "manifest.json"
    payload["transport"]["unrecoverable"] = 4
    payload["status"] = "PASS"
    path.write_text(json.dumps(payload))
    assert main(["verify", str(path)]) in (3, 4)


def test_demo_is_byte_deterministic(tmp_path: Path) -> None:
    first = tmp_path / "first.json"
    second = tmp_path / "second.json"
    args = [
        "demo", "--seed", "42", "--frames", "40", "--loss", "0.05",
        "--target-bps", "1500000", "--quiet",
    ]
    assert main(args + ["--out", str(first)]) == 0
    assert main(args + ["--out", str(second)]) == 0
    assert first.read_bytes() == second.read_bytes()


def test_cli_rejects_invalid_parameters(tmp_path: Path) -> None:
    out = tmp_path / "bad.json"
    assert main(["demo", "--loss", "1.5", "--out", str(out)]) == 2
    assert main(["demo", "--target-bps", "0", "--out", str(out)]) == 2
    assert main(["demo", "--jitter-ms", "-1", "--out", str(out)]) == 2
    assert main(["demo", "--fps", "abc", "--out", str(out)]) == 2
    assert not out.exists()


def test_verify_missing_file_exits_two(tmp_path: Path) -> None:
    assert main(["verify", str(tmp_path / "absent.json")]) == 2


def test_all_three_codecs_pass_end_to_end(tmp_path: Path) -> None:
    for codec in ("h264", "hevc", "av1"):
        result = run_pipeline(
            PipelineConfig(
                seed=11, codec=codec, frames=30, target_bps=1_000_000, loss=0.05
            )
        )
        payload = result.manifest.to_dict()
        assert payload["status"] == "PASS", codec
        assert verify_manifest(payload) == []
