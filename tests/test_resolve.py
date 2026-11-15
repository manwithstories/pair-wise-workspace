"""单元测试与回归比对：模型、扫描、消解、格式、渲染、OTIO、CLI。"""
from __future__ import annotations

import json
import sys
import time
from fractions import Fraction
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from src.cli import main as cli_main
from src.formats import otio_bridge, v1, v2
from src.render.plan import render_plan
from src.timeline import resolve as resolve_mod
from src.timeline.model import (
    Clip, ModelError, TimeRange, Timeline, Track, parse_fps,
)
from src.timeline.resolve import resolve_timeline
from src.timeline.scan import audio_video_deviation, scan_timeline
from tests.gen_fixtures import build_timeline

FIXTURES = Path(__file__).resolve().parent.parent / "fixtures"


def make_clip(cid, start, dur, speed=Fraction(1)):
    return Clip(cid, TimeRange(Fraction(start), Fraction(dur)),
                TimeRange(Fraction(0), Fraction(dur) * speed), speed)


# ---------- 模型与帧率 ----------

def test_supported_fps_exact():
    assert parse_fps("23.976") == Fraction(24000, 1001)
    assert parse_fps("25") == Fraction(25)
    assert parse_fps("29.97") == Fraction(30000, 1001)
    assert parse_fps("50") == Fraction(50)
    assert parse_fps("60") == Fraction(60)


def test_fps_conversion_within_one_frame():
    fps = parse_fps("29.97")
    seconds = Fraction(3600)
    frames = seconds * fps
    assert abs(frames / fps - seconds) <= 1 / fps


def test_bad_fps_raises():
    with pytest.raises(ModelError) as exc:
        parse_fps("17.5")
    assert exc.value.code == "FPS_CONFLICT"


def test_half_open_overlap():
    a = TimeRange(Fraction(0), Fraction(50))
    b = TimeRange(Fraction(50), Fraction(50))
    assert not a.overlaps(b)  # 首尾相接不算冲突
    assert a.overlaps(TimeRange(Fraction(49), Fraction(10)))


# ---------- 扫描 ----------

def test_scan_zero_length_and_adjacent():
    tl = Timeline(fps=Fraction(25))
    tr = Track(kind="video", index=0)
    tr.clips = [make_clip("a", 0, 50), make_clip("b", 50, 50),
                make_clip("z", 100, 0)]
    tl.tracks.append(tr)
    report = scan_timeline(tl)
    assert report.conflict_count == 0
    assert report.zero_length_count == 1
    assert len(report.adjacent_pairs) == 1


def test_scan_same_track_conflict():
    tl = Timeline(fps=Fraction(25))
    tr = Track(kind="video", index=0)
    tr.clips = [make_clip("a", 0, 60), make_clip("b", 40, 60)]
    tl.tracks.append(tr)
    report = scan_timeline(tl)
    assert report.conflict_count == 1
    assert report.conflicts[0].overlap_frames == 20


# ---------- 消解 ----------

def _two_track(winner_prio, loser_prio):
    tl = Timeline(fps=Fraction(25))
    hi = Track(kind="video", index=0, priority=winner_prio)
    lo = Track(kind="video", index=1, priority=loser_prio)
    tl.tracks.extend([hi, lo])
    return tl, hi, lo


def test_resolve_drop_fully_covered():
    tl, hi, lo = _two_track(2, 1)
    hi.clips.append(make_clip("win", 0, 100))
    lo.clips.append(make_clip("lose", 20, 30))
    result = resolve_timeline(tl)
    assert result.conflicts_resolved == 1
    assert lo.clips == []
    assert result.resolutions[0].action == "drop"


def test_resolve_split_middle():
    tl, hi, lo = _two_track(2, 1)
    hi.clips.append(make_clip("w1", 40, 20))
    lo.clips.append(make_clip("l1", 0, 100))  # 中段被压 -> 劈开
    result = resolve_timeline(tl)
    ranges = sorted((c.range.start, c.range.end) for c in lo.clips)
    assert ranges == [(0, 40), (60, 100)]
    assert result.resolutions[0].action == "split"


