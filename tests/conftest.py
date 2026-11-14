from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest

from mixer.cli import main
from tools.make_fixtures import make_fixtures


@pytest.fixture(scope="session")
def show_root(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("show")
    make_fixtures(root, [44100, 48000, 96000], 4.0, 20261115)
    return root


@pytest.fixture
def show_manifest_data(show_root: Path) -> dict:
    return json.loads((show_root / "mix.json").read_text(encoding="utf-8"))


@pytest.fixture
def write_manifest(show_root: Path):
    def _write(data: dict, name: str = "case.json") -> str:
        path = show_root / name
        path.write_text(json.dumps(data), encoding="utf-8")
        return str(path)

    return _write


@pytest.fixture
def invoke(capsys: pytest.CaptureFixture[str]):
    def _invoke(*args: str) -> tuple[int, dict]:
        code = main(list(args))
        captured = capsys.readouterr()
        lines = captured.out.strip().splitlines()
        assert len(lines) == 1, captured.out
        return code, json.loads(lines[0])

    return _invoke


@pytest.fixture
def manifest_copy(show_manifest_data: dict):
    return lambda: copy.deepcopy(show_manifest_data)
