from __future__ import annotations

import importlib.util
from pathlib import Path

import pytest


ROOT = Path(__file__).resolve().parents[1]


def _fixture_builder():
    path = ROOT / "scripts" / "gen_fixtures.py"
    spec = importlib.util.spec_from_file_location("cutplan_fixture_builder", path)
    if spec is None or spec.loader is None:
        raise RuntimeError("cannot load fixture builder")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.build_conflict_fixture


@pytest.fixture
def conflict_timeline():
    return _fixture_builder()(20261114)