def test_resolve_trim_head():
    tl, hi, lo = _two_track(2, 1)
    hi.clips.append(make_clip("w", 0, 40))
    lo.clips.append(make_clip("l", 20, 80))
    resolve_timeline(tl)
    assert [(c.range.start, c.range.end) for c in lo.clips] == [(40, 100)]
    clip = lo.clips[0]
    assert clip.source_range.start == 20  # 源区间同步平移


def test_resolve_trim_tail():
    tl, hi, lo = _two_track(2, 1)
    hi.clips.append(make_clip("w", 60, 40))
    lo.clips.append(make_clip("l", 0, 80))
    resolve_timeline(tl)
    assert [(c.range.start, c.range.end) for c in lo.clips] == [(0, 60)]


def test_resolve_same_priority_by_track_index():
    tl, hi, lo = _two_track(5, 5)  # 同级按轨道索引，index 小者胜
    hi.clips.append(make_clip("a", 0, 50))
    lo.clips.append(make_clip("b", 0, 50))
    resolve_timeline(tl)
    assert len(hi.clips) == 1 and lo.clips == []


def test_resolve_audio_not_touched():
    tl = Timeline(fps=Fraction(25))
    au = Track(kind="audio", index=0)
    au.clips = [make_clip("a", 0, 60), make_clip("b", 30, 60)]
    tl.tracks.append(au)
    result = resolve_timeline(tl)
    assert result.conflicts_detected == 1
    assert result.conflicts_resolved == 0  # 音频只检测不消解
    assert len(au.clips) == 2


def test_non_convergence_raises(monkeypatch):
    monkeypatch.setattr(resolve_mod, "MAX_ROUNDS", 1)
    tl, hi, lo = _two_track(2, 1)
    hi.clips.append(make_clip("w", 0, 50))
    lo.clips.append(make_clip("l", 0, 50))
    with pytest.raises(ModelError) as exc:
        resolve_timeline(tl)
    assert exc.value.code == "NON_CONVERGENCE"


def test_audio_deviation():
    tl = Timeline(fps=Fraction(25))
    v = Track(kind="video", index=0)
    v.clips.append(make_clip("v", 0, 100))
    a = Track(kind="audio", index=1)
    a.clips.append(make_clip("a", 0, 95))
    tl.tracks.extend([v, a])
    assert audio_video_deviation(tl) == 5


# ---------- 格式 ----------

def test_v1_to_v2_normalize_fills_source():
    data = {"version": 1, "fps": "25", "extra": 1,
            "tracks": [{"kind": "video", "clips": [
                {"id": "c", "start_frame": 10, "duration_frames": 40,
                 "custom": "keep"}]}]}
    out = v2.normalize_v1(data)
    clip = out["tracks"][0]["clips"][0]
    assert clip["source_range"] == {"start_frame": 0, "duration_frames": 40}
    assert clip["custom"] == "keep"      # 未知字段透传
    assert out["extra"] == 1
    assert out["tracks"][0]["priority"] == 0


def test_missing_field_error():
    with pytest.raises(ModelError) as exc:
        v1.parse({"fps": "25", "tracks": [{"kind": "video",
                                           "clips": [{"id": "x"}]}]})
    assert exc.value.code == "MISSING_FIELD"


def test_negative_duration_error():
    with pytest.raises(ModelError) as exc:
        v1.parse({"fps": "25", "tracks": [{"kind": "video", "clips": [
            {"id": "x", "start_frame": 0, "duration_frames": -3}]}]})
    assert exc.value.code == "NEGATIVE_DURATION"


# ---------- OTIO ----------

def test_otio_round_trip_fixture():
    tl = v2.parse(json.loads((FIXTURES / "tl_v2.json").read_text()))
    ok, diffs = otio_bridge.round_trip_equal(tl)
    assert ok, diffs


# ---------- 渲染 ----------

def test_render_speed_mapping():
    tl = Timeline(fps=Fraction(25))
    tr = Track(kind="video", index=0, priority=1)
    tr.clips.append(Clip("fast", TimeRange(Fraction(0), Fraction(10)),
                         TimeRange(Fraction(100), Fraction(20)),
                         Fraction(2)))
    tr.clips.append(Clip("slow", TimeRange(Fraction(10), Fraction(10)),
                         TimeRange(Fraction(0), Fraction(5)),
                         Fraction(1, 2)))
    tl.tracks.append(tr)
    plan = render_plan(tl)
    assert plan["frame_count"] == 20
    assert plan["frames"][0]["source_frame"] == 100
    assert plan["frames"][5]["source_frame"] == 110   # 2x
    assert plan["frames"][14]["source_frame"] == 2    # 0.5x
    again = render_plan(tl)
    assert again["plan_sha256"] == plan["plan_sha256"]


