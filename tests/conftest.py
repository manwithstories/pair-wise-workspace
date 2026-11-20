"""Shared fixtures: tiny deterministic streams, generated once per session."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from mediacore.fixtures import FixtureSpec, make_fixtures  # noqa: E402


@pytest.fixture(scope="session")
def tiny_dir(tmp_path_factory) -> dict:
    """A 2-second H.264 stream plus its WAV, generated once per test session."""
    out = tmp_path_factory.mktemp("tiny")
    spec = FixtureSpec(codec="h264", duration_s=2, bitrate=600_000, seed=7)
    return make_fixtures(out, spec)


@pytest.fixture(scope="session")
def tiny_bytes(tiny_dir) -> bytes:
    return Path(tiny_dir["video_path"]).read_bytes()


@pytest.fixture(scope="session")
def tiny_units(tiny_bytes):
    from mediacore.bitstream import chunk_bytes, scan

    units = []
    for item in scan(chunk_bytes(tiny_bytes, 4096), codec="h264"):
        if not hasattr(item, "units"):
            units.append(item)
    return units
