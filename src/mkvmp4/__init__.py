"""Matroska to faststart MP4 conversion package."""

from .mkv.parser import parse_digest

__version__ = "0.1.0"

__all__ = ["__version__", "parse_digest"]

