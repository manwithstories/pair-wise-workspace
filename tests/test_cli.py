from __future__ import annotations

import json
import sqlite3
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def _run(image: Path, manifest: Path, work: Path) -> subprocess.CompletedProcess:
    return subprocess.run(
        [
            sys.executable,
            "-m",
            "forensictl.cli",
            "run",
            "--image",
            str(image),
            "--manifest",
            str(manifest),
            "--work",
            str(work),
            "--bucket",
            "300",
        ],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )


def test_run_is_idempotent_and_checkpoint_resumes(
    image_path: Path, manifest_path: Path, tmp_path: Path
) -> None:
    work = tmp_path / "work.db"
    first = _run(image_path, manifest_path, work)
    assert first.returncode == 1
    assert "events=1284" in first.stdout
    assert "hash_mismatch=3" in first.stdout
    assert "missing_ranges=2" in first.stdout
    assert first.stdout.rstrip().endswith("exit=1")

    second = _run(image_path, manifest_path, work)
    assert second.returncode == 1
    assert second.stdout == first.stdout

    with sqlite3.connect(work) as connection:
        connection.execute("UPDATE meta SET value='carved' WHERE key='stage'")
        connection.execute("DELETE FROM event_rows")
        connection.execute("DELETE FROM missing_ranges")
        connection.commit()
    resumed = _run(image_path, manifest_path, work)
    assert resumed.returncode == 1
    assert resumed.stdout == first.stdout


def test_report_json_is_computed_from_database(
    image_path: Path, manifest_path: Path, tmp_path: Path
) -> None:
    work = tmp_path / "work.db"
    _run(image_path, manifest_path, work)
    completed = subprocess.run(
        [
            sys.executable,
            "-m",
            "forensictl.cli",
            "report",
            "--work",
            str(work),
            "--json",
            "-",
        ],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    assert completed.returncode == 1
    value = json.loads(completed.stdout)
    assert value["events"] == 1284
    assert value["hash_mismatch"] == 3
    assert value["missing_ranges"] == 2
    assert value["exit"] == 1


def test_invalid_bucket_is_input_error(
    image_path: Path, manifest_path: Path, tmp_path: Path
) -> None:
    completed = subprocess.run(
        [
            sys.executable,
            "-m",
            "forensictl.cli",
            "run",
            "--image",
            str(image_path),
            "--manifest",
            str(manifest_path),
            "--work",
            str(tmp_path / "work.db"),
            "--bucket",
            "0",
        ],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    assert completed.returncode == 2
    assert "error:" in completed.stderr
