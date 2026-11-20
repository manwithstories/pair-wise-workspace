"""mediarc - long-term, verifiable archiving for production masters.

The package deliberately keeps every transformation local and every diagnostic
addressable: each failure carries the source path, stream index, pipeline stage
and, where libavformat exposes it, the source-file byte offset.
"""

__version__ = "0.1.0"
__all__ = ["__version__"]
