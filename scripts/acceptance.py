#!/usr/bin/env python3
"""Acceptance driver: run the three documented commands and assert the contract.

Usage:
    python scripts/acceptance.py [--work-dir DIR]

Exit codes mirror the CLI: 0 when every assertion holds, 1 when a check fails.
The script writes into a temporary directory by default so it never touches the
repository's tracked files.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import tempfile
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent


class Checker:
    def __init__(self) -> None:
        self.failures: list[str] = []
        self.checks = 0

    def check(self, label: str, condition: bool, detail: str = "") -> None:
        self.checks += 1
        if condition:
            print(f"  ok   {label}")
        else:
            self.failures.append(f"{label}: {detail}")
            print(f"  FAIL {label} {detail}")

    def between(self, label: str, value: float, low: float, high: float) -> None:
        self.check(label, low <= value <= high, f"{value} not in [{low}, {high}]")


def run(command: list[str], *, stdin: bytes | None = None) -> tuple[int, dict]:
    result = subprocess.run(
        command,
        cwd=REPO_ROOT,
        input=stdin,
        capture_output=True,
    )
    try:
        payload = json.loads(result.stdout.decode() or "{}")
    except json.JSONDecodeError:
        payload = {}
    return result.returncode, payload


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work-dir", help="directory for generated fixtures and output")
    args = parser.parse_args()

    work = Path(args.work_dir) if args.work_dir else Path(tempfile.mkdtemp(prefix="mkv2mp4-acc-"))
    work.mkdir(parents=True, exist_ok=True)
    fixture = work / "fixture.mkv"
    output = work / "sample.mp4"
    py = sys.executable
    checker = Checker()

    print("inspect --generate-fixture --json")
    code, envelope = run(
        [py, "-m", "src.cli", "inspect", "--generate-fixture", "--json", "--fixture-path", str(fixture)]
    )
    checker.check("exit 0", code == 0, f"got {code}")
    data = envelope.get("data") or {}
    summary = data.get("summary", {})
    checker.between("element_count", summary.get("element_count", 0), 30, 80)
    checker.check("unknown_length_count >= 2", summary.get("unknown_length_count", 0) >= 2)
    checker.between("track_count", summary.get("track_count", 0), 2, 3)
    checker.between("sample_count", summary.get("sample_count", 0), 10, 40)
    plan = data.get("timestamp_plan") or {}
    checker.check("timestamp plan present", bool(plan.get("tracks")))
    for track in plan.get("tracks", []):
        checker.check(
            f"track {track['track_number']} first_pts >= 0",
            track["rewritten_first_pts"] >= 0,
            str(track["rewritten_first_pts"]),
        )
        checker.check(
            f"track {track['track_number']} pts increasing",
            track["rewritten_last_pts"] > track["rewritten_first_pts"],
        )
    checker.check("track_mapping present", bool(data.get("track_mapping")))

    print("remux --generate-fixture --output <mp4> --json")
    code, envelope = run(
        [
            py, "-m", "src.cli", "remux",
            "--generate-fixture", "--fixture-path", str(fixture),
            "--output", str(output), "--json",
        ]
    )
    checker.check("exit 0", code == 0, f"got {code}")
    data = envelope.get("data") or {}
    checker.check("output exists", output.is_file())
    checker.check("sha256 is 64 hex", len(data.get("sha256", "")) == 64)
    checker.check("box_count > 20", data.get("box_count", 0) > 20)
    checker.check("sample_count in range", 10 <= sum(
        entry["sample_count"] for entry in data.get("indexes", [])
    ) <= 40)
    for entry in (data.get("timestamps") or {}).get("tracks", []):
        checker.check(
            f"rewritten_first_pts >= 0 (track {entry['track_number']})",
            entry["rewritten_first_pts"] >= 0,
        )

    print("verify --input <mp4> --json")
    code, envelope = run([py, "-m", "src.cli", "verify", "--input", str(output), "--json"])
    checker.check("exit 0", code == 0, f"got {code}")
    data = envelope.get("data") or {}
    checker.check("verify ok", data.get("ok") is True)
    checker.check("zero failures", data.get("failure_count") == 0)
    checker.check("tracks reported", len(data.get("tracks", [])) >= 2)

    print(f"\n{checker.checks - len(checker.failures)}/{checker.checks} checks passed")
    if checker.failures:
        print("failures:")
        for failure in checker.failures:
            print(f"  - {failure}")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
