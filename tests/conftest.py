from __future__ import annotations

from pathlib import Path

import pytest

from framefix.synth import synth_many


@pytest.fixture(scope="session")
def fixture_dir(tmp_path_factory: pytest.TempPathFactory) -> Path:
    target = tmp_path_factory.mktemp("fixtures")
    synth_many(str(target), ["clean", "bframes", "broken"], frames=72, width=160, height=90)
    return target


@pytest.fixture(scope="session")
def clean_path(fixture_dir: Path) -> Path:
    return fixture_dir / "clean.mp4"


@pytest.fixture(scope="session")
def bframes_path(fixture_dir: Path) -> Path:
    return fixture_dir / "bframes.mp4"


@pytest.fixture(scope="session")
def broken_path(fixture_dir: Path) -> Path:
    return fixture_dir / "broken.mp4"

