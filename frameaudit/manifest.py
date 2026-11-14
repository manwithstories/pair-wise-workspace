"""JSONL manifest input and output."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

from .timeline import TimelineFrame


REQUIRED_FIELDS = {
    "display_index",
    "decode_index",
    "pict_type",
    "keyframe",
    "source_pts",
    "repaired_pts",
    "action",
}


def write_manifest(path: str | Path, timeline: list[TimelineFrame]) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.with_name(destination.name + ".tmp")
    with temporary.open("w", encoding="utf-8", newline="\n") as handle:
        for frame in timeline:
            handle.write(json.dumps(frame.to_dict(), ensure_ascii=False, separators=(",", ":")))
            handle.write("\n")
    os.replace(temporary, destination)


def read_manifest(path: str | Path) -> list[dict[str, Any]]:
    source = Path(path)
    try:
        lines = source.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError) as exc:
        raise ValueError(f"cannot read manifest: {source}: {exc}") from exc
    rows: list[dict[str, Any]] = []
    for line_number, line in enumerate(lines, start=1):
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError as exc:
            raise ValueError(f"invalid manifest JSON at line {line_number}: {exc}") from exc
        if not isinstance(row, dict) or not REQUIRED_FIELDS.issubset(row):
            raise ValueError(f"manifest line {line_number} is missing required fields")
        rows.append(row)
    return rows
