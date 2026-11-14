from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .errors import CutplanError
from .fmt_v1 import dumps_v1, parse_v1, read_v1
from .fmt_v2 import SCHEMA, dumps_v2, parse_v2, read_v2
from .model import Timeline


SUPPORTED_VERSIONS = ("v1", "v2")


def _version_error(found: object) -> CutplanError:
    return CutplanError(
        "E_FORMAT_VERSION",
        f"unsupported format version {found!r}; supported: {', '.join(SUPPORTED_VERSIONS)}",
    )


def normalize_version(value: object) -> str:
    text = str(value).strip().lower().lstrip("v")
    if text in {"1", "v1"}:
        return "v1"
    if text in {"2", "v2"}:
        return "v2"
    raise _version_error(value)


def detect_version_text(text: str) -> str:
    stripped = text.lstrip("\ufeff \t\r\n")
    if stripped.startswith("CUTPLAN|"):
        header = stripped.splitlines()[0].strip()
        version = header.split("|", 1)[1]
        if version == "1":
            return "v1"
        raise _version_error(version)
    if stripped.startswith("{"):
        try:
            root: Any = json.loads(stripped)
        except (TypeError, ValueError, json.JSONDecodeError) as exc:
            raise CutplanError("E_JSON_INVALID", "input is not valid JSON") from exc
        if not isinstance(root, dict):
            raise CutplanError("E_FORMAT_UNKNOWN", "JSON root must be an object")
        schema = root.get("schema")
        version = root.get("version")
        if schema == SCHEMA and version == 2:
            return "v2"
        raise _version_error(version if version is not None else schema)
    raise CutplanError("E_FORMAT_UNKNOWN", "cannot detect cut plan format")


def parse_text(text: str) -> Timeline:
    version = detect_version_text(text)
    return parse_v1(text) if version == "v1" else parse_v2(text)


def load_timeline(path: str | Path) -> Timeline:
    source = Path(path)
    try:
        text = source.read_text(encoding="utf-8")
    except OSError as exc:
        raise CutplanError("E_INPUT_READ", f"cannot read input: {source}") from exc
    return parse_text(text)


def dumps_version(timeline: Timeline, version: object) -> str:
    target = normalize_version(version)
    return dumps_v1(timeline) if target == "v1" else dumps_v2(timeline)


def save_timeline(timeline: Timeline, path: str | Path, version: object) -> str:
    target = normalize_version(version)
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    if target == "v1":
        destination.write_text(dumps_v1(timeline), encoding="utf-8")
    else:
        destination.write_text(dumps_v2(timeline), encoding="utf-8")
    return target


def convert_file(source: str | Path, destination: str | Path, to: object) -> tuple[Timeline, str]:
    timeline = load_timeline(source)
    target = save_timeline(timeline, destination, to)
    return timeline, target


def read_any(path: str | Path) -> Timeline:
    return load_timeline(path)


detect_version = detect_version_text
load = load_timeline


__all__ = [
    "SUPPORTED_VERSIONS",
    "convert_file",
    "detect_version_text",
    "dumps_version",
    "load_timeline",
    "normalize_version",
    "parse_text",
    "read_any",
    "read_v1",
    "read_v2",
    "save_timeline",
]
