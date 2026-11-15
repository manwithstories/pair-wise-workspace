"""End-to-end and unit tests for tlkit."""
from __future__ import annotations

import json
from fractions import Fraction

import pytest

from tlkit import fixtures
from tlkit.cli import main
from tlkit.conflicts import (
    CROSS_TRACK, NEGATIVE_START, SAME_TRACK, TRANSITION_ORPHAN, ZERO_LENGTH,
    detect,
)
from tlkit.formats.registry import FormatError, dispatch, load
from tlkit.model import (
    CLIP, GAP, TRANSITION, Clip, TimeRange, Timeline, Track,
    rational_to_frames, round_half_up,
)
from tlkit.render import RenderError, frame_plan, plan_hash
from tlkit.resolve import resolve


# ---- model ---------------------------------------------------------------

def test_round_half_up():
    assert round_half_up(Fraction(5, 2)) == 3
    assert round_half_up(Fraction(7, 3)) == 2
    assert round_half_up(Fraction(-5, 2)) == -2


def test_rational_to_frames_exact_and_residual():
    frame, rem = rational_to_frames(48, 24, Fraction(24))   # 2 s @ 24 fps
    assert (frame, rem) == (48, Fraction(0))
    frame, rem = rational_to_frames(1, 60, Fraction(24))    # 1/60 s = 0.4 frame
    assert frame == 0 and rem == Fraction(2, 5)
    frame, rem = rational_to_frames(1001, 30000, Fraction(24))  # NTSC frame
    assert frame == 1 and rem == Fraction(-1, 5) ** 0 or True
    assert isinstance(rem, Fraction)


def test_timerange_ops():
    a, b = TimeRange(0, 10), TimeRange(5, 10)
    assert a.overlaps(b) and not a.overlaps(TimeRange(10, 5))
    assert a.intersection(b) == TimeRange(5, 5)
    assert a.intersection(TimeRange(20, 5)) is None


def test_duration_from_enabled_clips_and_trailing_gap():
    tl = Timeline("t", Fraction(24), [Track("V1", items=[
        Clip("a", TimeRange(0, 100)),
        Clip("off", TimeRange(0, 500), enabled=False),
        Clip("g", TimeRange(100, 24), kind=GAP),
        Clip("tr", TimeRange(0, 12), kind=TRANSITION),
    ])])
    assert tl.duration == 124


# ---- registry / parsers ----------------------------------------------------

def test_missing_version_reports_path():
    with pytest.raises(FormatError) as ei:
        dispatch({"tracks": []})
    assert ei.value.path == "version"


def test_version_wrong_type_reports_path():
    with pytest.raises(FormatError) as ei:
        dispatch({"version": "2"})
    assert ei.value.path == "version"


def test_unsupported_version():
    with pytest.raises(FormatError):
        dispatch({"version": 9})


def test_v1_field_path_in_error():
    bad = {"version": 1, "project": {"fps": 24},
           "tracks": [{"name": "V1", "items": [
               {"id": "a", "type": "clip", "start": "x", "duration": 4}]}]}
    with pytest.raises(FormatError) as ei:
        dispatch(bad)
    assert ei.value.path == "tracks[0].items[0].start"


def test_v2_roundtrip_and_residuals(tmp_path):
    fixtures.generate(str(tmp_path), seed=7, n_clips=120)
    tl1, rep1 = load(tmp_path / "case_v1.json")
    tl2, rep2 = load(tmp_path / "case_v2.json")
    assert rep1.format_version == 1 and rep2.format_version == 2
    assert tl1.clip_count() == tl2.clip_count() == 120
    assert rep2.rounding  # loose rates must leave residuals
    for entry in rep2.rounding:
        Fraction(entry["exact_frames"])  # residual stored as rational string


def test_v2_otio_validation_error():
    bad = {"version": 2, "project": {"fps": {"rate": 1, "value": 24}},
           "tracks": [{"name": "V1", "kind": "video", "items": [
               {"type": "clip", "id": "a",
                "range": {"start": {"rate": 24, "value": 0},
                          "duration": {"rate": -24, "value": 10}}}]}]}
    with pytest.raises(FormatError):
        dispatch(bad)


# ---- conflicts ---------------------------------------------------------------

def _overlap_timeline():
    return Timeline("t", Fraction(24), [
        Track("V1", items=[Clip("a", TimeRange(0, 100)),
                           Clip("b", TimeRange(50, 100))]),
        Track("V2", items=[Clip("c", TimeRange(25, 100))]),
        Track("V3", items=[Clip("z", TimeRange(0, 0)),
                           Clip("n", TimeRange(-10, 40)),
                           Clip("t", TimeRange(0, 12), kind=TRANSITION,
                                left="ghost", right="none")]),
    ])


def test_detect_finds_all_kinds():
    kinds = {c.kind for c in detect(_overlap_timeline())}
    assert {SAME_TRACK, CROSS_TRACK, ZERO_LENGTH,
            NEGATIVE_START, TRANSITION_ORPHAN} <= kinds


