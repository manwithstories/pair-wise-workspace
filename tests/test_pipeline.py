from __future__ import annotations

import json
import os
import time
from dataclasses import FrozenInstanceError
from fractions import Fraction
from pathlib import Path

import pytest

import cli
from exchange_v1 import V1Error, read_v1, write_v1
from exchange_v2 import V2Error, read_v2, write_v2
from fixtures import build_primary_timeline, build_small_timeline
from ledger import aggregate, initialize, record_run
from model import Clip, ModelError, Timeline, make_track, parse_rational
from normalize import model_signature, normalize_v1, normalize_v2, timeline_from_report
from otio_bridge import export_otio
from overlap import (
    build_resolution_report, duration_metrics, resolve_timeline, scan_conflicts,
)
from renderer import hashes_match, render_timeline

RATE = Fraction(30000, 1001)


def _pair(tmp_path: Path) -> tuple[Path, Path]:
    timeline = build_small_timeline("roundtrip", 7)
    v1, v2 = tmp_path / "roundtrip.v1.json", tmp_path / "roundtrip.v2.json"
    write_v1(timeline, v1)
    write_v2(timeline, v2)
    return v1, v2


def test_model_is_exact_and_immutable() -> None:
    clip = Clip("a", "T", "s", Fraction(0), Fraction(10))
    with pytest.raises(FrozenInstanceError):
        clip.start = Fraction(1)  # type: ignore[misc]
    with pytest.raises(ModelError):
        Clip("b", "T", "s", 0.5, Fraction(1))
    with pytest.raises(ModelError):
        parse_rational({"num": 1, "den": 0})
    zero = Clip("z", "T", "s", Fraction(4), Fraction(0))
    assert not zero.is_content and zero.end == 4


def test_v1_v2_roundtrip_has_same_model(tmp_path: Path) -> None:
    v1, v2 = _pair(tmp_path)
    assert model_signature(normalize_v1(v1)) == model_signature(normalize_v2(v2))
    first = normalize_v2(v2)
    again = tmp_path / "again.v2.json"
    write_v2(first, again)
    assert model_signature(normalize_v2(again)) == model_signature(first)


def test_v1_mixed_rates_and_precision_loss(tmp_path: Path) -> None:
    path = tmp_path / "mixed.v1.json"
    payload = {
        "format": "offline-timeline-v1", "version": 1, "name": "mixed",
        "rate": "30000/1001", "master_frames": 10,
        "events": [
            {"seq": 0, "op": "in", "track": "A", "src": "x", "tc": "00:00:00:01", "rate": "25/1"},
            {"seq": 1, "op": "out", "track": "A", "src": "x", "tc": "00:00:00:02", "rate": "25/1"},
        ],
    }
    path.write_text(json.dumps(payload), encoding="utf-8")
    timeline = read_v1(path, master_rate=RATE)
    clip = timeline.content_clips()[0]
    assert clip.start == Fraction(1200, 1001)
    assert clip.duration == Fraction(1200, 1001)
    assert timeline.precision_losses


def test_v2_nested_gap_and_policy(tmp_path: Path) -> None:
    path = tmp_path / "nested.v2.json"
    payload = {
        "format": "offline-timeline-v2", "version": 2, "name": "nested",
        "rate": {"num": 30000, "den": 1001}, "master_frames": 20, "policy": "protect-locked",
        "tracks": [{"id": "V", "layer": 2, "priority": 9, "children": [
            {"id": "CAM", "clips": [
                {"id": "c", "src": "cam", "start": {"num": 0, "den": 1},
                 "duration": {"num": 10, "den": 1}},
                {"id": "g", "kind": "gap", "src": "gap", "start": {"num": 10, "den": 1},
                 "duration": {"num": 10, "den": 1}},
            ]}
        ]}],
    }
    path.write_text(json.dumps(payload), encoding="utf-8")
    timeline = read_v2(path)
    assert timeline.tracks[0].name == "V/CAM"
    assert timeline.tracks[0].layer == 2
    assert sum(clip.kind == "gap" for clip in timeline.all_clips()) == 1
    out = tmp_path / "nested.out.v2.json"
    write_v2(timeline, out)
    assert model_signature(read_v2(out)) == model_signature(timeline)


