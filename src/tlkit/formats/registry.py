"""Format detection and dispatch.

Every schema violation raises :class:`FormatError` carrying the JSON field
path (e.g. ``tracks[2].items[0].start``) so the CLI can report it and exit
with code 2.
"""
from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path

from ..model import Timeline


class FormatError(Exception):
    """Input/validation failure with a JSON field path."""

    def __init__(self, path: str, message: str):
        self.path = path
        self.message = message
        super().__init__(f"{path}: {message}")


@dataclass
class ParseReport:
    format_version: int
    source: str
    rounding: list[dict] = field(default_factory=list)

    def to_json(self) -> dict:
        return {
            "format_version": self.format_version,
            "source": self.source,
            "rounding_residuals": self.rounding,
        }


# ---- typed field helpers -------------------------------------------------

def req(obj: dict, key: str, path: str):
    if not isinstance(obj, dict) or key not in obj:
        raise FormatError(f"{path}.{key}" if path else key, "missing required field")
    return obj[key]


def as_int(value, path: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise FormatError(path, "must be an integer")
    return value


def as_bool(value, path: str) -> bool:
    if not isinstance(value, bool):
        raise FormatError(path, "must be a boolean")
    return value


def as_str(value, path: str) -> str:
    if not isinstance(value, str):
        raise FormatError(path, "must be a string")
    return value


def as_list(value, path: str) -> list:
    if not isinstance(value, list):
        raise FormatError(path, "must be an array")
    return value


def as_dict(value, path: str) -> dict:
    if not isinstance(value, dict):
        raise FormatError(path, "must be an object")
    return value


def opt(obj: dict, key: str, default):
    return obj[key] if isinstance(obj, dict) and key in obj else default


# ---- dispatch ------------------------------------------------------------

def dispatch(data, source: str = "<memory>") -> tuple[Timeline, ParseReport]:
    if not isinstance(data, dict):
        raise FormatError("$", "root must be an object")
    if "version" not in data:
        raise FormatError("version", "missing required field")
    version = data["version"]
    if isinstance(version, bool) or not isinstance(version, int):
        raise FormatError("version", "must be an integer")
    if version == 1:
        from . import v1
        return v1.parse(data, source)
    if version == 2:
        from . import v2
        return v2.parse(data, source)
    raise FormatError("version", f"unsupported format version {version}")


def load(path: str | Path) -> tuple[Timeline, ParseReport]:
    source = str(path)
    try:
        text = Path(path).read_text(encoding="utf-8")
    except OSError as exc:
        raise FormatError("$", f"cannot read file: {exc}") from exc
    try:
        data = json.loads(text)
    except json.JSONDecodeError as exc:
        raise FormatError("$", f"invalid JSON: {exc}") from exc
    return dispatch(data, source)