# ---------- 回归（夹具） ----------

def test_fixture_regression():
    tl = v2.parse(json.loads((FIXTURES / "tl_v2.json").read_text()))
    before = tl.duration
    result = resolve_timeline(tl)
    assert result.conflicts_detected == 40
    assert result.conflicts_resolved == 40
    assert result.zero_length_clips == 4
    assert tl.clip_count() == 140
    assert tl.duration - before == 0
    assert scan_timeline(tl).conflict_count == 0


def test_render_plan_golden_hash():
    tl = v2.parse(json.loads((FIXTURES / "tl_v2.json").read_text()))
    resolve_timeline(tl)
    plan = render_plan(tl)
    assert plan["frame_count"] == 2995
    assert plan["plan_sha256"] == \
        "a5a809db9c57b688a76f7e7a42c48353cd83a397b1510197f166dbdd31a15de5"


def test_performance_5000_clips():
    tl = Timeline(fps=Fraction(25))
    hi = Track(kind="video", index=0, priority=2)
    lo = Track(kind="video", index=1, priority=1)
    for i in range(2500):
        hi.clips.append(make_clip(f"h{i}", i * 10, 8))
        lo.clips.append(make_clip(f"l{i}", i * 10 + 1, 4))
    tl.tracks.extend([hi, lo])
    start = time.monotonic()
    result = resolve_timeline(tl)
    elapsed = time.monotonic() - start
    assert result.conflicts_resolved == 2500
    assert elapsed < 2.0, f"5000 条片段消解耗时 {elapsed:.2f}s"


# ---------- CLI ----------

def test_cli_resolve_and_render(tmp_path, capsys):
    out = tmp_path / "resolved.json"
    rc = cli_main(["resolve", "--input", str(FIXTURES / "tl_v2.json"),
                   "--format", "v2", "--policy", "priority",
                   "--out", str(out), "--json"])
    assert rc == 0
    payload = json.loads(capsys.readouterr().out)
    assert payload["conflicts_detected"] == 40
    assert payload["duration_delta_frames"] == 0
    rc = cli_main(["render", "--input", str(out), "--fps", "25",
                   "--out", str(tmp_path / "plan.json")])
    assert rc == 0


def test_cli_diff_round_trip(capsys):
    rc = cli_main(["diff", "--a", str(FIXTURES / "tl_v1.json"),
                   "--b", str(FIXTURES / "tl_v2.json"), "--format", "v1"])
    assert rc == 0
    assert "round_trip_equal: True" in capsys.readouterr().out


def test_cli_invalid_input_exit_2(capsys):
    rc = cli_main(["resolve", "--input",
                   str(FIXTURES / "tl_invalid_negative.json")])
    assert rc == 2
    err = json.loads(capsys.readouterr().err)
    assert err["error"]["code"] == "NEGATIVE_DURATION"


def test_cli_fps_conflict_exit_2(tmp_path, capsys):
    rc = cli_main(["render", "--input", str(FIXTURES / "tl_v2.json"),
                   "--fps", "60"])
    assert rc == 2
    assert json.loads(capsys.readouterr().err)["error"]["code"] == \
        "FPS_CONFLICT"


def test_cli_audio_deviation_exit_1(tmp_path, capsys):
    tl = Timeline(fps=Fraction(25))
    v = Track(kind="video", index=0, priority=1)
    v.clips.append(make_clip("v", 0, 100))
    a = Track(kind="audio", index=1)
    a.clips.append(make_clip("a", 0, 90))
    tl.tracks.extend([v, a])
    path = tmp_path / "bad_audio.json"
    path.write_text(json.dumps(v2.serialize(tl)))
    rc = cli_main(["resolve", "--input", str(path), "--json"])
    assert rc == 1
    assert "warning" in json.loads(capsys.readouterr().out)
