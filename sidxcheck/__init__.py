"""Development-tree shim for running ``python -m sidxcheck`` from the repo root."""

from pathlib import Path

__version__ = "0.1.0"
_SOURCE_PACKAGE = Path(__file__).resolve().parent.parent / "src" / "sidxcheck"
if _SOURCE_PACKAGE.is_dir():
    __path__ = [str(_SOURCE_PACKAGE)]
