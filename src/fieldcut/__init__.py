"""fieldcut: align multi-camera field recordings and cut multi-bitrate segments.

The shared media time base, error model and pipeline stages live in this
package. All timeline math is expressed in integer 90 kHz ticks (or exact
``Fraction`` values) until the final reporting step, so floating point drift
can never move a frame boundary.
"""

from __future__ import annotations

__version__ = "0.1.0"

# Canonical tick rate shared by every timeline (MPEG-compatible 90 kHz clock).
TICKS_PER_SECOND = 90_000
TICKS_PER_MS = TICKS_PER_SECOND // 1_000
