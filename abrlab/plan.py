"""Plan generation, validation and canonical de-duplication."""

from __future__ import annotations

import copy
import hashlib
import json
import os
from pathlib import Path
from typing import Any


class PlanError(ValueError):
    """Raised when a scan plan is malformed or infeasible."""


BLOCK_SIZES = (4, 8, 16)
CHROMA_MODES = ("420", "444", "gray")
PATTERNS = ("mixed", "constant")
MAX_FRAMES = 120
MAX_DIMENSION = 1024
MAX_SOURCE_SAMPLES = 8_000_000
MAX_DUPLICATES = 1_000

DEMO_RESOLUTIONS = ((80, 45), (240, 135), (480, 270))
DEMO_QPS = (20, 24, 28, 32)
DEMO_BLOCKS = (4, 8)
DEMO_CHROMA = ("420", "gray")


def _require_int(value: Any, field: str, low: int, high: int) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise PlanError(f"{field} must be an integer")
    if not low <= value <= high:
        raise PlanError(f"{field} must be between {low} and {high}")
    return value


def _require_number(value: Any, field: str, low: float, high: float) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise PlanError(f"{field} must be a number")
    number = float(value)
    if not low <= number <= high:
        raise PlanError(f"{field} must be between {low:g} and {high:g}")
    return number


def canonical_json(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True)


def task_key(source: dict[str, Any], params: dict[str, Any]) -> str:
    payload = {"source": source, "params": params}
    return hashlib.sha256(canonical_json(payload).encode("ascii")).hexdigest()


def _validate_source(raw: Any, prefix: str) -> dict[str, Any]:
    if not isinstance(raw, dict):
        raise PlanError(f"{prefix} must be an object")
    width = _require_int(raw.get("width"), f"{prefix}.width", 1, MAX_DIMENSION)
    height = _require_int(raw.get("height"), f"{prefix}.height", 1, MAX_DIMENSION)
    base_width = _require_int(
        raw.get("base_width", width), f"{prefix}.base_width", 1, MAX_DIMENSION
    )
    base_height = _require_int(
        raw.get("base_height", height), f"{prefix}.base_height", 1, MAX_DIMENSION
    )
    frames = _require_int(raw.get("frames"), f"{prefix}.frames", 1, MAX_FRAMES)
    fps = _require_number(raw.get("fps"), f"{prefix}.fps", 1.0, 120.0)
    seed = _require_int(raw.get("seed"), f"{prefix}.seed", 0, 2**32 - 1)
    pattern = raw.get("pattern", "mixed")
    if pattern not in PATTERNS:
        raise PlanError(f"{prefix}.pattern must be one of {PATTERNS}")
    if frames * width * height > MAX_SOURCE_SAMPLES:
        raise PlanError(f"{prefix} exceeds the source sample limit")
    if frames * base_width * base_height > MAX_SOURCE_SAMPLES:
        raise PlanError(f"{prefix} base frames exceed the source sample limit")
    return {
        "seed": seed,
        "frames": frames,
        "fps": fps,
        "width": width,
        "height": height,
        "base_width": base_width,
        "base_height": base_height,
        "pattern": pattern,
    }


def _validate_params(raw: Any, prefix: str) -> dict[str, Any]:
    if not isinstance(raw, dict):
        raise PlanError(f"{prefix} must be an object")
    qp = _require_int(raw.get("qp"), f"{prefix}.qp", 1, 51)
    block = _require_int(raw.get("block_size"), f"{prefix}.block_size", 4, 16)
    if block not in BLOCK_SIZES:
        raise PlanError(f"{prefix}.block_size must be one of {BLOCK_SIZES}")
    chroma = raw.get("chroma")
    if chroma not in CHROMA_MODES:
        raise PlanError(f"{prefix}.chroma must be one of {CHROMA_MODES}")
    return {"qp": qp, "block_size": block, "chroma": chroma}


