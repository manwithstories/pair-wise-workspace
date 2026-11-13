from __future__ import annotations

from pathlib import Path

import pytest

from fixtures.make_image import generate


@pytest.fixture(scope="session")
def evidence_dir(tmp_path_factory: pytest.TempPathFactory) -> Path:
    output = tmp_path_factory.mktemp("evidence")
    generate(output, seed=20260308, total_blocks=16)
    return output


@pytest.fixture(scope="session")
def image_path(evidence_dir: Path) -> Path:
    return evidence_dir / "evidence.img"


@pytest.fixture(scope="session")
def manifest_path(evidence_dir: Path) -> Path:
    return evidence_dir / "manifest.jsonl"


@pytest.fixture(scope="session")
def ground_truth(evidence_dir: Path) -> dict:
    import json

    return json.loads((evidence_dir / "ground_truth.json").read_text(encoding="utf-8"))
