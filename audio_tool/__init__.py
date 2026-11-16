"""Source-layout shim for ``python -m audio_tool.cli``.

The implementation lives in ``src/audio_tool`` (see pyproject.toml).  Python's
import system honours ``__path__``, so pointing it at the real package keeps
both the documented layout and the acceptance command working without an
editable install.
"""

from pathlib import Path

_SRC = Path(__file__).resolve().parent.parent / "src" / "audio_tool"
if _SRC.is_dir():
    __path__ = [str(_SRC)]

__version__ = "1.0.0"
