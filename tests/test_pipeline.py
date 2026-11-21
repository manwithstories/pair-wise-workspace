"""End-to-end pipeline tests over the full synthetic scenario set."""

import json

import pytest

from archive_media.cli import main as cli_main
from archive_media.index import MediaIndex
from archive_media.pipeline import Pipeline, PipelineConfig
from fixtures import build_fixtures


@pytest.fixture(scope="module")
def fixture_set(tmp_path_factory):
    root = str(tmp_path_factory.mktemp("fx"))
    return build_fixtures(root, target_kbps=900)


def _run(fixture_set, db_path):
    wrap = {c.name: c.wrap_period_ticks for c in fixture_set.cases if c.wrap_period_ticks}
    with MediaIndex(db_path) as index:
        pipeline = Pipeline(PipelineConfig(chunk_size=4, target_kbps=900), index)
        return pipeline.run_batch(fixture_set.cases, wrap_periods=wrap), pipeline


def test_all_files_met_bitrate_and_alignment(fixture_set, tmp_path):
    results, _ = _run(fixture_set, str(tmp_path / "i.db"))
    by_name = {r.metrics.source.split("/")[-1]: r.metrics for r in results}
    for name, metrics in by_name.items():
        assert abs(metrics.bitrate_delta_pct) <= 10.0, name
        assert metrics.manifest_alignment_ms <= 40.0, name


def test_silence_and_duplicate_detection(fixture_set, tmp_path):
    results, _ = _run(fixture_set, str(tmp_path / "i.db"))
    by_name = {r.metrics.source.split("/")[-1]: r.metrics for r in results}
    assert by_name["silence.mp4"].silence_count >= 1
    assert by_name["dup_a.mp4"].duplicate_count == 1
    assert by_name["dup_b.mp4"].duplicate_count == 1
    # Same timestamps, different content must never be flagged.
    assert by_name["distinct.mp4"].duplicate_count == 0


def test_corruption_and_truncation_are_recoverable(fixture_set, tmp_path):
    results, pipeline = _run(fixture_set, str(tmp_path / "i.db"))
    by_name = {r.metrics.source.split("/")[-1]: r.metrics for r in results}
    assert by_name["corrupted.mp4"].status == "recovered"
    assert by_name["corrupted.mp4"].recoverable_errors >= 1
    assert by_name["truncated.mp4"].status == "recovered"
    exit_code, failures = Pipeline.aggregate_exit(results)
    assert exit_code == 2  # recovered, not fatal
    assert failures == []


def test_resume_uses_checkpoint_and_is_compatible(fixture_set, tmp_path):
    db = str(tmp_path / "resume.db")
    first, _ = _run(fixture_set, db)
    assert all(not r.metrics.checkpoint_resumed for r in first)
    second, _ = _run(fixture_set, db)
    assert all(r.metrics.checkpoint_resumed for r in second)


def test_manifest_is_stable_across_runs(fixture_set, tmp_path):
    from archive_media.cli import _write_manifest

    r1, _ = _run(fixture_set, str(tmp_path / "a.db"))
    r2, _ = _run(fixture_set, str(tmp_path / "b.db"))
    p1, p2 = tmp_path / "m1.jsonl", tmp_path / "m2.jsonl"
    _write_manifest(str(p1), r1)
    _write_manifest(str(p2), r2)
    lines1 = [json.loads(x) for x in p1.read_text().splitlines()]
    lines2 = [json.loads(x) for x in p2.read_text().splitlines()]
    # Same record types in the same deterministic order.
    assert [r["record"] for r in lines1] == [r["record"] for r in lines2]
    assert len(lines1) == len(lines2)


def test_demo_cli_exit_code(tmp_path):
    workdir = str(tmp_path / "demo")
    code = cli_main([
        "demo", "--workdir", workdir,
        "--chunk-size", "4", "--target-kbps", "900", "--resume",
    ])
    assert code == 2  # scenarios include recoverable corruption
    manifest = tmp_path / "demo" / "manifest.jsonl"
    assert manifest.exists()
    rows = [json.loads(x) for x in manifest.read_text().splitlines()]
    file_rows = [r for r in rows if r["record"] == "file"]
    assert len(file_rows) == len(list((tmp_path / "demo" / "fixtures" / "media").glob("*.mp4")))
    required = {
        "target_kbps", "actual_kbps", "bitrate_delta_pct", "video_frames",
        "audio_rms", "silence_count", "duplicate_count",
        "manifest_alignment_ms", "checkpoint_resumed",
    }
    for row in file_rows:
        assert required <= set(row["metrics"])
