from __future__ import annotations

from pathlib import Path

import pytest

from mediaqc.cli import probe_path
from mediaqc.model import SEVERITIES
from mediaqc.packager import PackError, pack_file
from mediaqc.rules import RULE_CATALOG
from mediaqc.store import Database


def _rules(path: Path) -> set[str]:
    return {finding.rule_id for finding in probe_path(path).findings}


def test_rule_catalog_has_twelve_rule_ids_and_severities() -> None:
    assert len(RULE_CATALOG) >= 12
    assert set(RULE_CATALOG.values()) <= set(SEVERITIES)


def test_duration_drift_and_short_audio(fixture_dir: Path) -> None:
    result = probe_path(fixture_dir / "drift_audio.mp4")
    assert result.drift_ms == pytest.approx(2000.0)
    finding = next(item for item in result.findings if item.rule_id == "AUDIO_VIDEO_DRIFT")
    assert finding.severity == "warning"
    assert "2000.000ms" in finding.evidence
    assert probe_path(fixture_dir / "short_audio.mp4").drift_ms == pytest.approx(-2000.0)


def test_table_offset_overlap_and_interleave_rules(fixture_dir: Path) -> None:
    assert "STSZ_STTS_COUNT_MISMATCH" in _rules(fixture_dir / "stsz_stts_mismatch.mp4")
    assert "CHUNK_OFFSET_OUT_OF_RANGE" in _rules(fixture_dir / "bad_stco.mp4")
    assert "CHUNK_OVERLAP" in _rules(fixture_dir / "chunk_overlap.mp4")


def test_edit_ctts_timescale_and_sync_rules(fixture_dir: Path) -> None:
    assert "EMPTY_EDIT" in _rules(fixture_dir / "empty_edit.mp4")
    assert "NEGATIVE_CTTS" in _rules(fixture_dir / "negative_ctts.mp4")
    assert "TIMESCALE_ZERO" in _rules(fixture_dir / "zero_timescale.mp4")
    assert "NO_SYNC_SAMPLE" in _rules(fixture_dir / "no_sync.mp4")


def test_codec_parameter_missing(fixture_dir: Path) -> None:
    findings = probe_path(fixture_dir / "missing_sps.mp4").findings
    item = next(finding for finding in findings if finding.rule_id == "CODEC_PARAM_MISSING")
    assert item.severity == "error"
    assert item.track_index == 0
    assert item.offset is not None


def test_store_is_idempotent_and_aggregates_by_rule(fixture_dir: Path, tmp_path: Path) -> None:
    db = Database(tmp_path / "media.db")
    files = [fixture_dir / "drift_audio.mp4", fixture_dir / "negative_ctts.mp4"]
    with db.writer() as writer:
        for path in files:
            writer.write(probe_path(path))
    first = db.summary()
    assert first["files"] == 2
    with db.writer() as writer:
        for path in files:
            writer.write(probe_path(path))
    second = db.summary()
    assert second["files"] == 2
    assert second["findings_total"] == first["findings_total"]
    queried = db.findings("warning")
    assert queried["by_rule"]["NEGATIVE_CTTS"] == 1
    assert queried["by_rule"]["AUDIO_VIDEO_DRIFT"] == 1
    assert queried["by_severity"]["error"] == 0


def test_pack_segments_and_rejects_out_of_bounds(fixture_dir: Path, tmp_path: Path) -> None:
    manifest = pack_file(fixture_dir / "drift_audio.mp4", 4000, tmp_path / "pack")
    assert manifest["segments"] == 3
    assert manifest["max_duration_deviation_ms"] == pytest.approx(0.0)
    assert manifest["segment_list"][-1]["end_ms"] == pytest.approx(12000.0)
    assert (tmp_path / "pack" / "manifest.json").is_file()
    with pytest.raises(PackError) as caught:
        pack_file(fixture_dir / "bad_stco.mp4", 4000, tmp_path / "bad")
    assert caught.value.kind == "bounds"
    assert len(caught.value.details) == 500
    assert all(item["reason"] == "outside_mdat" for item in caught.value.details)
    with pytest.raises(PackError, match="no sync samples"):
        pack_file(fixture_dir / "no_sync.mp4", 4000, tmp_path / "no-sync")
