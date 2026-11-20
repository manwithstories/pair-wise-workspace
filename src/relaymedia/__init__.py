"""relaymedia: local, resumable segment-relay tooling for edge media nodes.

The package is import-safe: importing it never opens a file, starts a thread or
touches the network. Every public entry point accepts plain data so other
Python programs can embed individual stages instead of driving the CLI.
"""

from __future__ import annotations

__version__ = "0.1.0"

__all__ = ["__version__"]
