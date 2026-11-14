from __future__ import annotations

import json

from frameaudit.fixtures import generate_fixtures
from frameaudit.pipeline import repair_video, scan_video
from frameaudit.verify import verify_outputs


def test_synthetic_pipeline_is_deterministic_and_verifiable(tmp_path) -> None:
    fixture_dir = tmp_path / "fixtures"
    fixture_report = generate_fixtures(fixture_dir, seed=7)
    sample = fixture_report["files"]["vfr_bpyramid"]

    scan = scan_video(sample)
    assert scan["vfr"] is True
    assert scan["decoded_frames"] == 1440
    assert scan["i_frames"] == 45
    assert scan["keyframe_count"] == 45
    assert 450 <= scan["p_frames"] <= 600
    assert 800 <= scan["b_frames"] <= 950
    assert scan["frame_interval_peak_ms"] >= 1700

    first_index = tmp_path / "first.jsonl"
    first_proxy = tmp_path / "first.mp4"
    first = repair_video(sample, first_index, first_proxy)
    assert first["index_rows"] == first["proxy_frames"] == 1440
    assert 30 <= first["repaired_count"] <= 60
    assert 1700 <= first["max_jump_before_ms"] <= 1900
    assert first["max_jump_after_ms"] <= 42

    second_index = tmp_path / "second.jsonl"
    second_proxy = tmp_path / "second.mp4"
    repair_video(sample, second_index, second_proxy)
    assert first_index.read_bytes() == second_index.read_bytes()

    verification = verify_outputs(first_index, first_proxy)
    assert verification["ok"] is True
    assert verification["hash_mismatches"] == 0
    assert verification["first_pts_us"] == verification["proxy_first_pts_us"]
    assert verification["last_pts_us"] == verification["proxy_last_pts_us"]


def test_truncated_packet_is_reported_without_failing_the_scan(tmp_path) -> None:
    report = generate_fixtures(tmp_path / "fixtures", seed=7)
    scan = scan_video(report["files"]["truncated_packet"])
    assert scan["bad_regions"]
    assert scan["bad_regions"][0]["kind"] in {"bad_packet", "truncated"}
    assert scan["decoded_frames"] > 0


def test_verify_detects_hash_tampering(tmp_path) -> None:
    fixture = generate_fixtures(tmp_path / "fixtures", seed=7)
    index = tmp_path / "index.jsonl"
    proxy = tmp_path / "proxy.mp4"
    repair_video(fixture["files"]["vfr_bpyramid"], index, proxy)

    rows = [json.loads(line) for line in index.read_text(encoding="utf-8").splitlines()]
    rows[3]["plane_hash"] = "tampered"
    index.write_text(
        "".join(json.dumps(row, separators=(",", ":")) + "\n" for row in rows),
        encoding="utf-8",
    )

    verification = verify_outputs(index, proxy)
    assert verification["ok"] is False
    assert verification["hash_mismatches"] == 1