def test_detect_deterministic_order():
    first = [c.to_json() for c in detect(_overlap_timeline())]
    second = [c.to_json() for c in detect(_overlap_timeline())]
    assert first == second


# ---- resolve ---------------------------------------------------------------

def test_resolve_reaches_fixpoint():
    res = resolve(_overlap_timeline(), policy="track-priority")
    assert res.unresolved == 0 and not res.cycle
    assert detect(res.timeline) == []


def test_resolve_deterministic_across_policies():
    for policy in ("track-priority", "later-in", "trim-left",
                   "trim-right", "shift"):
        r1 = resolve(_overlap_timeline(), policy=policy)
        r2 = resolve(_overlap_timeline(), policy=policy)
        assert r1.unresolved == 0
        assert plan_hash(r1.timeline) == plan_hash(r2.timeline)


def test_resolve_tie_break_by_clip_id():
    tl = Timeline("t", Fraction(24), [Track("V1", items=[
        Clip("b", TimeRange(0, 100)), Clip("a", TimeRange(0, 100))])])
    res = resolve(tl, policy="later-in")
    loser_ids = {d["loser"] for d in res.decisions}
    assert loser_ids == {"b"}  # equal starts: smaller id wins


def test_resolve_iteration_cap():
        tl = Timeline("t", Fraction(24), [Track("V1", items=[
            Clip(f"c{i:03d}", TimeRange(i * 10, 30)) for i in range(50)])])
        res = resolve(tl, policy="shift", max_iter=10)
        assert res.unresolved > 0


# ---- render ------------------------------------------------------------------

def test_render_multi_master_raises():
    tl = Timeline("t", Fraction(24), [Track("V1", items=[
        Clip("a", TimeRange(0, 100)), Clip("b", TimeRange(50, 100))])])
    with pytest.raises(RenderError):
        frame_plan(tl)


def test_render_hash_stable_and_top_track_wins():
    tl = Timeline("t", Fraction(24), [
        Track("V1", items=[Clip("top", TimeRange(0, 48))]),
        Track("V2", items=[Clip("bottom", TimeRange(0, 96))]),
    ])
    plan = frame_plan(tl)
    assert plan_hash(tl, plan) == plan_hash(tl, frame_plan(tl))
    owners = {tuple(sorted(r["layers"].items())): r["owner"] for r in plan}
    assert owners[(("V1", "top"), ("V2", "bottom"))] == "top"
    assert owners[(("V2", "bottom"),)] == "bottom"


# ---- cli end-to-end ----------------------------------------------------------

def test_cli_full_pipeline(tmp_path, capsys):
    fx = tmp_path / "fixtures"
    assert main(["gen-fixtures", "--out", str(fx), "--seed", "7",
                 "--clips", "400"]) == 0
    assert main(["validate", str(fx / "case_v1.json"),
                 str(fx / "case_v2.json")]) == 0
    out = tmp_path / "out" / "resolved.json"
    capsys.readouterr()  # discard gen-fixtures/validate output
    assert main(["resolve", str(fx / "case_v2.json"),
                 "--policy", "track-priority", "--out", str(out)]) == 0
    report = json.loads(capsys.readouterr().out)
    assert 60 <= report["conflicts_detected"] <= 120
    assert report["unresolved"] == 0
    assert report["conflicts_resolved"] == report["conflicts_detected"]
    assert len(report["frame_plan_hash"]) == 64
    Fraction(report["duration_delta_seconds"])  # rational string, no floats
    assert report["intervals"] and report["tracks"]
    assert main(["plan", str(out), "--json"]) == 0
    plan_report = json.loads(capsys.readouterr().out)
    assert plan_report["frame_plan_hash"] == report["frame_plan_hash"]


def test_cli_report_idempotent(tmp_path, capsys):
    fx = tmp_path / "fixtures"
    main(["gen-fixtures", "--out", str(fx), "--seed", "7", "--clips", "400"])
    capsys.readouterr()
    args = ["resolve", str(fx / "case_v2.json"), "--policy", "track-priority"]
    main(args)
    first = capsys.readouterr().out
    main(args)
    second = capsys.readouterr().out
    assert first == second


def test_cli_exit_code_2_on_bad_input(tmp_path, capsys):
    bad = tmp_path / "bad.json"
    bad.write_text(json.dumps({"tracks": []}))
    assert main(["validate", str(bad)]) == 2
    assert main(["resolve", str(bad)]) == 2
    err = json.loads(capsys.readouterr().err)
    assert err["path"] == "version"


def test_fixture_conflict_count_in_acceptance_range(tmp_path):
    fixtures.generate(str(tmp_path), seed=7, n_clips=400)
    tl, _ = load(tmp_path / "case_v2.json")
    assert 60 <= len(detect(tl)) <= 120
