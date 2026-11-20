"""relay_timeline: unified multi-track timeline reconstruction.

Data flow: manifest parse -> clock normalization -> decode -> track
composition -> offscreen export -> statistics.  Manifest, decode, audio
and compose modules share the clock results and the unified error types
defined in :mod:`relay_timeline.errors`.
"""

__all__ = ["__version__"]
__version__ = "0.1.0"
