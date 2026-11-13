from __future__ import annotations

import json
import shutil
import subprocess
import sys
from pathlib import Path

from fixtures.make_image import _write_manifest
from forensictl.verify import verify_image


def test_three_tampered_blocks_are_mapped(image_path: Path, manifest_path: Path) -> None:
    result = verify_image(image_path, manifest_path)
    assert result.exit_code == 1
    assert len(result.mismatches) == 3
    assert {item.asset for item in result.mismatches} == {
        "FILE_RECORDS",
        "NTP_LOG",
        "MAINTENANCE",
    }
    assert all(item.offset == item.block * 4 * 1024 * 1024 for item in result.mismatches)
    assert not result.chain_breaks


def test_clean_manifest_is_exit_zero(
    image_path: Path, tmp_path: Path
) -> None:
    manifest = tmp_path / "clean.jsonl"
    _write_manifest(image_path, manifest, 16)
    result = verify_image(image_path, manifest)
    assert result.exit_code == 0
    assert len(result.mismatches) == 0


def test_manifest_block_count_mismatch_does_not_crash(
    image_path: Path, manifest_path: Path, tmp_path: Path
) -> None:
    lines = manifest_path.read_text(encoding="utf-8").splitlines()
    manifest = tmp_path / "short-manifest.jsonl"
    manifest.write_text("\n".join(lines[:-1]) + "\n", encoding="utf-8")
    result = verify_image(image_path, manifest)
    assert result.exit_code == 1
    assert any("manifest block count mismatch" in text for text in result.structural_errors)


def test_chain_break_is_reported(image_path: Path, manifest_path: Path, tmp_path: Path) -> None:
    entries = [json.loads(line) for line in manifest_path.read_text(encoding="utf-8").splitlines()]
    entries[4]["prev_hash"] = "0" * 64
    manifest = tmp_path / "broken-chain.jsonl"
    manifest.write_text(
        "\n".join(json.dumps(item, sort_keys=True) for item in entries) + "\n",
        encoding="utf-8",
    )
    result = verify_image(image_path, manifest)
    assert result.exit_code == 1
    assert result.chain_breaks == [4]
    assert any("hash chain break at block 4" in text for text in result.structural_errors)


def test_truncated_final_block_is_reported(
    image_path: Path, manifest_path: Path, tmp_path: Path
) -> None:
    truncated = tmp_path / "truncated.img"
    shutil.copyfile(image_path, truncated)
    with truncated.open("r+b") as handle:
        handle.truncate(image_path.stat().st_size - 1024 * 1024)
    result = verify_image(truncated, manifest_path)
    assert result.exit_code == 1
    assert any("short block" in text for text in result.structural_errors)
    assert any(item.reason == "short block" for item in result.mismatches)


def test_missing_image_is_input_error(tmp_path: Path) -> None:
    completed = subprocess.run(
        [
            sys.executable,
            "-m",
            "forensictl.cli",
            "verify",
            "--image",
            str(tmp_path / "absent.img"),
            "--manifest",
            str(tmp_path / "absent.jsonl"),
        ],
        cwd=Path(__file__).resolve().parents[1],
        capture_output=True,
        text=True,
        check=False,
    )
    assert completed.returncode == 2
    assert "error:" in completed.stderr
