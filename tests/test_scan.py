"""End-to-end acceptance tests for the local scanner."""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

from gen_fixtures import generate


REPO_ROOT = Path(__file__).resolve().parents[1]


def _run(command: list[str], cwd: Path) -> subprocess.CompletedProcess[str]:
    env = dict(os.environ)
    local_path = os.path.join(str(REPO_ROOT), "src")
    existing = env.get("PYTHONPATH", "")
    env["PYTHONPATH"] = os.pathsep.join(part for part in (local_path, existing) if part)
    return subprocess.run(
        command,
        cwd=cwd,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=60,
        check=False,
    )


@pytest.fixture()
def workspace(tmp_path: Path) -> Path:
    generate(tmp_path)
    return tmp_path


def _scan(
    workspace: Path, report: Path, cache: Path, jobs: int = 4
) -> subprocess.CompletedProcess[str]:
    return _run(
        [
            sys.executable,
            "-m",
            "taint",
            "scan",
            "--root",
            str(workspace / "fixtures" / "vuln"),
            "--policy",
            str(workspace / "policy.yaml"),
            "--report",
            str(report),
            "--stats",
            "--cache-dir",
            str(cache),
            "--jobs",
            str(jobs),
        ],
        workspace,
    )


def _scan_tree(
    workspace: Path,
    root: Path,
    cache: Path,
    *extra: str,
) -> subprocess.CompletedProcess[str]:
    return _run(
        [
            sys.executable,
            "-m",
            "taint",
            "scan",
            "--root",
            str(root),
            "--policy",
            str(workspace / "policy.yaml"),
            "--stats",
            "--cache-dir",
            str(cache),
            *extra,
        ],
        workspace,
    )


def test_scan_counts_paths_and_cache_consistency(workspace: Path) -> None:
    report = workspace / "out" / "report.json"
    cache = workspace / ".cache"
    first = _scan(workspace, report, cache)
    assert first.returncode == 0, first.stderr
    lines = first.stdout.splitlines()
    assert lines[0] == "files=14 parsed=14 cache_hit=0 diagnostics=1"
    assert lines[1] == "sources=6 sinks=5 paths=8 truncated=false"
    assert len(lines) == 10
    assert all(
        line.startswith(f"P{index} ") for index, line in enumerate(lines[2:], start=1)
    )
    assert any("-" + ">" + "-" in line or "->" in line for line in lines[2:])
    first_report = report.read_bytes()

    second = _scan(workspace, report, cache)
    assert second.returncode == 0, second.stderr
    second_lines = second.stdout.splitlines()
    assert second_lines[0] == "files=14 parsed=0 cache_hit=14 diagnostics=1"
    assert second_lines[1:] == lines[1:]
    assert report.read_bytes() == first_report

    payload = json.loads(report.read_text(encoding="utf-8"))
    assert payload["summary"]["sources"] == 6
    assert payload["summary"]["sinks"] == 5
    assert payload["summary"]["paths"] == 8
    assert payload["summary"]["truncated"] is False
    assert len(payload["paths"]) == 8
    assert all(
        not Path(entry["source"]["file"]).is_absolute() for entry in payload["paths"]
    )
    assert all(
        not Path(entry["sink"]["file"]).is_absolute() for entry in payload["paths"]
    )


def test_changed_file_invalidates_only_its_cache_entry(workspace: Path) -> None:
    report = workspace / "out" / "report.json"
    cache = workspace / ".cache"
    first = _scan(workspace, report, cache)
    assert first.returncode == 0
    target = workspace / "fixtures" / "vuln" / "app.py"
    target.write_text(
        target.read_text(encoding="utf-8") + "\n# changed\n", encoding="utf-8"
    )
    second = _scan(workspace, report, cache)
    assert second.returncode == 0, second.stderr
    assert (
        second.stdout.splitlines()[0] == "files=14 parsed=1 cache_hit=13 diagnostics=1"
    )


