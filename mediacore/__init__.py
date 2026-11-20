"""mediacore - deterministic, resumable timeline processing for mobile clips.

The real package lives under ``src/mediacore`` (standard src layout).  This
top-level module is only a path shim so that ``python -m mediacore.cli`` works
from a fresh checkout without ``pip install``; it prepends the src package
directory to this package's search path.
"""

from pathlib import Path as _Path

_SRC_PACKAGE = _Path(__file__).resolve().parent.parent / "src" / "mediacore"
if not _SRC_PACKAGE.is_dir():  # pragma: no cover - defensive
    raise ImportError(f"mediacore src package not found at {_SRC_PACKAGE}")
__path__.insert(0, str(_SRC_PACKAGE))

__version__ = "0.1.0"
