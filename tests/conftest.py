from __future__ import annotations

import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))


@pytest.fixture(scope="session")
def fixture_dir(tmp_path_factory: pytest.TempPathFactory) -> Path:
    output = tmp_path_factory.mktemp("fixtures")
    subprocess.run(
        [sys.executable, str(ROOT / "scripts" / "make_fixtures.py"),
         "--out", str(output), "--count", "24", "--seed", "7"],
        check=True,
        cwd=ROOT,
        stdout=subprocess.DEVNULL,
    )
    return output
