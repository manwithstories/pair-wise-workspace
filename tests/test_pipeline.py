"""Segment accounting, manifest verification and codec coverage."""

from __future__ import annotations

import json

import pytest

from mediacore.bitstream import chunk_bytes, scan
from mediacore.manifest import Manifest, build_segments, verify_manifest
from mediacore.pipeline import PipelineConfig, run_pipeline


def _units(path: str):
    data = open(path, "rb").read()
    return [u for u in scan(chunk_bytes(data, 65536), codec="h264")
            if not hasattr(u, "units")]


def test_segments_tile_the_timeline_and_start_at_keyframes(tiny_dir):
    units = _units(tiny_dir["video_path"])
    seconds = [i / 30 for i in range(len(units))]
    segments = build_segments(units, seconds, target_bps=600_000,
                              frame_duration_s=1 / 30)
    assert segments[0].start_s == 0.0
    assert abs(segments[-1].end_s - 2.0) < 1e-6
    for a, b in zip(segments, segments[1:]):
        assert a.end_s == pytest.approx(b.start_s)
        assert a.keyframe_count == 1
    assert sum(s.bytes for s in segments) == sum(u.size for u in units)


def test_segment_digest_covers_content(tiny_dir):
    units = _units(tiny_dir["video_path"])
    seconds = [i / 30 for i in range(len(units))]
    first = build_segments(units, seconds, target_bps=600_000,
                           frame_duration_s=1 / 30)
    second = build_segments(units, seconds, target_bps=600_000,
                            frame_duration_s=1 / 30)
    assert [s.digest for s in first] == [s.digest for s in second]
    second[0].bytes += 1
    assert second[0].digest != first[0].digest


def test_pipeline_manifest_roundtrip_and_verification(tmp_path, tiny_dir):
    config = PipelineConfig(codec="h264", target_bps=600_000, duration_s=2,
                            seed=7, quality_limit=30)
    result = run_pipeline(tiny_dir["video_path"], tiny_dir["audio"]["path"],
                          tmp_path / "run", config)
    manifest_path = tmp_path / "manifest.json"
    result.manifest.save(str(manifest_path))

    loaded = Manifest.load(str(manifest_path))
    assert loaded.digest == result.manifest.digest
    # Two-second fixtures carry proportionally more keyframe overhead, so the
    # overall rate check is exercised with its documented 15% limit separately
    # (see test_acceptance_scenario); here the structural checks must pass.
    assert verify_manifest(loaded, deviation_limit_pct=25.0) == []
    assert loaded.codec == "h264"
    assert loaded.video["units"] == 60
    assert loaded.video["keyframes"] == 4
    assert loaded.audio["sample_rate"] == 48000
    assert loaded.index["entries"]
    assert loaded.segments
    assert loaded.timeline["records"] == 60
    assert loaded.quality["video"]["frames"] == 30


def test_manifest_detects_tampering(tmp_path, tiny_dir):
    config = PipelineConfig(codec="h264", target_bps=600_000, duration_s=2, seed=7)
    result = run_pipeline(tiny_dir["video_path"], tiny_dir["audio"]["path"],
                          tmp_path / "run", config)
    manifest = result.manifest
    manifest.segments[0].bytes += 500
    failures = verify_manifest(manifest)
    assert any("rate is inconsistent" in f for f in failures)
    manifest.quality["video"]["psnr_db"] = float("nan")
    failures = verify_manifest(manifest)
    assert any("quality statistic" in f for f in failures)


def test_verification_enforces_documented_limits(tmp_path, tiny_dir):
    config = PipelineConfig(codec="h264", target_bps=600_000, duration_s=2, seed=7)
    result = run_pipeline(tiny_dir["video_path"], tiny_dir["audio"]["path"],
                          tmp_path / "run", config)
    manifest = result.manifest
    failures = verify_manifest(manifest, alignment_limit_ms=0.001,
                               deviation_limit_pct=0.001)
    assert failures
    assert any("alignment" in f for f in failures)
    assert any("deviation" in f for f in failures)


def test_supplied_index_with_bad_entries_is_reported_in_manifest(tmp_path, tiny_dir):
    units = _units(tiny_dir["video_path"])
    keys = [u for u in units if u.keyframe]
    supplied = {"keyframes": [
        {"offset": keys[0].offset, "pts": 0},
        {"offset": keys[1].offset + 3, "pts": 15},
    ]}
    config = PipelineConfig(codec="h264", target_bps=600_000, duration_s=2, seed=7)
    result = run_pipeline(tiny_dir["video_path"], tiny_dir["audio"]["path"],
                          tmp_path / "run", config, index_payload=supplied)
    index = result.manifest.index["validation"]
    assert index["supplied"] == 2
    assert index["accepted"] == 1
    assert index["rejected"] == 1
    assert index["rejections"][0]["offset"] == keys[1].offset + 3


@pytest.mark.parametrize("codec", ["h265", "av1"])
def test_other_codecs_run_end_to_end(tmp_path, codec):
    from mediacore.fixtures import FixtureSpec, make_fixtures

    spec = FixtureSpec(codec=codec, duration_s=2, bitrate=600_000, seed=9)
    files = make_fixtures(tmp_path / "in", spec)
    config = PipelineConfig(codec=codec, target_bps=600_000, duration_s=2,
                            seed=9, quality_limit=15)
    result = run_pipeline(files["video_path"], files["audio"]["path"],
                          tmp_path / "run", config)
    manifest = result.manifest
    assert manifest.codec == codec
    assert manifest.video["units"] == 60
    assert manifest.video["keyframes"] >= 2
    assert verify_manifest(manifest, deviation_limit_pct=25.0) == []
    assert manifest.quality["video"]["finite"]


def test_acceptance_scenario_meets_all_limits(tmp_path):
    """The documented acceptance command: H.264, 12 s, 900 kbps, seed 41."""
    from mediacore.fixtures import FixtureSpec, make_fixtures

    spec = FixtureSpec(codec="h264", duration_s=12, bitrate=900_000, seed=41)
    files = make_fixtures(tmp_path / "in", spec)
    config = PipelineConfig(codec="h264", target_bps=900_000, duration_s=12,
                            seed=41, gop=15)
    result = run_pipeline(files["video_path"], files["audio"]["path"],
                          tmp_path / "run", config)
    manifest = result.manifest
    assert abs(manifest.deviation_pct) <= 15.0
    assert manifest.manifest_alignment_max_ms <= 40.0
    assert manifest.random_access_max_ms <= 40.0
    assert manifest.quality["video"]["finite"] is True
    assert manifest.audio["rms"] > 0.0
    assert verify_manifest(manifest) == []
