from __future__ import annotations

import json
from pathlib import Path

import pytest

from framefix.report import analyze_media
from framefix.repair import repair_media
from framefix.synth import synth_profile


def test_clean_and_bframe_profiles_are_healthy(clean_path, bframes_path):
    clean = analyze_media(str(clean_path))
    assert clean.status_code == 0
    assert clean.plan.fixed_entries == 0
    assert clean.classifier.b_frames == 0

    bframes = analyze_media(str(bframes_path))
    assert bframes.status_code == 0
    assert bframes.classifier.b_frames > 0
    assert bframes.plan.fixed_entries == 0


def test_broken_profile_exposes_all_injected_faults(broken_path):
    result = analyze_media(str(broken_path))
    reasons = result.plan.reasons
    assert result.plan.fixed_entries > 0
    assert reasons["pts_rollback"] >= 1
    assert reasons["dts_duplicate"] >= 1
    assert reasons["negative_initial_dts"] >= 1
    assert reasons["jump"] >= 1
    assert result.max_jump_ms > 250


def test_repair_roundtrip_writes_report_and_preserves_pixels(broken_path, tmp_path: Path):
    output = tmp_path / "out" / "broken.fixed.mp4"
    report_path = tmp_path / "out" / "broken.report.json"
    outcome = repair_media(str(broken_path), str(output), str(report_path))
    assert outcome.exit_code == 0
    assert output.exists() and report_path.exists()

    report = json.loads(report_path.read_text(encoding="utf-8"))
    assert report["fixed_entries"] > 0
    assert report["reasons"]
    assert report["entries"]
    assert report["pixel_digest_match"] is True
    assert report["residual_entries"] == 0
    assert report["passes"] >= 1

    rescan = analyze_media(str(output))
    assert rescan.status_code == 0
    assert rescan.plan.fixed_entries == 0
    assert rescan.max_jump_ms <= 250
    assert rescan.pixels.hexdigest == analyze_media(str(broken_path)).pixels.hexdigest


def test_hevc_broken_roundtrip(tmp_path: Path):
    source = tmp_path / "hevc-broken.mp4"
    synth_profile(str(source), "broken", codec="hevc", frames=48, width=128, height=72)
    outcome = repair_media(
        str(source), str(tmp_path / "hevc.fixed.mp4"), str(tmp_path / "hevc.json")
    )
    assert outcome.exit_code == 0
    assert outcome.report["pixel_digest_match"] is True
    assert outcome.report["residual_entries"] == 0

