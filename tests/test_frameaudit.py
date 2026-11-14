from __future__ import annotations

import json
from collections import Counter
from fractions import Fraction
from pathlib import Path

import pytest

from frameaudit.cli import main
from frameaudit.decode import FrameInfo, decode_file
from frameaudit.fixtures import make_fixtures
from frameaudit.manifest import read_manifest, write_manifest
from frameaudit.reorder import reorder_frames
from frameaudit.repair import repair_timestamps
from frameaudit.report import build_stats
from frameaudit.timeline import build_timeline


@pytest.fixture(scope="session")
def media(tmp_path_factory: pytest.TempPathFactory) -> dict[str, Path]:
    directory = tmp_path_factory.mktemp("frameaudit-fixtures")
    paths = make_fixtures(directory, fps=Fraction(25), seconds=Fraction(6), gop=12)
    return {path.stem: path for path in paths}


def frame(index: int, pts: int, pict_type: str = "P", keyframe: bool = False) -> FrameInfo:
    return FrameInfo(
        decode_index=index,
        packet_index=index,
        pts=pts,
        dts=index * 3600,
        pict_type=pict_type,
        keyframe=keyframe,
        packet_size=10,
    )


def test_fixture_scan_contract(media: dict[str, Path], tmp_path: Path) -> None:
    decoded = decode_file(media["broken"])
    assert abs(decoded.decoded_frames - 150) <= 1
    assert decoded.packets == decoded.decoded_frames
    assert decoded.decode_errors == 0
    types = Counter(item.pict_type for item in decoded.frames)
    assert types["I"] + types["P"] + types["B"] + types["other"] == decoded.decoded_frames

    reordered = reorder_frames(decoded.frames)
    repaired = repair_timestamps(
        reordered.frames,
        time_base=decoded.time_base,
        fps=Fraction(25),
        strategy="scan",
        max_jump_ms=200,
    )
    assert reordered.reordered_frames > 0
    assert reordered.max_reorder_depth >= 1
    assert repaired.anomaly_count >= 12
    assert repaired.max_jump_ms >= 1000
    assert not repaired.source_monotonic

    output = tmp_path / "scan.json"
    assert main(["scan", str(media["broken"]), "--json-out", str(output)]) == 0
    payload = json.loads(output.read_text(encoding="utf-8"))
    assert payload["decoded_frames"] == 150
    assert payload["pict_type"] == {"I": 13, "P": 85, "B": 52, "other": 0}
    assert payload["keyframes"] == 13
    assert payload["packets"] == 150
    assert payload["repaired_timestamps"] >= 12
    assert payload["max_jump_ms"] >= 1000


def test_raw_stream_is_synthetic(media: dict[str, Path]) -> None:
    decoded = decode_file(media["raw"])
    assert decoded.frames
    assert all(item.synthetic for item in decoded.frames)
    assert decoded.frames[0].pts == 0
    pts = [item.pts for item in decoded.frames]
    assert pts == sorted(pts)
    reordered = reorder_frames(decoded.frames)
    assert reordered.fallback_to_decode_order is False
    assert reordered.reordered_frames == 0


def test_bounded_reorder_stable_ties() -> None:
    frames = [
        frame(0, 0, "I", True),
        frame(1, 40),
        frame(2, 20, "B"),
        frame(3, 20, "B"),
    ]
    result = reorder_frames(frames, capacity=4)
    assert [item.decode_index for item in result.frames] == [0, 2, 3, 1]
    assert result.reordered_frames == 1
    assert result.max_reorder_depth == 2


def test_shift_detects_and_repairs_all_anomaly_types() -> None:
    time_base = Fraction(1, 90000)
    frames = [
        frame(0, 1800, "I", True),
        frame(1, 1800),
        frame(2, 900),
        frame(3, 90000),
        frame(4, 93600),
    ]
    result = repair_timestamps(
        frames, time_base=time_base, fps=Fraction(25), strategy="shift", max_jump_ms=200
    )
    assert result.first_offset
    assert result.duplicate_count == 1
    assert result.rollback_count == 1
    assert result.jump_count == 1
    assert result.anomaly_count == 4
    assert result.decisions[0].repaired_pts == 0
    assert result.monotonic
    assert result.residual_max_jump_ms <= 40


