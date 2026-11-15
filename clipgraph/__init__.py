"""Source-tree launcher so ``python -m clipgraph`` works without installation."""

from pathlib import Path

__path__.append(str(Path(__file__).resolve().parent.parent / "src" / "clipgraph"))
