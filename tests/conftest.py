"""Shared test fixtures: a temporary project tree with generated WAVs."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

# Make both `audio_tool` (via the repo shim) and `tests` importable.
ROOT = Path(__file__).resolve().parent.parent
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from tests import generate_fixtures  # noqa: E402


@pytest.fixture(scope="session")
def fixture_tree(tmp_path_factory) -> dict[str, Path]:
    """Generate the full fixture set once per session into a temp dir.

    Valid and deliberately-broken inputs land in sibling directories, the
    same split the acceptance demo relies on.
    """

    root = tmp_path_factory.mktemp("fixtures")
    return generate_fixtures.generate_all(root / "good", root / "broken")


@pytest.fixture()
def good_inputs(fixture_tree) -> list[Path]:
    """Fixtures expected to process successfully."""

    return [fixture_tree[name] for name in
            ("tone_44100_stereo", "tone_48000_51", "tone_96000_quad",
             "mono_odd_tail", "quiet_music")]
