"""Lazy access to the deterministic sample synthesizer in ``tests/``.

The synthesizer ships as test fixture data (``tests/synthesize.py``) rather
than as runtime library code, but the ``demo`` command must drive it from an
installed wheel/editable install. This shim locates that file relative to the
repository and imports it by path, so no package has to wrap the tests tree.
"""

from __future__ import annotations

import importlib.util
import os
import sys
import types

_CACHE: types.ModuleType | None = None


def _candidate_paths() -> list[str]:
    here = os.path.dirname(os.path.abspath(__file__))
    roots = [
        os.path.dirname(here),              # src/fieldcut -> repo root
        os.getcwd(),
    ]
    return [os.path.join(r, "tests", "synthesize.py") for r in roots]


def _load() -> types.ModuleType:
    global _CACHE
    if _CACHE is not None:
        return _CACHE
    for path in _candidate_paths():
        if os.path.isfile(path):
            spec = importlib.util.spec_from_file_location(
                "fieldcut_synth_fixture", path)
            module = importlib.util.module_from_spec(spec)
            sys.modules["fieldcut_synth_fixture"] = module
            spec.loader.exec_module(module)  # type: ignore[union-attr]
            _CACHE = module
            return module
    raise FileNotFoundError(
        "tests/synthesize.py not found; the demo synthesizer is required")


def __getattr__(name: str):
    return getattr(_load(), name)
