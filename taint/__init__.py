"""Compatibility launcher for running ``python -m taint`` from the repo root."""

from pathlib import Path

__path__ = [str(Path(__file__).resolve().parent.parent / "src" / "taint")]
