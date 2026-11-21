"""Shared test fixtures: temporary directories and a tiny generated corpus."""

from __future__ import annotations

import os
import tempfile
import unittest
from typing import Any

from avtimeline.config import ProjectConfig
from avtimeline.errors import Stage
from avtimeline.pipeline import Pipeline, PipelineResult
from avtimeline.samples import SampleSet, generate_samples


class TempDirMixin(unittest.TestCase):
    """Creates a per-test temporary directory that is removed afterwards."""

    _dir: str

    def setUp(self) -> None:  # noqa: D401 - unittest hook
        super().setUp()
        self._dir = tempfile.mkdtemp(prefix="avtimeline-test-")

    def tearDown(self) -> None:
        import shutil

        shutil.rmtree(self._dir, ignore_errors=True)
        super().tearDown()

    def path(self, *parts: str) -> str:
        return os.path.join(self._dir, *parts)


def make_config(**overrides: Any) -> ProjectConfig:
    payload: dict[str, Any] = {"window": {"start": 0, "end": "101/50"}, "policy": "skip"}
    payload.update(overrides)
    return ProjectConfig.from_mapping(payload)


def run_pipeline(
    workdir: str,
    config: ProjectConfig | None = None,
    **kwargs: Any,
) -> PipelineResult:
    return Pipeline(workdir, config or make_config(), **kwargs).run()


def samples(directory: str, force: bool = True) -> SampleSet:
    return generate_samples(directory, force=force)


__all__ = [
    "Stage",
    "TempDirMixin",
    "make_config",
    "run_pipeline",
    "samples",
]
