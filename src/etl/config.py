"""Declarative mapping-spec loading and validation.

Validates the target schema, per-source field bindings, source priorities
and the failure policy. Every validation error is reported with the YAML
line number it originates from.
"""
from __future__ import annotations

from dataclasses import dataclass

import yaml

TARGET_TYPES = ("decimal", "float", "int", "datetime", "string")
SOURCE_TYPES = ("csv", "jsonl", "sqlite")
FAILURE_POLICIES = ("quarantine", "fail-fast")
UNIT_FACTORS = {("cent", "yuan"): 0.01, ("yuan", "cent"): 100.0}


class ConfigError(Exception):
    """Raised for any mapping-spec violation; message carries file:line."""


class _LineLoader(yaml.SafeLoader):
    """SafeLoader that records the 1-based line of every mapping node."""


def _construct_mapping(loader, node, deep=False):
    mapping = yaml.SafeLoader.construct_mapping(loader, node, deep=deep)
    mapping["__line__"] = node.start_mark.line + 1
    mapping["__lines__"] = {
        key_node.value: value_node.start_mark.line + 1
        for key_node, value_node in node.value
    }
    return mapping


_LineLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _construct_mapping
)


@dataclass
class Binding:
    column: str
    unit_factor: float | None = None


@dataclass
class SourceConfig:
    name: str
    type: str
    path: str
    table: str | None
    priority: int
    bindings: dict  # target field -> Binding


@dataclass
class Config:
    path: str
    target_table: str
    target_schema: dict  # field -> {"type":..., "pk":bool, "default":..., "enum":...}
    enums: dict  # enum name -> {raw: canonical}
    sources: list  # list[SourceConfig]
    failure_policy: str
    batch_size: int
    run_dir: str


def _line(node, fallback="?"):
    if isinstance(node, dict):
        return node.get("__line__", fallback)
    return fallback


def _key_line(node, key, fallback="?"):
    if isinstance(node, dict):
        return node.get("__lines__", {}).get(key, _line(node, fallback))
    return fallback


