"""JSON manifest with atomic replacement and validation.

The manifest is the hand-off artifact for transcode and retrieval stages.
Writes are atomic (temp file + os.replace) so a failed run never leaves a
half-written manifest, and a previous complete manifest survives a failed
re-run untouched.
"""

from __future__ import annotations

import json
import os
import re
import tempfile
from pathlib import Path

from .errors import UnrecoverableError

MANIFEST_VERSION = 1
_SAFE_NAME = re.compile(r"^[A-Za-z0-9._-]+$")


def safe_segment_name(name: str) -> str:
    """Reject anything that could escape the output directory."""
    if not _SAFE_NAME.match(name) or ".." in name:
        raise UnrecoverableError(f"unsafe segment file name: {name!r}")
    return name


def atomic_write_json(path: str | Path, payload: dict) -> None:
    """Write JSON atomically: same-directory temp file then replace."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=".manifest-",
                               suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(payload, fh, indent=2, sort_keys=True)
            fh.write("\n")
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def load_manifest(path: str | Path) -> dict:
    path = Path(path)
    try:
        with path.open("r", encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        raise UnrecoverableError(f"cannot read manifest {path}: {exc}") from exc
    if not isinstance(data, dict) or "segments" not in data:
        raise UnrecoverableError("manifest missing required 'segments' key")
    return data


def validate_manifest(data: dict, base_dir: str | Path) -> list[str]:
    """Return a list of problems; empty list means the manifest is valid."""
    problems: list[str] = []
    base = Path(base_dir).resolve()
    segments = data.get("segments")
    if not isinstance(segments, list) or not segments:
        problems.append("no segments recorded")
        return problems
    for seg in segments:
        name = seg.get("file", "")
        try:
            safe_segment_name(name)
        except UnrecoverableError as exc:
            problems.append(str(exc))
            continue
        resolved = (base / name).resolve()
        if not str(resolved).startswith(str(base) + os.sep):
            problems.append(f"segment path escapes output dir: {name}")
        elif not resolved.exists():
            problems.append(f"segment file missing: {name}")
        if seg.get("duration_ms", 0) <= 0:
            problems.append(f"segment {name} has non-positive duration")
    reps = data.get("representations")
    if not isinstance(reps, list) or not reps:
        problems.append("no representations recorded")
    if data.get("duration_ms", 0) <= 0:
        problems.append("non-positive total duration")
    return problems
