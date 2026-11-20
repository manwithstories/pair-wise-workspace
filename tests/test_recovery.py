"""End-to-end recovery-chain tests: policies, determinism and the CLI."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from archive_media.audio import AudioConfig
from archive_media.cli import main as cli_main
from archive_media.errors import Code, FatalDamageError
from archive_media.manifest import parse_manifest
from archive_media.pipeline import Policy
from archive_media.pipeline_runner import RecoveryRunner
from archive_media.verify import (
    input_fingerprint,
    verify_segment_hashes,
)

from fixture_media import (
    corrupt_packet_region,
    encode_adts,
    flip_bytes,
    tone_stereo,
)

CONFIG = AudioConfig(48000, "stereo", target_lufs=-18.0)


def _make_simple_playlist(directory: Path, count: int = 4) -> Path:
    refs = []
    for index in range(count):
        rate = 44100 if index % 2 == 0 else 48000
        ref = f"seg-{index}.aac"
        encode_adts(
            directory / ref,
            tone_stereo(rate, 1.0, 400.0 + 40.0 * index),
            rate,
        )
        refs.append(ref)
    lines = ["#EXTM3U", "#EXT-X-TARGETDURATION:1", "#ARCHIVE-REP:r0"]
    for ref in refs:
        lines.append("#EXTINF:1.0,")
        lines.append(ref)
    manifest = directory / "program.m3u8"
    manifest.write_text("\n".join(lines) + "\n")
    return manifest


def test_clean_run_is_deterministic(tmp_path):
    manifest = _make_simple_playlist(tmp_path)
    first = RecoveryRunner(
        parse_manifest(manifest), CONFIG, tmp_path / "out-a",
        policy=Policy.GAP, queue_size=4,
    )
    first.run()
    second = RecoveryRunner(
        parse_manifest(manifest), CONFIG, tmp_path / "out-b",
        policy=Policy.GAP, queue_size=2,
    )
    second.run()
    assert [o["sha256"] for o in first.outputs] == [
        o["sha256"] for o in second.outputs
    ]
    report = verify_segment_hashes(first.outputs)
    assert report.mismatches == 0 and report.count == 4


def test_recoverable_damage_keeps_timeline_with_silence(tmp_path):
    manifest = _make_simple_playlist(tmp_path)
    flip_bytes(tmp_path / "seg-2.aac")
    runner = RecoveryRunner(
        parse_manifest(manifest), CONFIG, tmp_path / "out",
        policy=Policy.GAP, queue_size=4,
    )
    runner.run()
    statuses = {o["sequence"]: o for o in runner.outputs}
    assert all(o["written_samples"] == 48000 for o in runner.outputs)
    assert statuses[2]["status"] in {"recovered", "gap"}
    assert runner.collector.damaged_segments >= 1


def test_skip_policy_emits_zero_length_placeholder(tmp_path):
    manifest = _make_simple_playlist(tmp_path)
    (tmp_path / "seg-1.aac").unlink()
    runner = RecoveryRunner(
        parse_manifest(manifest), CONFIG, tmp_path / "out",
        policy=Policy.SKIP, queue_size=4,
    )
    runner.run()
    skipped = next(o for o in runner.outputs if o["sequence"] == 1)
    assert skipped["status"] == "skipped"
    assert skipped["written_samples"] == 0


def test_strict_policy_is_fatal_with_locator(tmp_path):
    manifest = _make_simple_playlist(tmp_path)
    corrupt_packet_region(tmp_path / "seg-1.aac", keep_fraction=0.4)
    runner = RecoveryRunner(
        parse_manifest(manifest), CONFIG, tmp_path / "out",
        policy=Policy.STOP, queue_size=4,
    )
    with pytest.raises(FatalDamageError) as exc:
        runner.run()
    assert exc.value.code in {Code.PACKET_CORRUPT, Code.SEGMENT_TRUNCATED}
    assert exc.value.locator.sequence == 1
    assert exc.value.locator.segment_ref == "seg-1.aac"


def test_missing_segment_gap_preserves_duration(tmp_path):
    manifest = _make_simple_playlist(tmp_path, count=3)
    (tmp_path / "seg-2.aac").unlink()
    runner = RecoveryRunner(
        parse_manifest(manifest), CONFIG, tmp_path / "out",
        policy=Policy.GAP, queue_size=4,
    )
    runner.run()
    gap = next(o for o in runner.outputs if o["sequence"] == 2)
    assert gap["status"] == "gap"
    assert gap["written_samples"] == 48000


def test_fingerprint_changes_when_input_changes(tmp_path):
    manifest = _make_simple_playlist(tmp_path)
    before = input_fingerprint(parse_manifest(manifest))
    path = tmp_path / "seg-0.aac"
    path.write_bytes(path.read_bytes()[:-20])
    after = input_fingerprint(parse_manifest(manifest))
    assert before != after


def test_cli_success_and_strict_exit_codes(tmp_path, capsys):
    manifest = _make_simple_playlist(tmp_path)
    code = cli_main([
        "verify", "--input", str(manifest), "--output",
        str(tmp_path / "out"), "--rate", "48000", "--layout", "stereo",
        "--target-lufs", "-18", "--queue-size", "4",
    ])
    assert code == 0
    out = capsys.readouterr().out
    for key in (
        "roundtrip_consistency=", "keyframe_hit_rate=",
        "segment_sha256_count=", "segment_sha256_mismatches=0",
        "queue_peak_items=", "recovered_segments=", "damaged_segments=",
    ):
        assert key in out
    corrupt_packet_region(tmp_path / "seg-1.aac", keep_fraction=0.4)
    strict = cli_main([
        "verify", "--input", str(manifest), "--output",
        str(tmp_path / "out-strict"), "--strict",
    ])
    assert strict == 2


def test_cli_syntax_error_exit_code_three(tmp_path):
    manifest = tmp_path / "bad.m3u8"
    manifest.write_text("not a playlist\n")
    code = cli_main([
        "verify", "--input", str(manifest),
        "--output", str(tmp_path / "out"),
    ])
    assert code == 3


def test_cli_bad_parameter_exit_code_three(tmp_path):
    manifest = _make_simple_playlist(tmp_path)
    code = cli_main([
        "verify", "--input", str(manifest),
        "--output", str(tmp_path / "out"), "--queue-size", "0",
    ])
    assert code == 3


def test_report_json_is_written_and_valid(tmp_path):
    manifest = _make_simple_playlist(tmp_path)
    cli_main([
        "verify", "--input", str(manifest),
        "--output", str(tmp_path / "out"),
    ])
    report = json.loads(
        (tmp_path / "out" / "recovery_report.json").read_text()
    )
    assert report["segment_sha256_mismatches"] == 0
    assert report["queue_peak_items"] <= 4
    assert report["timeline_coverage"] == 1.0
