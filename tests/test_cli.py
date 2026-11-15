"""End-to-end CLI tests on small, test-local generated media."""

from __future__ import annotations

import csv
import json
from fractions import Fraction
from pathlib import Path

import av
import numpy as np
import pytest

from cinecheck.cli import main
from cinecheck.fixtures import (
    FIXTURE_HEIGHT,
    FIXTURE_TIME_BASE,
    FIXTURE_WIDTH,
)


def _encode(path: Path, frame_count: int, pts=None, bframes=2, seed=1):
    rng = np.random.default_rng(seed)
    options = {
        "bf": str(bframes),
        "gop": "12",
        "keyint_min": "12",
        "sc_threshold": "0",
        "b_adapt": "0",
        "bpyramid": "1",
        "preset": "ultrafast",
    }
    container = av.open(str(path), "w", options={"movflags": "+faststart"})
    stream = container.add_stream("libx264", rate=24, options=options)
    stream.width = FIXTURE_WIDTH
    stream.height = FIXTURE_HEIGHT
    stream.pix_fmt = "yuv420p"
    stream.time_base = FIXTURE_TIME_BASE
    step = 512
    ionly = bframes == 0
    for index in range(frame_count):
        image = rng.integers(
            0, 255, (FIXTURE_HEIGHT, FIXTURE_WIDTH, 3), dtype=np.uint8
        )
        frame = av.VideoFrame.from_ndarray(image, format="rgb24")
        frame.pts = int(pts[index] if pts is not None else index * step)
        if ionly:
            frame.pict_type = av.video.frame.PictureType.I
        elif index and index % 12 == 0:
            frame.pict_type = av.video.frame.PictureType.I
        for packet in stream.encode(frame):
            container.mux(packet)
    for packet in stream.encode():
        container.mux(packet)
    container.close()


def test_clean_clip_exits_zero(tmp_path, capsys):
    clip = tmp_path / "clean.mp4"
    _encode(clip, 48)
    code = main(["analyze", str(clip)])
    out = capsys.readouterr().out
    assert code == 0
    assert "decoded_frames=48" in out
    assert "tb=1/12288" in out


def test_anomalous_clip_exits_two_and_repairs(tmp_path, capsys):
    clip = tmp_path / "bad.mp4"
    pts = [i * 512 for i in range(60)]
    for index in range(30, 60):
        pts[index] += 1024
    _encode(clip, 60, pts=pts)
    code = main(["analyze", str(clip)])
    assert code == 2
    csv_path = tmp_path / "out.csv"
    json_path = tmp_path / "out.json"
    code = main([
        "repair", str(clip), "--csv", str(csv_path),
        "--json", str(json_path), "--fps-override", "24",
    ])
    assert code == 2
    rows = list(csv.reader(open(csv_path)))
    assert len(rows) - 1 == 60
    payload = json.loads(Path(json_path).read_text())
    assert payload["schema_version"]
    assert payload["repair"]["idempotent_pass"] == 0
    repaired = [int(row[3]) for row in rows[1:]]
    intervals = {
        repaired[i] - repaired[i - 1] for i in range(1, len(repaired))
    }
    assert intervals == {512}


def test_truncated_clip_exits_three(tmp_path):
    clip = tmp_path / "full.mp4"
    _encode(clip, 60)
    trunc = tmp_path / "trunc.mp4"
    blob = clip.read_bytes()
    trunc.write_bytes(blob[: int(len(blob) * 0.7)])
    json_path = tmp_path / "t.json"
    code = main(["analyze", str(trunc), "--json", str(json_path)])
    assert code == 3
    payload = json.loads(Path(json_path).read_text())
    assert payload["truncated"] is True
    assert payload["stats"]["decoded_frames"] < 60


def test_missing_file_is_usage_error(tmp_path):
    with pytest.raises(SystemExit) as exc:
        main(["analyze", str(tmp_path / "nope.mp4")])
    assert exc.value.code == 4


def test_bad_reorder_depth_is_usage_error(tmp_path):
    clip = tmp_path / "c.mp4"
    _encode(clip, 24)
    with pytest.raises(SystemExit) as exc:
        main(["analyze", str(clip), "--reorder-depth", "64"])
    assert exc.value.code == 4


def test_no_video_stream_exits_three(tmp_path):
    text = tmp_path / "notes.txt"
    text.write_text("not a video")
    code = main(["analyze", str(text)])
    assert code == 3


def test_pure_i_frames_are_counted(tmp_path, capsys):
    clip = tmp_path / "ionly.mp4"
    _encode(clip, 24, bframes=0)
    code = main(["analyze", str(clip)])
    out = capsys.readouterr().out
    assert code in (0, 2)
    assert "I=24" in out
    assert "P=0" in out
    assert "B=0" in out


def test_fps_override_changes_nominal_rate(tmp_path, capsys):
    clip = tmp_path / "c.mp4"
    _encode(clip, 24)
    code = main(["analyze", str(clip), "--fps-override", "30"])
    out = capsys.readouterr().out
    assert code in (0, 2)
    assert "fps=30.000" in out


def test_fixtures_subcommand_generates_four_clips(tmp_path, capsys):
    out_dir = tmp_path / "fx"
    code = main(["fixtures", "--out", str(out_dir), "--seed", "7"])
    assert code == 0
    names = {p.name for p in out_dir.glob("*.mp4")}
    assert names == {
        "clean_bframes.mp4",
        "bpyramid_gap.mp4",
        "vfr_jitter.mp4",
        "truncated_tail.mp4",
    }


def _encode_mkv(path: Path, frame_count: int, seed=2):
    rng = np.random.default_rng(seed)
    container = av.open(str(path), "w")
    stream = container.add_stream(
        "libx264", rate=24, options={"bf": "1", "preset": "ultrafast"}
    )
    stream.width = FIXTURE_WIDTH
    stream.height = FIXTURE_HEIGHT
    stream.pix_fmt = "yuv420p"
    for index in range(frame_count):
        image = rng.integers(
            0, 255, (FIXTURE_HEIGHT, FIXTURE_WIDTH, 3), dtype=np.uint8
        )
        frame = av.VideoFrame.from_ndarray(image, format="rgb24")
        frame.pts = index
        for packet in stream.encode(frame):
            container.mux(packet)
    for packet in stream.encode():
        container.mux(packet)
    container.close()


def test_alternative_time_base_is_read_from_stream(tmp_path):
    clip = tmp_path / "alt.mkv"
    _encode_mkv(clip, 24)
    result = main(["analyze", str(clip)])
    assert result in (0, 2)


def test_reorder_infers_missing_pts_from_dts():
    from cinecheck.decode import DecodedFrame
    from cinecheck.reorder import reorder_frames

    frames = [
        DecodedFrame(0, None, 10, True, "I", 0),
        DecodedFrame(1, None, 30, False, "P", 1),
        DecodedFrame(2, None, 20, False, "B", 2),
    ]
    result = reorder_frames(frames)
    assert result.inferred_pts == 3
    assert [frame.pts for frame in result.frames] == [10, 20, 30]
