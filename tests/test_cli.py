"""CLI tests: exit codes, JSON shape and the acceptance commands.

The demo/verify pair is exercised end-to-end on a reduced corpus (one format,
short assets) so the suite stays fast while still proving the acceptance path
prints the required metrics and returns the documented codes.
"""

from __future__ import annotations

import json
import pytest

from aircheck.cli import main
from aircheck.timebase import ExitCode


def _run(capsys, argv):
    code = main(argv)
    captured = capsys.readouterr()
    return code, captured.out, captured.err


def test_no_command_prints_help_and_returns_usage(capsys):
    code, out, err = _run(capsys, [])
    assert code == ExitCode.USAGE
    assert "usage:" in out


def test_version_flag(capsys):
    code, out, _ = _run(capsys, ["--version"])
    assert code == ExitCode.OK
    assert out.strip()


def test_unknown_format_is_a_usage_error(capsys, tmp_path):
    code, _out, err = _run(
        capsys, ["demo", "--formats", "avi", "--output", str(tmp_path / "out")]
    )
    assert code == ExitCode.USAGE
    assert "unsupported format" in err


def test_non_positive_queue_size_is_a_usage_error(capsys, tmp_path):
    code, _out, err = _run(
        capsys,
        ["demo", "--formats", "mp4", "--queue-size", "0", "--output", str(tmp_path / "o")],
    )
    assert code == ExitCode.USAGE
    assert "queue-size" in err


def test_probe_reports_metadata(capsys, multi_mp4):
    code, out, _ = _run(capsys, ["probe", "--input", str(multi_mp4.path)])
    assert code == ExitCode.OK
    assert "format=" in out
    assert "#0" in out


def test_probe_json_is_parseable(capsys, multi_mp4):
    code, out, _ = _run(capsys, ["probe", "--input", str(multi_mp4.path), "--json"])
    assert code == ExitCode.OK
    payload = json.loads(out)
    assert len(payload["streams"]) == 4


def test_probe_missing_file_is_input_error(capsys, tmp_path):
    code, _out, err = _run(capsys, ["probe", "--input", str(tmp_path / "missing.mp4")])
    assert code == ExitCode.INPUT
    assert "error[input]" in err


def test_map_reports_dropped_streams(capsys, multi_mp4):
    code, out, _ = _run(
        capsys,
        ["map", "--input", str(multi_mp4.path), "--map", "v:0=a:2"],
    )
    assert code == ExitCode.OK
    assert "dropped streams: [1, 3]" in out


def test_map_rejects_bad_stream_number(capsys, multi_mp4):
    code, _out, err = _run(
        capsys, ["map", "--input", str(multi_mp4.path), "--map", "v:9"]
    )
    assert code == ExitCode.INPUT
    assert "stream_index" in err


def test_index_reports_hit_rate(capsys, multi_mov):
    code, out, _ = _run(capsys, ["index", "--input", str(multi_mov.path)])
    assert code == ExitCode.OK
    assert "hit_rate=" in out
    assert "keyframes=" in out


def test_audio_reports_channels_and_peak(capsys, multi_mkv):
    code, out, _ = _run(capsys, ["audio", "--input", str(multi_mkv.path)])
    assert code == ExitCode.OK
    assert "channels=2" in out
    assert "peak=" in out


def test_remux_roundtrip_succeeds(capsys, multi_mp4, tmp_path):
    output = tmp_path / "remuxed.mp4"
    code, out, _ = _run(
        capsys,
        ["remux", "--input", str(multi_mp4.path), "--output", str(output), "--target", "mp4"],
    )
    assert code == ExitCode.OK
    assert "segment_hash_match=1.000" in out
    assert output.exists()


def test_remux_does_not_overwrite(capsys, multi_mp4, tmp_path):
    output = tmp_path / "taken.mp4"
    output.write_bytes(b"keep me")
    code, _out, err = _run(
        capsys,
        ["remux", "--input", str(multi_mp4.path), "--output", str(output), "--target", "mp4"],
    )
    assert code == ExitCode.UNRECOVERABLE
    assert "overwrite" in err
    assert output.read_bytes() == b"keep me"


def test_cancelled_remux_returns_unrecoverable_and_leaves_checkpoint(
    capsys, multi_mkv, tmp_path
):
    output = tmp_path / "cancelled.mkv"
    code, _out, err = _run(
        capsys,
        [
            "remux",
            "--input", str(multi_mkv.path),
            "--output", str(output),
            "--target", "mkv",
            "--slow-consumer-ms", "40",
            "--cancel-after-ms", "20",
        ],
    )
    assert code == ExitCode.UNRECOVERABLE
    assert "cancelled" in err
    assert (tmp_path / "cancelled.mkv.resume.json").exists()


@pytest.mark.parametrize("formats", ["mp4", "mov", "mkv"])
def test_demo_acceptance_metrics(capsys, tmp_path, formats):
    output = tmp_path / f"demo-{formats}"
    code, out, _ = _run(
        capsys,
        [
            "demo",
            "--formats", formats,
            "--queue-size", "4",
            "--slow-consumer-ms", "1",
            "--output", str(output),
        ],
    )
    assert code == ExitCode.OK, out
    text = out
    assert "roundtrip_parse_rate=1.000" in text
    assert "segment_hash_match=1.000" in text
    assert "keyframe_hit_rate=" in text
    assert "peak_queue_items=" in text
    assert "peak_rss_mb=" in text
    assert "damaged_segments=" in text


def test_demo_json_contains_thresholds_and_exit_code(capsys, tmp_path):
    output = tmp_path / "demo-json"
    code, out, _ = _run(
        capsys,
        ["demo", "--formats", "mp4", "--slow-consumer-ms", "1",
         "--output", str(output), "--json"],
    )
    assert code == ExitCode.OK
    payload = json.loads(out)
    assert payload["exit_code"] == int(ExitCode.OK)
    assert payload["metrics"]["segment_hash_match"] == 1.0
    assert payload["peak_queue_items"] <= payload["queue_size"]
    assert payload["damaged_segments"] >= 1
    assert payload["thresholds"]["keyframe_hit_rate"] == 0.99


def test_verify_after_demo_reports_acceptance(capsys, tmp_path):
    output = tmp_path / "demo-then-verify"
    _run(capsys, ["demo", "--formats", "mkv", "--slow-consumer-ms", "1",
                  "--output", str(output)])
    code, out, _ = _run(capsys, ["verify", "--input", str(output), "--json"])
    assert code == ExitCode.OK
    payload = json.loads(out)
    assert payload["roundtrip_parse_rate"] == 1.0
    assert payload["segment_hash_match"] == 1.0
    assert payload["keyframe_hit_rate"] >= 0.99
    assert payload["damaged_segments"] >= 1


def test_verify_missing_directory_is_input_error(capsys, tmp_path):
    code, _out, err = _run(capsys, ["verify", "--input", str(tmp_path / "nope")])
    assert code == ExitCode.INPUT
    assert "does not exist" in err


def test_verify_without_pairs_reports_zero_and_differs(capsys, tmp_path):
    empty = tmp_path / "empty"
    empty.mkdir()
    code, out, _ = _run(capsys, ["verify", "--input", str(empty), "--json"])
    assert code == ExitCode.ACCEPTANCE_DIFF
    payload = json.loads(out)
    assert payload["roundtrip_parse_rate"] == 0.0
    assert payload["notes"]
