"""Shared pytest fixtures: one generated fixture set per session."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from mediacore.testdata import generate_fixtures  # noqa: E402


@pytest.fixture(scope="session")
def fixtures(tmp_path_factory):
    """Generate the full fixture family once per test session."""
    root = tmp_path_factory.mktemp("fixtures")
    return generate_fixtures(root)


@pytest.fixture()
def workdir(tmp_path):
    path = tmp_path / "work"
    path.mkdir()
    return path
