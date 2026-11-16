"""Shared test helpers: fixture builders and a temp workspace."""

from __future__ import annotations

import os
import sys
import tempfile
import unittest

import numpy as np

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRC_ROOT = os.path.join(REPO_ROOT, "src")
if SRC_ROOT not in sys.path:
    sys.path.insert(0, SRC_ROOT)

from audio_segmenter.fixtures import (  # noqa: E402
    generate_fixtures,
    write_float_wav,
    write_truncated_wav,
    write_wav,
)
from audio_segmenter.model import DetectionConfig  # noqa: E402


def tone(sr: int, seconds: float, freq: float = 440.0, amp: float = 0.4) -> np.ndarray:
    n = int(round(sr * seconds))
    t = np.arange(n, dtype=np.float64) / sr
    return (amp * np.sin(2 * np.pi * freq * t)).astype(np.float32)


def noise(sr: int, seconds: float, amp: float = 0.001, seed: int = 0) -> np.ndarray:
    rng = np.random.default_rng(seed)
    n = int(round(sr * seconds))
    return (rng.standard_normal(n) * amp).astype(np.float32)


def silence(sr: int, seconds: float) -> np.ndarray:
    return np.zeros(int(round(sr * seconds)), dtype=np.float32)


def fade(values: np.ndarray, sr: int, fade_in_s: float, fade_out_s: float) -> np.ndarray:
    out = values.astype(np.float32, copy=True)
    n_in = min(out.size, int(round(sr * fade_in_s)))
    n_out = min(out.size, int(round(sr * fade_out_s)))
    if n_in:
        ramp = np.linspace(0.0, 1.0, n_in, dtype=np.float32)
        out[:n_in] *= ramp
    if n_out:
        ramp = np.linspace(1.0, 0.0, n_out, dtype=np.float32)
        out[-n_out:] *= ramp
    return out


class TempWorkspace(unittest.TestCase):
    """Base class giving each test an isolated directory."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory(prefix="segtest-")
        self.root = self._tmp.name

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def path(self, name: str) -> str:
        return os.path.join(self.root, name)

    def default_config(self) -> DetectionConfig:
        return DetectionConfig()


__all__ = [
    "DetectionConfig",
    "TempWorkspace",
    "fade",
    "generate_fixtures",
    "noise",
    "silence",
    "tone",
    "write_float_wav",
    "write_truncated_wav",
    "write_wav",
]
