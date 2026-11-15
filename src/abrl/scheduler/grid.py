"""Parameter grid expansion, task-key hashing and conflict detection."""
from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, asdict
from pathlib import Path

from .. import __version__

ENCODER_VERSION = f"abrl-{__version__}"


class GridConflictError(ValueError):
    """Raised when the same task key maps to differing parameters."""


@dataclass(frozen=True)
class TaskSpec:
    key: str
    source: str
    source_digest: str
    width: int
    height: int
    fps: int
    qp: int
    gop: int
    encoder_version: str

    def as_dict(self) -> dict:
        return asdict(self)


def make_key(source_digest: str, width: int, height: int, fps: int, qp: int, gop: int) -> str:
    payload = "|".join(
        [source_digest, f"{width}x{height}", str(fps), f"qp{qp}", f"gop{gop}", ENCODER_VERSION]
    )
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()[:16]


def _as_list(value, name: str) -> list:
    if isinstance(value, list) and value:
        return value
    raise GridConflictError(f"grid field '{name}' must be a non-empty list")


def load_grid(path: str | Path) -> dict:
    with open(path, "r", encoding="utf-8") as fh:
        grid = json.load(fh)
    required = ("resolutions", "fps", "qp", "gop")
    missing = [name for name in required if name not in grid]
    if missing:
        raise GridConflictError(f"grid missing fields: {', '.join(missing)}")
    return grid


def expand(grid: dict, source: str | Path, source_digest: str) -> tuple[list[TaskSpec], int]:
    """Expand a grid dict into deduplicated tasks; return (tasks, duplicates)."""
    source = str(source)
    tasks: dict[str, TaskSpec] = {}
    duplicates = 0
    fps_values = _as_list(grid["fps"], "fps")
    qp_values = _as_list(grid["qp"], "qp")
    gop_values = _as_list(grid["gop"], "gop")
    for resolution in _as_list(grid["resolutions"], "resolutions"):
        width, height = (int(part) for part in str(resolution).lower().split("x", 1))
        for fps in (int(v) for v in fps_values):
            for qp in (int(v) for v in qp_values):
                for gop in (int(v) for v in gop_values):
                    key = make_key(source_digest, width, height, fps, qp, gop)
                    spec = TaskSpec(key, source, source_digest, width, height, fps, qp, gop, ENCODER_VERSION)
                    if key in tasks:
                        if tasks[key] != spec:
                            raise GridConflictError(f"key collision with differing params: {key}")
                        duplicates += 1
                        continue
                    tasks[key] = spec
    return list(tasks.values()), duplicates


def detect_conflicts(existing: dict[str, dict], specs: list[TaskSpec]) -> None:
    """Reject specs whose key already exists in the DB with different params."""
    for spec in specs:
        prior = existing.get(spec.key)
        if prior is None:
            continue
        for field in ("source_digest", "width", "height", "fps", "qp", "gop", "encoder_version"):
            if prior.get(field) != getattr(spec, field):
                raise GridConflictError(
                    f"task {spec.key} exists with different {field}: "
                    f"{prior.get(field)!r} vs {getattr(spec, field)!r}"
                )