def load_config(path: str) -> Config:
    try:
        with open(path, encoding="utf-8") as fh:
            raw = yaml.load(fh, Loader=_LineLoader)
    except OSError as exc:
        raise ConfigError(f"{path}: cannot read file: {exc}")
    except yaml.YAMLError as exc:
        raise ConfigError(f"{path}: invalid YAML: {exc}")
    if not isinstance(raw, dict):
        raise ConfigError(f"{path}:1: top-level mapping required")

    def err(line, msg):
        raise ConfigError(f"{path}:{line}: {msg}")

    # ---- target schema -------------------------------------------------
    schema_raw = raw.get("target_schema")
    if not isinstance(schema_raw, dict) or not schema_raw:
        err(_line(raw, 1), "target_schema must be a non-empty mapping")
    target_schema = {}
    pk_fields = []
    for field, spec in schema_raw.items():
        if field in ("__line__", "__lines__"):
            continue
        line = _line(spec)
        if not isinstance(spec, dict):
            err(line, f"target field {field!r} must be a mapping")
        ftype = spec.get("type")
        if ftype not in TARGET_TYPES:
            err(line, f"target field {field!r} has unknown type {ftype!r}; "
                      f"expected one of {TARGET_TYPES}")
        target_schema[field] = {
            "type": ftype,
            "pk": bool(spec.get("pk", False)),
            "default": spec.get("default"),
            "enum": spec.get("enum"),
        }
        if spec.get("pk"):
            pk_fields.append(field)
    if len(pk_fields) != 1:
        err(_line(schema_raw), f"exactly one pk field required, got {len(pk_fields)}")

    # ---- enums ----------------------------------------------------------
    enums_raw = raw.get("enums", {}) or {}
    enums = {}
    for name, table in enums_raw.items():
        if name in ("__line__", "__lines__"):
            continue
        if not isinstance(table, dict) or not table:
            err(_line(enums_raw), f"enum {name!r} must be a non-empty mapping")
        enums[name] = {str(k): str(v) for k, v in table.items()
                       if k not in ("__line__", "__lines__")}
    for field, spec in target_schema.items():
        if spec["enum"] is not None and spec["enum"] not in enums:
            err(_line(schema_raw), f"field {field!r} references unknown enum {spec['enum']!r}")

    # ---- sources ---------------------------------------------------------
    sources_raw = raw.get("sources")
    if not isinstance(sources_raw, list) or not sources_raw:
        err(_line(raw, 1), "sources must be a non-empty list")
    sources = []
    seen_names, seen_priorities = set(), set()
    for entry in sources_raw:
        line = _line(entry)
        if not isinstance(entry, dict):
            err(line, "source entry must be a mapping")
        name = entry.get("name")
        stype = entry.get("type")
        spath = entry.get("path")
        priority = entry.get("priority")
        if not isinstance(name, str) or not name:
            err(line, "source requires a non-empty 'name'")
        if name in seen_names:
            err(line, f"duplicate source name {name!r}")
        seen_names.add(name)
        if stype not in SOURCE_TYPES:
            err(line, f"source {name!r} has unknown type {stype!r}; expected {SOURCE_TYPES}")
        if not isinstance(spath, str) or not spath:
            err(line, f"source {name!r} requires a 'path'")
        table = entry.get("table")
        if stype == "sqlite" and not table:
            err(line, f"sqlite source {name!r} requires a 'table'")
        if not isinstance(priority, int) or isinstance(priority, bool):
            err(line, f"source {name!r} requires an integer 'priority'")
        if priority in seen_priorities:
            err(line, f"duplicate source priority {priority}")
        seen_priorities.add(priority)
        bindings_raw = entry.get("bindings")
        if not isinstance(bindings_raw, dict) or not bindings_raw:
            err(line, f"source {name!r} requires a non-empty 'bindings' mapping")
        bindings = {}
        for target, spec in bindings_raw.items():
            if target in ("__line__", "__lines__"):
                continue
            bline = _key_line(bindings_raw, target, line)
            if target not in target_schema:
                err(bline, f"source {name!r} binds unknown target field {target!r}")
            bindings[target] = _parse_binding(name, target, spec, bline, err)
        sources.append(SourceConfig(name, stype, spath, table, priority, bindings))

    # ---- misc ------------------------------------------------------------
    policy = raw.get("failure_policy", "quarantine")
    if policy not in FAILURE_POLICIES:
        err(_line(raw, 1), f"failure_policy must be one of {FAILURE_POLICIES}")
    batch_size = raw.get("batch_size", 50000)
    if not isinstance(batch_size, int) or batch_size <= 0:
        err(_line(raw, 1), "batch_size must be a positive integer")
    run_dir = raw.get("run_dir", "run")
    target_table = raw.get("target_table", "facts")
    return Config(path, target_table, target_schema, enums, sources,
                  policy, batch_size, run_dir)


def _parse_binding(source, target, spec, line, err) -> Binding:
    if isinstance(spec, str):
        return Binding(column=spec)
    if isinstance(spec, dict):
        column = spec.get("column")
        if not isinstance(column, str) or not column:
            err(line, f"binding {source}.{target} requires a 'column'")
        factor = None
        unit = spec.get("unit")
        if unit is not None:
            if not isinstance(unit, dict):
                err(line, f"binding {source}.{target} unit must be a mapping")
            pair = (unit.get("from"), unit.get("to"))
            factor = UNIT_FACTORS.get(pair)
            if factor is None:
                err(line, f"binding {source}.{target} has unknown unit "
                          f"conversion {pair[0]!r}->{pair[1]!r}")
        return Binding(column=column, unit_factor=factor)
    err(line, f"binding {source}.{target} must be a column name or mapping")
