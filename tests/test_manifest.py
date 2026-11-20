"""Manifest schema, provenance mapping and digest stability rules."""

from __future__ import annotations

import json

import pytest

from mediacore.errors import MediaInputError
from mediacore.manifest import MANIFEST_VERSION, Manifest, Segment


def _segment(ordinal: int = 0) -> Segment:
    return Segment(
        ordinal=ordinal, unit_start=0, unit_end=15, byte_start=0, byte_end=1000,
        start_s=0.0, end_s=0.5, keyframe_offsets=[0], keyframe_count=1,
        bytes=1000, seconds=0.5, target_bps=800_000, actual_bps=16_000.0,
        deviation_pct=-98.0, decisions=["duplicate"])


def _manifest() -> Manifest:
    return Manifest(
        run_id="r1", codec="h264", time_base={"num": 1, "den": 30},
        target_bps=800_000, actual_bps=799_000.0, deviation_pct=-0.125,
        video={"path": "/a/video.h264", "bytes": 1000, "units": 15,
               "sha256": "abc"},
        audio={"path": "/a/audio.wav", "rms": 0.2, "sample_rate": 48000},
        segments=[_segment()],
        index={"entries": [{"offset": 0}], "random_access_max_ms": 0.0},
        timeline={"records": 15}, quality={"video": {"psnr_db": 40.0}},
        alignment={"max_alignment_ms": 3.0},
        inputs=[{"path": "/a/video.h264", "size": 1000, "digest": "d"},
                {"path": "/a/audio.wav", "size": 1000, "digest": "e"}],
        checkpoint={"path": "/run/checkpoint.json", "runs": 1,
                    "resumed": False, "stages": ["scan"]},
        resource={"queue": {"high_water": 3}}, duration_s=0.5)


def test_segment_has_provenance_digest_and_switch_point_fields():
    segment = _segment()
    payload = segment.to_dict()
    assert payload["digest"] == segment.digest
    assert payload["keyframe_offsets"] == [0]
    assert "decisions" in payload
    assert {"byte_start", "byte_end", "start_s", "end_s"} <= payload.keys()


def test_manifest_roundtrip_preserves_digest(tmp_path):
    manifest = _manifest()
    path = tmp_path / "manifest.json"
    manifest.save(str(path))
    loaded = Manifest.load(str(path))
    assert loaded.digest == manifest.digest
    assert loaded.segments[0].digest == manifest.segments[0].digest
    raw = json.loads(path.read_text())
    assert raw["manifest_digest"] == manifest.digest


def test_digest_ignores_run_bookkeeping_but_not_media_data(tmp_path):
    manifest = _manifest()
    baseline = manifest.digest
    manifest.checkpoint["runs"] = 7
    manifest.checkpoint["resumed"] = True
    manifest.checkpoint["path"] = "/elsewhere/checkpoint.json"
    manifest.resource["different"] = True
    assert manifest.digest == baseline

    manifest.audio["rms"] = 0.3
    assert manifest.digest != baseline


def test_digest_is_location_independent(tmp_path):
    a = _manifest()
    b = _manifest()
    b.video["path"] = "/different/root/video.h264"
    b.audio["path"] = "/different/root/audio.wav"
    b.inputs[0]["path"] = "/different/root/video.h264"
    assert a.digest == b.digest


def test_unknown_manifest_version_is_rejected(tmp_path):
    path = tmp_path / "manifest.json"
    path.write_text(json.dumps({"version": MANIFEST_VERSION + 1}))
    with pytest.raises(MediaInputError):
        Manifest.load(str(path))
