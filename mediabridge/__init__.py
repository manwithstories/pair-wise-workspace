"""Import shim so ``python -m mediabridge`` works from a fresh checkout.

The real package lives in ``src/mediabridge`` (see ``pyproject.toml``).  The
acceptance workflow installs third-party wheels only, never the project
itself, so this top-level package extends its search path to ``src`` and
re-exports the real modules from there.
"""

import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
_SRC = os.path.join(os.path.dirname(_HERE), "src", "mediabridge")
if _SRC not in sys.path:
    sys.path.insert(0, os.path.join(os.path.dirname(_HERE), "src"))

# Search this directory first (so ``__main__`` lives next to this file) and
# then the real source tree.
__path__ = [_HERE, _SRC]