def test_reason_codes_for_bad_inputs(tmp_path: Path) -> None:
    zero = tmp_path / "zero.v2.json"
    zero.write_text(json.dumps({"version": 2, "rate": "30000/1001", "master_frames": 1, "tracks": [
        {"id": "T", "clips": [{"id": "c", "start": {"num": 0, "den": 0},
                               "duration": {"num": 1, "den": 1}}]}]}), encoding="utf-8")
    with pytest.raises(V2Error, match="denominator"):
        read_v2(zero)
    policy = tmp_path / "policy.v2.json"
    policy.write_text(json.dumps({"version": 2, "rate": "1/1", "master_frames": 1,
                                  "policy": "mystery", "tracks": []}), encoding="utf-8")
    with pytest.raises(V2Error) as exc:
        read_v2(policy)
    assert exc.value.code == "unknown_policy"
    unclosed = tmp_path / "unclosed.v1.json"
    unclosed.write_text(json.dumps({"version": 1, "rate": "1/1", "master_frames": 5, "events": [
        {"seq": 0, "op": "in", "track": "T", "src": "s", "frame": {"num": 0, "den": 1}}]}),
                        encoding="utf-8")
    with pytest.raises(V1Error) as exc:
        read_v1(unclosed)
    assert exc.value.code == "v1_unclosed_clip"


def test_primary_scan_and_resolution_targets() -> None:
    timeline = build_primary_timeline()
    scan = scan_conflicts(timeline)
    assert 30 <= scan.conflict_count <= 60
    assert set(scan.by_kind) == {"intra_track", "cross_layer", "locked_intrusion"}
    assert scan.comparisons < 100
    resolved, initial, actions = resolve_timeline(timeline, "protect-locked")
    assert initial.conflict_count == scan.conflict_count
    assert 250 <= len(resolved.content_clips()) <= 320
    metrics = duration_metrics(resolved)
    assert metrics["resolved_frames"] == 7200
    assert metrics["duration_delta_frames"] == 0
    assert metrics["total_duration_seconds"] == pytest.approx(240.24, abs=0.01)
    assert actions and all({"action", "before", "after"} <= set(action) for action in actions)
    assert scan_conflicts(resolved).conflict_count == 0


@pytest.mark.parametrize("policy", ["trim-left", "trim-right", "split", "upper-covers", "reject"])
def test_all_unlocked_policies_reach_fixed_point(policy: str) -> None:
    clips = [Clip("a", "A", "s", Fraction(0), Fraction(10), layer=1),
             Clip("b", "B", "s", Fraction(5), Fraction(10), layer=2)]
    timeline = Timeline("policy", RATE, 20, (
        make_track("A", [clips[0]], layer=1), make_track("B", [clips[1]], layer=2)))
    resolved, _, actions = resolve_timeline(timeline, policy)
    assert actions and scan_conflicts(resolved).conflict_count == 0


def test_protect_locked_and_locked_move_error() -> None:
    locked = Clip("locked", "L", "s", Fraction(0), Fraction(10), layer=1, priority=1, locked=True)
    upper = Clip("upper", "U", "s", Fraction(2), Fraction(3), layer=2, priority=99)
    timeline = Timeline("locked", RATE, 10, (
        make_track("L", [locked], layer=1), make_track("U", [upper], layer=2)))
    resolved, _, actions = resolve_timeline(timeline, "protect-locked")
    assert [clip.id for clip in resolved.content_clips()] == ["locked"]
    assert actions[0]["action"] == "protect-locked"
    with pytest.raises(Exception) as exc:
        resolve_timeline(timeline, "trim-left")
    assert getattr(exc.value, "code", None) == "locked_segment_move_required"


