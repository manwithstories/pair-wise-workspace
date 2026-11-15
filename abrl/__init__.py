"""Path shim so `python -m abrl.cli` works without an editable install.

The real package lives under ``src/abrl`` (see pyproject.toml). Redirecting
``__path__`` makes Python resolve every submodule from the src tree while a
normal editable/regular install still takes precedence when present.
"""
from pathlib import Path

__version__ = "1.0.0"
_src_pkg = Path(__file__).resolve().parent.parent / "src" / "abrl"
if _src_pkg.is_dir():
    __path__ = [str(_src_pkg)]
