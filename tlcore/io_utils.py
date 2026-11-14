"""Small deterministic document helpers shared by exchange modules."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import yaml


class ExchangeError(ValueError):
    """Raised for malformed or unsupported exchange documents."""


class DocumentIOError(OSError):
    """Raised when a document cannot be opened, parsed or written."""


def load_document(path: str | Path) -> dict[str, Any]:
    location = Path(path)
    try:
        text = location.read_text(encoding="utf-8")
        data = json.loads(text) if location.suffix == ".json" else yaml.safe_load(text)
    except OSError as exc:
        raise DocumentIOError(f"cannot read {location}: {exc}") from exc
    except (ValueError, yaml.YAMLError) as exc:
        raise ExchangeError(f"cannot read {location}: {exc}") from exc
    if not isinstance(data, dict):
        raise ExchangeError(f"{location}: document root must be a mapping")
    return data


def save_document(path: str | Path, data: dict[str, Any]) -> None:
    location = Path(path)
    try:
        location.parent.mkdir(parents=True, exist_ok=True)
        if location.suffix in (".yaml", ".yml"):
            text = yaml.safe_dump(data, sort_keys=False, allow_unicode=True)
        else:
            text = json.dumps(data, indent=2, ensure_ascii=False, sort_keys=False) + "\n"
        location.write_text(text, encoding="utf-8")
    except OSError as exc:
        raise DocumentIOError(f"cannot write {location}: {exc}") from exc


def mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ExchangeError(f"{label} must be a mapping")
    return value


def sequence(value: Any, label: str) -> list[Any]:
    if not isinstance(value, list):
        raise ExchangeError(f"{label} must be a list")
    return value


def text(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value:
        raise ExchangeError(f"{label} must be a non-empty string")
    return value


def integer(value: Any, label: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise ExchangeError(f"{label} must be an integer")
    return value


def extension_fields(data: dict[str, Any], known: set[str]) -> dict[str, Any]:
    result = dict(mapping(data.get("extensions", {}), "extensions"))
    result.update({key: value for key, value in data.items() if key not in known and key != "extensions"})
    return result