def make_entry(source: dict[str, Any], params: dict[str, Any], name: str | None = None) -> dict[str, Any]:
    src = _validate_source(source, "source")
    par = _validate_params(params, "params")
    auto_name = f"{src['width']}x{src['height']}_qp{par['qp']}_b{par['block_size']}_{par['chroma']}"
    entry = {"name": name or auto_name, "source": src, "params": par}
    entry["task_key"] = task_key(src, par)
    return entry


def normalize_entries(raw_entries: Any) -> tuple[list[dict[str, Any]], int]:
    if not isinstance(raw_entries, list):
        raise PlanError("entries must be a list")
    unique: list[dict[str, Any]] = []
    seen_keys: dict[str, str] = {}
    seen_names: dict[str, str] = {}
    duplicates = 0
    for index, raw in enumerate(raw_entries):
        if not isinstance(raw, dict):
            raise PlanError(f"entries[{index}] must be an object")
        name = raw.get("name")
        if not isinstance(name, str) or not name.strip():
            raise PlanError(f"entries[{index}].name must be a non-empty string")
        entry = make_entry(raw.get("source"), raw.get("params"), name.strip())
        key = entry["task_key"]
        if name in seen_names and seen_names[name] != key:
            raise PlanError(f"duplicate name {name!r} refers to different parameters")
        if key in seen_keys:
            duplicates += 1
            continue
        seen_names[name] = key
        seen_keys[key] = name
        unique.append(entry)
    return unique, duplicates


def normalize_plan(plan: Any) -> tuple[dict[str, Any], int]:
    if not isinstance(plan, dict):
        raise PlanError("plan must be a JSON object")
    if plan.get("version", 1) != 1:
        raise PlanError("unsupported plan version")
    entries, duplicates = normalize_entries(plan.get("entries"))
    if not entries:
        raise PlanError("plan contains no entries")
    return {"version": 1, "entries": entries}, duplicates


def load_plan(path: str | os.PathLike[str]) -> tuple[dict[str, Any], int]:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            raw = json.load(handle)
    except OSError as exc:
        raise PlanError(f"cannot read plan: {exc}") from exc
    except json.JSONDecodeError as exc:
        raise PlanError(f"invalid plan JSON: {exc}") from exc
    return normalize_plan(raw)


def write_json_atomic(path: str | os.PathLike[str], value: Any) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temp = target.with_name(f".{target.name}.{os.getpid()}.tmp")
    try:
        with open(temp, "w", encoding="utf-8") as handle:
            json.dump(value, handle, indent=2, sort_keys=True)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, target)
    finally:
        if temp.exists():
            temp.unlink()


def generate_demo(inject_duplicates: int = 0) -> dict[str, Any]:
    if isinstance(inject_duplicates, bool) or not isinstance(inject_duplicates, int):
        raise PlanError("inject_duplicates must be an integer")
    if not 0 <= inject_duplicates <= MAX_DUPLICATES:
        raise PlanError(f"inject_duplicates must be between 0 and {MAX_DUPLICATES}")
    raw: list[dict[str, Any]] = []
    for width, height in DEMO_RESOLUTIONS:
        for qp, block, chroma in (
            (qp, block, chroma)
            for qp in DEMO_QPS
            for block in DEMO_BLOCKS
            for chroma in DEMO_CHROMA
        ):
            source = {
                "seed": 10291,
                "frames": 12,
                "fps": 24.0,
                "width": width,
                "height": height,
                "base_width": 480,
                "base_height": 270,
                "pattern": "mixed",
            }
            raw.append(make_entry(source, {"qp": qp, "block_size": block, "chroma": chroma}))
    for index in range(inject_duplicates):
        raw.append(copy.deepcopy(raw[index % len(raw)]))
    entries, duplicates = normalize_entries(raw)
    return {
        "version": 1,
        "entries": entries,
        "stats": {"entries": len(raw), "unique": len(entries), "duplicates_removed": duplicates},
    }
