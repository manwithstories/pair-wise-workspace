from __future__ import annotations

import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from mediaprobe.samplegen import (  # noqa: E402,F401
    TrackSpec,
    box,
    deterministic_bytes,
    fullbox,
    sample_recipes,
)