def test_sanitizer_truncates_candidate_paths(workspace: Path) -> None:
    root = workspace / "fixtures" / "sanitized"
    root.mkdir()
    (root / "safe_case.py").write_text(
        "import os\n\ndef safe_flow(request):\n    value = request.args.get('x')\n    os.system(int(value))\n",
        encoding="utf-8",
    )
    assert generate(workspace).parent == workspace / "fixtures"
    policy = workspace / "policy.yaml"
    report = workspace / "out" / "sanitized.json"
    result = _run(
        [
            sys.executable,
            "-m",
            "taint",
            "scan",
            "--root",
            str(root),
            "--policy",
            str(policy),
            "--report",
            str(report),
            "--stats",
            "--cache-dir",
            str(workspace / ".cache-sanitized"),
        ],
        workspace,
    )
    assert result.returncode == 0, result.stderr
    payload = json.loads(report.read_text(encoding="utf-8"))
    assert payload["summary"]["paths"] == 0
    assert payload["summary"]["sanitized_candidates"] == 1


def test_unresolved_symbol_is_diagnostic_not_fatal(workspace: Path) -> None:
    root = workspace / "fixtures" / "unresolved"
    root.mkdir()
    (root / "helpers.py").write_text("def known():\n    return 1\n", encoding="utf-8")
    (root / "consumer.py").write_text(
        "from helpers import missing\n\ndef use():\n    return missing()\n",
        encoding="utf-8",
    )
    result = _run(
        [
            sys.executable,
            "-m",
            "taint",
            "scan",
            "--root",
            str(root),
            "--policy",
            str(workspace / "policy.yaml"),
            "--stats",
            "--cache-dir",
            str(workspace / ".cache-unresolved"),
        ],
        workspace,
    )
    assert result.returncode == 0, result.stderr
    assert "diagnostics=" in result.stdout
    assert int(result.stdout.split("diagnostics=")[1].split()[0]) >= 1


def test_policy_error_returns_2(workspace: Path) -> None:
    result = _run(
        [
            sys.executable,
            "-m",
            "taint",
            "scan",
            "--root",
            str(workspace / "fixtures" / "vuln"),
            "--policy",
            str(workspace / "missing.yaml"),
        ],
        workspace,
    )
    assert result.returncode == 2


def test_report_subcommand_renders_same_paths(workspace: Path) -> None:
    report = workspace / "out" / "report.json"
    scan = _scan(workspace, report, workspace / ".cache-report")
    assert scan.returncode == 0
    rendered = _run(
        [sys.executable, "-m", "taint", "report", "--input", str(report), "--stats"],
        workspace,
    )
    assert rendered.returncode == 0, rendered.stderr
    assert rendered.stdout.splitlines()[2:] == scan.stdout.splitlines()[2:]


def test_alias_chains_tuple_swap_and_class_fields(workspace: Path) -> None:
    root = workspace / "fixtures" / "aliases"
    root.mkdir()
    (root / "util.py").write_text(
        "import os\n\ndef sinkwrap(value):\n    return os.system(value)\n",
        encoding="utf-8",
    )
    (root / "app.py").write_text(
        """import os
from util import sinkwrap as s

alias = s
f2 = alias

def chain(query):
    f2(query)

def swap(query):
    left = query
    right = "safe"
    left, right = right, left
    os.system(right)

class Holder:
    def __init__(self, query):
        self.data = query
    def run(self):
        os.system(self.data)
""",
        encoding="utf-8",
    )
    result = _scan_tree(workspace, root, workspace / ".cache-alias")
    assert result.returncode == 0, result.stderr
    assert "paths=3" in result.stdout
    assert any("chain->sinkwrap" in line for line in result.stdout.splitlines())
    assert any(
        "Holder.__init__->Holder.run" in line for line in result.stdout.splitlines()
    )


def test_recursive_summary_is_finite_and_path_budget_truncates(workspace: Path) -> None:
    root = workspace / "fixtures" / "recursive"
    root.mkdir()
    (root / "rec.py").write_text(
        """import os

def outer(query):
    return inner(query)

def inner(query):
    os.system(query)
    return outer(query)
""",
        encoding="utf-8",
    )
    result = _scan_tree(
        workspace,
        root,
        workspace / ".cache-recursive",
        "--max-paths",
        "1",
    )
    assert result.returncode == 0, result.stderr
    assert "paths=1 truncated=true" in result.stdout
    assert len(result.stdout.splitlines()) == 3


def test_empty_root_returns_3(workspace: Path) -> None:
    empty = workspace / "fixtures" / "empty"
    empty.mkdir()
    result = _scan_tree(workspace, empty, workspace / ".cache-empty")
    assert result.returncode == 3
