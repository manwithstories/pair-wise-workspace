"""Filesystem helpers that keep every path inside the workspace."""

from __future__ import annotations

import glob as _glob
from pathlib import Path

from .errors import ParamError


def workspace_root() -> Path:
    return Path.cwd().resolve()


def safe_resolve(raw: str | Path) -> Path:
    """Resolve a path and reject anything escaping the current workspace."""
    path = Path(raw).expanduser().resolve()
    root = workspace_root()
    if path != root and root not in path.parents:
        raise ParamError(f"path outside workspace: {raw}")
    return path


def expand_inputs(patterns: list[str]) -> list[Path]:
    """Expand glob patterns (plain paths stay valid), de-duplicated and sorted."""
    seen: set[Path] = set()
    for pattern in patterns:
        matches = _glob.glob(str(Path(pattern).expanduser())) or [pattern]
        for item in matches:
            path = safe_resolve(item)
            if path.is_file() and path not in seen:
                seen.add(path)
    return sorted(seen)