def test_render_hashes_converge_and_upper_shading() -> None:
    timeline = Timeline("shade", RATE, 12, (
        make_track("LOW", [Clip("low", "LOW", "low", Fraction(0), Fraction(12), layer=1)], layer=1),
        make_track("HIGH", [Clip("high", "HIGH", "high", Fraction(3), Fraction(3), layer=2)], layer=2),
    ))
    rendered = render_timeline(timeline)
    assert rendered["frames_hashed"] == 12
    assert rendered["source_coverage"]["high"]["frames"] == 3
    assert rendered["uncovered_frames"] == 0
    tracks = {run["track"] for run in rendered["frame_ownership_runs"]}
    assert tracks == {"LOW", "HIGH"}


def test_duration_compensation_and_over_limit() -> None:
    clips = [Clip("pre", "T", "s", Fraction(-2), Fraction(5)),
             Clip("tail", "T", "s", Fraction(7), Fraction(6))]
    timeline = Timeline("comp", RATE, 10, (make_track("T", clips, layer=1),))
    metrics = duration_metrics(timeline)
    assert metrics["negative_preroll_trimmed"] == {"num": 2, "den": 1}
    assert metrics["tail_trimmed"] == {"num": 3, "den": 1}
    assert metrics["duration_delta_frames"] == 1
    bad = Timeline("bad", RATE, 10, (make_track("T", [
        Clip("long", "T", "s", Fraction(0), Fraction(13))], layer=1),))
    assert duration_metrics(bad)["over_limit"] is True


def test_otio_roundtrip_and_degradation(tmp_path: Path) -> None:
    timeline = Timeline("otio", RATE, 24, (
        make_track("A", [Clip("a", "A", "a", Fraction(0), Fraction(10), layer=1)], layer=1),
        make_track("B", [Clip("b", "B", "b", Fraction(10), Fraction(10), layer=1)], layer=1),
    ))
    summary = export_otio(timeline, tmp_path / "x.otio", verify=True)
    assert summary["verified"] and summary["mismatches"] == []
    assert summary["degraded_track_count"] == 1
    assert summary["track_count"] == 2


def test_otio_gap_roundtrip(tmp_path: Path) -> None:
    track = make_track("G", [
        Clip("a", "G", "a", Fraction(0), Fraction(5)),
        Clip("g", "G", "gap", Fraction(5), Fraction(5), kind="gap"),
        Clip("b", "G", "b", Fraction(10), Fraction(5)),
    ], layer=1)
    timeline = Timeline("gaps", RATE, 15, (track,))
    summary = export_otio(timeline, tmp_path / "gap.otio", verify=True)
    assert summary["verified"] and summary["gap_count"] == 1


def test_ledger_idempotent(tmp_path: Path) -> None:
    db = tmp_path / "ledger.sqlite3"
    initialize(db)
    for _ in range(2):
        record_run(db, "hash", "p", "success", 0, "{}")
    record_run(db, "hash2", "q", "failed", 1, "{}", "bad")
    summary = aggregate(db)
    assert summary["total"] == 2
    assert summary["by_status"] == {"failed": 1, "success": 1}
    assert summary["quarantine_reasons"] == {"bad": 1}


def test_5000_clips_is_sweep_based_and_fast() -> None:
    clips = [Clip(f"c{index}", "T", "s", Fraction(index), Fraction(1)) for index in range(5000)]
    timeline = Timeline("perf", RATE, 5000, (make_track("T", clips, layer=1),))
    started = time.perf_counter()
    scan = scan_conflicts(timeline)
    resolved, _, _ = resolve_timeline(timeline)
    assert scan.comparisons == 0
    assert len(resolved.content_clips()) == 5000
    assert time.perf_counter() - started < 3.0


def test_path_escape_is_rejected(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    cwd = tmp_path / "cwd"
    outside = tmp_path / "outside.json"
    cwd.mkdir()
    outside.write_text("{}", encoding="utf-8")
    monkeypatch.chdir(cwd)
    with pytest.raises(cli.CliError) as exc:
        cli.safe_path("../outside.json", must_exist=True)
    assert exc.value.code == "path_escape"
    link = cwd / "link.json"
    link.symlink_to(outside)
    with pytest.raises(cli.CliError):
        cli.safe_path("link.json", must_exist=True)
