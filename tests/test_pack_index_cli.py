from __future__ import annotations

import hashlib
import json
import sqlite3
from pathlib import Path

import pytest
from click.testing import CliRunner

from mediaprobe.checks import check_paths
from mediaprobe.cli import cli
from mediaprobe.errors import IndexWriteError
from mediaprobe.index import IndexDB
from mediaprobe.packager import pack
from mediaprobe.samplegen import generate_samples


@pytest.fixture(scope="module")
def corpus(tmp_path_factory) -> Path:
    destination = tmp_path_factory.mktemp("pack-fixtures")
    generate_samples(destination, 14, 7)
    return destination


def _counts(path: Path) -> dict[str, int]:
    connection = sqlite3.connect(path)
    try:
        return {
            table: connection.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0]
            for table in ("assets", "tracks", "issues", "runs")
        }
    finally:
        connection.close()


def test_pack_counts_slices_and_manifest_hash(corpus: Path, tmp_path: Path) -> None:
    bundle = tmp_path / "bundle"
    db_path = tmp_path / "index.sqlite3"
    result = pack(corpus, "profiles/delivery.json", bundle, db_path, 40)
    assert (result["asset_count"], result["passed"], result["rejected"]) == (14, 11, 3)
    assert result["track_count"] == 28
    assert result["tracks_written"] == 6
    assert len(result["manifest_sha256"]) == 64
    assert _counts(db_path) == {"assets": 14, "tracks": 28, "issues": 7, "runs": 1}
    assert all((bundle / track["stored_path"]).is_file() for track in result["tracks"])
    rejected = [asset for asset in result["assets"] if asset["status"] == "rejected"]
    assert len(rejected) == 3
    for asset in rejected:
        assert asset["repair_advice"]
        assert all(item["box_path"] and "offset" in item["advice"] for item in asset["repair_advice"])
    manifest_on_disk = json.loads((bundle / "manifest.json").read_text(encoding="utf-8"))
    digest = manifest_on_disk.pop("manifest_sha256")
    canonical = json.dumps(manifest_on_disk, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()
    assert hashlib.sha256(canonical).hexdigest() == digest


def test_pack_reuse_keeps_index_row_counts(corpus: Path, tmp_path: Path) -> None:
    bundle = tmp_path / "bundle-reuse"
    db_path = tmp_path / "reuse.sqlite3"
    first = pack(corpus, "profiles/delivery.json", bundle, db_path, 40)
    second = pack(corpus, "profiles/delivery.json", bundle, db_path, 40)
    assert not any(asset["reused"] for asset in first["assets"] if asset["status"] == "passed")
    assert all(asset["reused"] for asset in second["assets"] if asset["status"] == "passed")
    assert _counts(db_path) == {"assets": 14, "tracks": 28, "issues": 7, "runs": 1}


def test_index_upsert_is_idempotent_and_transaction_rolls_back(corpus: Path, tmp_path: Path) -> None:
    report = check_paths(corpus, "profiles/delivery.json", 40)
    database_path = tmp_path / "idempotent.sqlite3"
    with IndexDB(database_path) as database:
        database.ingest_report(report)
        first = database.counts()
        database.ingest_report(report)
        assert database.counts() == first
        assert first == {"assets": 14, "tracks": 28, "issues": 7, "runs": 0}
    rollback_path = tmp_path / "rollback.sqlite3"
    with IndexDB(rollback_path) as database:
        with pytest.raises(IndexWriteError):
            database.ingest_report(report, {})
        assert database.counts() == {"assets": 0, "tracks": 0, "issues": 0, "runs": 0}


def test_cli_exit_codes_and_json(corpus: Path, tmp_path: Path) -> None:
    runner = CliRunner()
    probe_json = tmp_path / "probe.json"
    probe_result = runner.invoke(
        cli,
        ["probe", str(corpus / "trailer_1080_av.mp4"), "--json", str(probe_json)],
    )
    assert probe_result.exit_code == 0, probe_result.output
    assert json.loads(probe_json.read_text())["track_count"] == 2
    check_result = runner.invoke(
        cli,
        ["check", str(corpus), "--profile", "profiles/delivery.json", "--tolerance-ms", "40"],
    )
    assert check_result.exit_code == 1
    unknown = tmp_path / "unknown.bin"
    unknown.write_bytes(b"not media" * 4)
    assert runner.invoke(cli, ["probe", str(unknown)]).exit_code == 3


def test_cli_index_issues_exit_code(corpus: Path, tmp_path: Path) -> None:
    report = check_paths(corpus, "profiles/delivery.json", 40)
    db_path = tmp_path / "issues.sqlite3"
    with IndexDB(db_path) as database:
        database.ingest_report(report)
    runner = CliRunner()
    result = runner.invoke(cli, ["index", "issues", "--db", str(db_path), "--severity", "blocking"])
    assert result.exit_code == 1
    assert "DUR_DRIFT" in result.output