def test_cfr_and_rounding_duplicate() -> None:
    time_base = Fraction(1, 90000)
    frames = [frame(0, 0, "I", True), frame(1, 3600), frame(2, 3601), frame(3, 7200)]
    detected = repair_timestamps(
        frames, time_base=time_base, fps=Fraction(25), strategy="scan", max_jump_ms=200
    )
    assert detected.rounding_duplicate_count == 1
    repaired = repair_timestamps(
        frames, time_base=time_base, fps=Fraction(25), strategy="cfr", max_jump_ms=200
    )
    assert [item.repaired_pts for item in repaired.decisions] == [0, 3600, 7200, 10800]
    assert repaired.monotonic
    assert repaired.residual_max_jump_ms == 40


def test_manifest_matches_timeline(media: dict[str, Path], tmp_path: Path) -> None:
    decoded = decode_file(media["broken"])
    reordered = reorder_frames(decoded.frames)
    repaired = repair_timestamps(
        reordered.frames,
        time_base=decoded.time_base,
        fps=Fraction(25),
        strategy="shift",
        max_jump_ms=200,
    )
    timeline = build_timeline(reordered.frames, repaired, decoded.time_base)
    manifest = tmp_path / "timeline.jsonl"
    write_manifest(manifest, timeline)
    rows = read_manifest(manifest)
    assert len(rows) == len(timeline) == decoded.decoded_frames
    assert rows[0] == timeline[0].to_dict()
    stats = build_stats(
        decoded, reordered, repaired, timeline, nominal_fps="25"
    )
    assert stats["decoded_frames"] == len(rows)
    assert stats["repaired_timestamps"] == repaired.anomaly_count


def test_verify_and_exit_codes(media: dict[str, Path], tmp_path: Path) -> None:
    manifest = tmp_path / "timeline.jsonl"
    assert main([
        "repair", str(media["broken"]), "--strategy", "shift", "--manifest", str(manifest)
    ]) == 0
    assert main(["verify", str(media["broken"]), "--manifest", str(manifest)]) == 0

    rows = read_manifest(manifest)
    rows[0]["pict_type"] = "B" if rows[0]["pict_type"] != "B" else "P"
    manifest.write_text(
        "".join(json.dumps(row, separators=(",", ":")) + "\n" for row in rows),
        encoding="utf-8",
    )
    assert main(["verify", str(media["broken"]), "--manifest", str(manifest)]) == 3

    assert main(["scan", str(tmp_path / "missing.mp4")]) == 1
    assert main(["scan", str(media["truncated"])]) == 2
    assert main([
        "scan", str(media["truncated"]), "--max-decode-errors", "1"
    ]) == 0
    assert main([
        "repair", str(media["broken"]), "--strategy", "shift",
        "--manifest", str(tmp_path / "bad.jsonl"), "--max-jump-ms", "0",
    ]) == 4
    with pytest.raises(SystemExit) as error:
        main(["scan", "--fps", "0", str(media["clean"])])
    assert error.value.code == 1


def test_edge_fixtures(media: dict[str, Path], tmp_path: Path) -> None:
    empty_json = tmp_path / "empty.json"
    assert main(["scan", str(media["empty"]), "--json-out", str(empty_json)]) == 0
    assert json.loads(empty_json.read_text(encoding="utf-8"))["decoded_frames"] == 0

    single = decode_file(media["single_frame"])
    assert len(single.frames) == 1
    all_i = decode_file(media["all_i"])
    assert all(item.pict_type == "I" for item in all_i.frames)
    vfr = decode_file(media["vfr"])
    vfr_scan = repair_timestamps(
        reorder_frames(vfr.frames).frames,
        time_base=vfr.time_base,
        fps=Fraction(25),
        strategy="scan",
        max_jump_ms=200,
    )
    assert vfr_scan.source_monotonic
    tb90000 = decode_file(media["tb90000"])
    assert tb90000.time_base == Fraction(1, 90000)
    truncated = decode_file(media["truncated"])
    assert truncated.frames
    assert truncated.decode_errors >= 1
