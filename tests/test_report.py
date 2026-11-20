"""Report tests: manifest gates, verification from scratch and tamper detection."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from mediasync.errors import InputError, QualityGateError
from mediasync.report import (
    BITRATE_TOLERANCE,
    SCHEMA,
    Manifest,
    TrackManifest,
    build_manifest,
    format_summary,
    load_manifest,
    manifest_failures_to_error,
    verify_manifest,
    verify_summary,
)


def track(**overrides) -> TrackManifest:
    base = dict(
        track_id="video0",
        kind="av",
        role="camera",
        codec="h264",
        start_ticks=0,
        end_ticks=450_000,
        complete=True,
        aligned=True,
        max_error_ms=4.0,
        packets=150,
    )
    base.update(overrides)
    return TrackManifest(**base)


def good_manifest() -> Manifest:
    return build_manifest(
        seed=17,
        target_bps=4_000_000,
        total_bytes=2_500_000,
        duration_ticks=450_000,
        tracks=[track(), track(track_id="program", kind="audio", codec="pcm_s16le", packets=0)],
        transport={"loss": 10, "recovered": 10, "unrecoverable": 0},
        audio={"peak": 0.25, "rms": 0.12},
        sync={"max_error_ms": 4.0},
    )


def test_good_manifest_passes() -> None:
    manifest = good_manifest()
    assert manifest.status() == "PASS"
    assert verify_manifest(manifest.to_dict()) == []


def test_bitrate_outside_tolerance_fails() -> None:
    manifest = good_manifest()
    failing = Manifest(
        seed=manifest.seed,
        target_bps=4_000_000,
        actual_bps=int(4_000_000 * (1 + BITRATE_TOLERANCE * 2)),
        duration_ticks=manifest.duration_ticks,
        tracks=manifest.tracks,
        transport=manifest.transport,
        audio=manifest.audio,
        sync=manifest.sync,
    )
    assert failing.status() == "FAIL"
    failures = verify_manifest(failing.to_dict())
    assert any("bitrate" in item for item in failures)


def test_unrecoverable_gap_fails_the_gate() -> None:
    manifest = build_manifest(
        seed=1,
        target_bps=4_000_000,
        total_bytes=2_500_000,
        duration_ticks=450_000,
        tracks=[track()],
        transport={"loss": 10, "recovered": 7, "unrecoverable": 3},
        audio={"peak": 0.2, "rms": 0.1},
    )
    failures = verify_manifest(manifest.to_dict())
    assert any("unrecoverable" in item for item in failures)


def test_alignment_error_above_limit_fails() -> None:
    manifest = build_manifest(
        seed=1,
        target_bps=4_000_000,
        total_bytes=2_500_000,
        duration_ticks=450_000,
        tracks=[track(max_error_ms=25.0)],
        transport={"loss": 0, "recovered": 0, "unrecoverable": 0},
        audio={"peak": 0.2, "rms": 0.1},
    )
    failures = verify_manifest(manifest.to_dict())
    assert any("alignment error" in item for item in failures)


def test_zero_audio_rms_fails() -> None:
    manifest = build_manifest(
        seed=1,
        target_bps=4_000_000,
        total_bytes=2_500_000,
        duration_ticks=450_000,
        tracks=[track()],
        transport={"loss": 0, "recovered": 0, "unrecoverable": 0},
        audio={"peak": 0.0, "rms": 0.0},
    )
    assert manifest.status() == "FAIL"
    assert any("RMS" in item for item in verify_manifest(manifest.to_dict()))


def test_manifest_round_trip(tmp_path: Path) -> None:
    manifest = good_manifest()
    path = manifest.write(tmp_path / "manifest.json")
    payload = load_manifest(path)
    assert payload["schema"] == SCHEMA
    assert verify_manifest(payload) == []
    assert payload["transport"]["unrecoverable"] == 0


def test_tampered_status_is_detected() -> None:
    payload = good_manifest().to_dict()
    payload["transport"]["unrecoverable"] = 5
    payload["status"] = "PASS"
    failures = verify_manifest(payload)
    assert any("claims PASS" in item for item in failures)


def test_unknown_schema_is_rejected() -> None:
    payload = good_manifest().to_dict()
    payload["schema"] = "mediasync/manifest/v9"
    with pytest.raises(InputError):
        verify_manifest(payload)


def test_malformed_sections_are_rejected() -> None:
    payload = good_manifest().to_dict()
    del payload["bitrate"]
    with pytest.raises(InputError):
        verify_manifest(payload)
    payload = good_manifest().to_dict()
    payload["transport"]["loss"] = -1
    with pytest.raises(InputError):
        verify_manifest(payload)
    payload = good_manifest().to_dict()
    payload["tracks"][0]["status"] = "maybe"
    with pytest.raises(InputError):
        verify_manifest(payload)


def test_manifest_rejects_bad_inputs(tmp_path: Path) -> None:
    with pytest.raises(InputError):
        build_manifest(
            seed=1,
            target_bps=4_000_000,
            total_bytes=10,
            duration_ticks=0,
            tracks=[track()],
        )
    with pytest.raises(InputError):
        Manifest(
            seed=1, target_bps=0, actual_bps=0, duration_ticks=10, tracks=[]
        ).deviation
    path = tmp_path / "broken.json"
    path.write_text("{not json")
    with pytest.raises(InputError):
        load_manifest(path)


def test_summary_lines_include_contract_fields() -> None:
    manifest = good_manifest()
    text = "\n".join(format_summary(manifest))
    for needle in (
        "target_bps=4000000",
        "actual_bps=",
        "video_frames=",
        "audio_rms=",
        "loss/recovered/unrecoverable=",
        "manifest_max_error_ms=",
        "status=PASS",
    ):
        assert needle in text
    verify_text = "\n".join(verify_summary(manifest.to_dict(), []))
    assert "status=PASS" in verify_text


def test_failure_error_carries_exit_code() -> None:
    error = manifest_failures_to_error(["x", "y"])
    assert isinstance(error, QualityGateError)
    assert error.exit_code == 4


def test_manifest_max_error_uses_aligned_tracks_only() -> None:
    manifest = build_manifest(
        seed=1,
        target_bps=4_000_000,
        total_bytes=2_500_000,
        duration_ticks=450_000,
        tracks=[track(max_error_ms=3.0), track(track_id="v2", aligned=False, max_error_ms=99.0)],
        transport={"loss": 0, "recovered": 0, "unrecoverable": 0},
        audio={"peak": 0.2, "rms": 0.1},
    )
    assert manifest.manifest_max_error_ms == pytest.approx(3.0)
