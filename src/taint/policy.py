"""YAML policy loading, validation, defaults and stable diagnostics."""

from __future__ import annotations

import fnmatch
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence

import yaml


class PolicyError(RuntimeError):
    """A policy cannot be loaded or is structurally unusable."""


DEFAULT_IGNORE_DIRS = (
    ".git",
    ".cache",
    "__pycache__",
    ".pytest_cache",
    ".venv",
    "venv",
    "node_modules",
    "build",
    "dist",
    ".tox",
    ".mypy_cache",
)

DEFAULT_SOURCE_FUNCTIONS = (
    "input",
    "os.getenv",
    "os.environ.get",
    "request.args.get",
    "request.form.get",
    "request.values.get",
    "request.get_json",
)
DEFAULT_SOURCE_ATTRIBUTES = (
    "request.args",
    "request.form",
    "request.values",
    "request.json",
    "request.data",
)
DEFAULT_SINKS = ("eval", "exec", "os.system", "subprocess.run", "cursor.execute")
DEFAULT_SANITIZERS = (
    "int",
    "float",
    "bool",
    "shlex.quote",
    "html.escape",
    "os.path.basename",
)


@dataclass(frozen=True)
class SinkSpec:
    name: str
    arg_indexes: tuple[int, ...] | None = None

    def applies_to(self, argument_index: int) -> bool:
        return self.arg_indexes is None or argument_index in self.arg_indexes


@dataclass(frozen=True)
class SourceSpec:
    functions: tuple[str, ...]
    attributes: tuple[str, ...]
    parameters: tuple[str, ...] | None
    environment: tuple[str, ...] | None


@dataclass(frozen=True)
class Budgets:
    max_files: int = 500
    max_summary_rounds: int = 12
    max_iterations: int = 2000
    max_paths: int = 1000
    max_chain: int = 12
    max_summary_items: int = 20000


@dataclass
class Policy:
    sources: SourceSpec
    sinks: tuple[SinkSpec, ...]
    sanitizers: tuple[str, ...]
    budgets: Budgets
    ignore_dirs: tuple[str, ...]
    diagnostics: list[dict[str, Any]] = field(default_factory=list)
    raw: dict[str, Any] = field(default_factory=dict)


def _warning(code: str, message: str) -> dict[str, Any]:
    return {"severity": "warning", "code": code, "message": message}


def _as_sequence(value: Any) -> list[Any] | None:
    if value is None:
        return None
    if isinstance(value, (list, tuple)):
        return list(value)
    return None


def _string_list(
    value: Any, field_name: str, diagnostics: list[dict[str, Any]]
) -> tuple[str, ...]:
    values = _as_sequence(value)
    if values is None:
        diagnostics.append(
            _warning("policy_type", f"{field_name} must be a list; using defaults")
        )
        return ()
    result: list[str] = []
    for item in values:
        if isinstance(item, str) and item.strip():
            result.append(item.strip())
        else:
            diagnostics.append(
                _warning("policy_item", f"ignored invalid {field_name} item: {item!r}")
            )
    return tuple(dict.fromkeys(result))


def _parse_bool_or_list(
    value: Any,
    *,
    true_defaults: tuple[str, ...],
    field_name: str,
    diagnostics: list[dict[str, Any]],
) -> tuple[str, ...] | None:
    if value is None:
        return None
    if isinstance(value, bool):
        return true_defaults if value else None
    if isinstance(value, str):
        return (value,)
    values = _as_sequence(value)
    if values is None:
        diagnostics.append(
            _warning("policy_type", f"{field_name} must be bool or list")
        )
        return None
    strings = [
        item.strip() for item in values if isinstance(item, str) and item.strip()
    ]
    return tuple(dict.fromkeys(strings))


def _parse_sinks(value: Any, diagnostics: list[dict[str, Any]]) -> tuple[SinkSpec, ...]:
    if value is None:
        diagnostics.append(
            _warning("policy_default", "sinks missing; using built-in defaults")
        )
        return tuple(SinkSpec(name) for name in DEFAULT_SINKS)
    values = _as_sequence(value)
    if values is None:
        diagnostics.append(
            _warning("policy_type", "sinks must be a list; using defaults")
        )
        return tuple(SinkSpec(name) for name in DEFAULT_SINKS)
    result: list[SinkSpec] = []
    for item in values:
        if isinstance(item, str) and item.strip():
            result.append(SinkSpec(item.strip()))
            continue
        if isinstance(item, Mapping):
            name = item.get("name", item.get("function", item.get("symbol")))
            if not isinstance(name, str) or not name.strip():
                diagnostics.append(
                    _warning("policy_item", f"ignored sink without name: {item!r}")
                )
                continue
            args = item.get("args", item.get("arguments", item.get("arg_indexes")))
            arg_indexes: tuple[int, ...] | None = None
            if isinstance(args, int):
                arg_indexes = (args,)
            elif isinstance(args, (list, tuple)) and all(
                isinstance(i, int) and i >= 0 for i in args
            ):
                arg_indexes = tuple(sorted(set(args)))
            elif args is not None:
                diagnostics.append(
                    _warning("policy_item", f"ignored invalid args for sink {name!r}")
                )
            result.append(SinkSpec(name.strip(), arg_indexes))
            continue
        diagnostics.append(
            _warning("policy_item", f"ignored invalid sink item: {item!r}")
        )
    if not result:
        diagnostics.append(
            _warning("policy_default", "no valid sinks; using built-in defaults")
        )
        return tuple(SinkSpec(name) for name in DEFAULT_SINKS)
    return tuple(dict.fromkeys(result))


def _parse_sanitizers(value: Any, diagnostics: list[dict[str, Any]]) -> tuple[str, ...]:
    if value is None:
        diagnostics.append(
            _warning("policy_default", "sanitizers missing; using built-in defaults")
        )
        return DEFAULT_SANITIZERS
    values = _as_sequence(value)
    if values is None:
        diagnostics.append(
            _warning("policy_type", "sanitizers must be a list; using defaults")
        )
        return DEFAULT_SANITIZERS
    result: list[str] = []
    for item in values:
        if isinstance(item, str) and item.strip():
            result.append(item.strip())
        elif isinstance(item, Mapping) and isinstance(item.get("name"), str):
            result.append(item["name"].strip())
        else:
            diagnostics.append(
                _warning("policy_item", f"ignored invalid sanitizer: {item!r}")
            )
    return tuple(dict.fromkeys(result or list(DEFAULT_SANITIZERS)))


def _parse_budgets(value: Any, diagnostics: list[dict[str, Any]]) -> Budgets:
    defaults = Budgets()
    if value is None:
        diagnostics.append(
            _warning("policy_default", "budgets missing; using defaults")
        )
        return defaults
    if not isinstance(value, Mapping):
        diagnostics.append(
            _warning("policy_type", "budgets must be a mapping; using defaults")
        )
        return defaults
    aliases = {
        "max_files": ("max_files",),
        "max_summary_rounds": ("max_summary_rounds", "summary_rounds", "rounds"),
        "max_iterations": ("max_iterations", "iterations"),
        "max_paths": ("max_paths", "paths"),
        "max_chain": ("max_chain", "max_depth", "chain_depth"),
        "max_summary_items": ("max_summary_items", "summary_items"),
    }
    parsed: dict[str, int] = {}
    for target, names in aliases.items():
        raw = next((value[name] for name in names if name in value), None)
        if raw is None:
            continue
        if isinstance(raw, int) and raw > 0:
            parsed[target] = raw
        else:
            diagnostics.append(
                _warning("policy_budget", f"ignored invalid budget {names[0]}={raw!r}")
            )
    return Budgets(**{**defaults.__dict__, **parsed})


def load_policy(path: str | Path) -> Policy:
    policy_path = Path(path)
    if not policy_path.is_file():
        raise PolicyError(f"policy file not found: {path}")
    try:
        data = yaml.safe_load(policy_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, yaml.YAMLError) as exc:
        raise PolicyError(f"cannot load policy {path}: {exc}") from exc
    if data is None:
        data = {}
    if not isinstance(data, Mapping):
        raise PolicyError("policy root must be a YAML mapping")

    diagnostics: list[dict[str, Any]] = []
    raw_sources = data.get("sources")
    if raw_sources is None:
        diagnostics.append(
            _warning("policy_default", "sources missing; using built-in defaults")
        )
        raw_sources = {}
    elif isinstance(raw_sources, (list, tuple)):
        raw_sources = {"functions": list(raw_sources)}
    elif not isinstance(raw_sources, Mapping):
        diagnostics.append(
            _warning("policy_type", "sources must be a mapping or list; using defaults")
        )
        raw_sources = {}

    functions = _string_list(
        raw_sources.get(
            "functions", list(DEFAULT_SOURCE_FUNCTIONS) if not raw_sources else []
        ),
        "sources.functions",
        diagnostics,
    )
    if not functions:
        functions = DEFAULT_SOURCE_FUNCTIONS
    attributes = _string_list(
        raw_sources.get(
            "attributes", list(DEFAULT_SOURCE_ATTRIBUTES) if not raw_sources else []
        ),
        "sources.attributes",
        diagnostics,
    )
    parameters = _parse_bool_or_list(
        raw_sources.get("parameters"),
        true_defaults=("*",),
        field_name="sources.parameters",
        diagnostics=diagnostics,
    )
    environment = _parse_bool_or_list(
        raw_sources.get("environment", raw_sources.get("env")),
        true_defaults=("os.getenv", "os.environ.get", "os.environ"),
        field_name="sources.environment",
        diagnostics=diagnostics,
    )
    if environment is None:
        environment = ("os.getenv", "os.environ.get", "os.environ")

    sinks = _parse_sinks(data.get("sinks"), diagnostics)
    sanitizers = _parse_sanitizers(data.get("sanitizers"), diagnostics)
    budgets = _parse_budgets(data.get("budgets"), diagnostics)
    ignore_value = data.get("ignore_dirs", data.get("ignore"))
    if ignore_value is None:
        diagnostics.append(
            _warning("policy_default", "ignore_dirs missing; using defaults")
        )
        ignore_dirs = DEFAULT_IGNORE_DIRS
    else:
        explicit = _string_list(ignore_value, "ignore_dirs", diagnostics)
        ignore_dirs = tuple(dict.fromkeys(DEFAULT_IGNORE_DIRS + explicit))

    return Policy(
        sources=SourceSpec(functions, attributes, parameters, environment),
        sinks=sinks,
        sanitizers=sanitizers,
        budgets=budgets,
        ignore_dirs=ignore_dirs,
        diagnostics=diagnostics,
        raw=dict(data),
    )


def normalize_symbol(name: str) -> str:
    name = "".join(name.split())
    if name.startswith("builtins."):
        name = name[len("builtins.") :]
    return name


def _wildcard_match(spec: str, candidate: str) -> bool:
    if any(char in spec for char in "*?["):
        return fnmatch.fnmatchcase(candidate, spec)
    return False


def symbol_matches(specs: Sequence[str], candidates: Iterable[str]) -> str | None:
    """Return the matching policy symbol, preferring exact canonical matches."""

    normalized_specs = [(spec, normalize_symbol(spec)) for spec in specs]
    normalized_candidates = [
        (candidate, normalize_symbol(candidate))
        for candidate in candidates
        if candidate
    ]
    for spec, normalized_spec in normalized_specs:
        for candidate, normalized_candidate in normalized_candidates:
            if normalized_spec == normalized_candidate:
                return spec
    for spec, normalized_spec in normalized_specs:
        for candidate, normalized_candidate in normalized_candidates:
            if _wildcard_match(normalized_spec, normalized_candidate):
                return spec
            if normalized_candidate.endswith("." + normalized_spec):
                return spec
    return None


def source_call_match(policy: Policy, names: Iterable[str]) -> str | None:
    return symbol_matches(policy.sources.functions, names)


def source_attribute_match(policy: Policy, names: Iterable[str]) -> str | None:
    direct = symbol_matches(policy.sources.attributes, names)
    if direct is not None:
        return direct
    normalized_names = [normalize_symbol(name) for name in names if name]
    for spec in policy.sources.attributes:
        normalized_spec = normalize_symbol(spec)
        for candidate in normalized_names:
            if candidate.startswith(normalized_spec + ".") or candidate.startswith(
                normalized_spec + "["
            ):
                return spec
    return None


def sink_match(policy: Policy, names: Iterable[str]) -> SinkSpec | None:
    for spec in policy.sinks:
        if symbol_matches((spec.name,), names):
            return spec
    return None


def sanitizer_match(policy: Policy, names: Iterable[str]) -> str | None:
    return symbol_matches(policy.sanitizers, names)


def parameter_matches(
    policy: Policy, function_qualname: str, parameter_name: str
) -> bool:
    patterns = policy.sources.parameters
    if not patterns:
        return False
    candidates = (parameter_name, f"{function_qualname}.{parameter_name}")
    for pattern in patterns:
        for candidate in candidates:
            if (
                pattern == "*"
                or pattern == candidate
                or _wildcard_match(pattern, candidate)
            ):
                return True
    return False


def environment_match(
    policy: Policy, names: Iterable[str], variable: str | None = None
) -> str | None:
    patterns = policy.sources.environment
    if not patterns:
        return None
    candidates = list(names)
    if variable:
        candidates.extend(
            (variable, f"os.environ[{variable}]", f"os.environ.get({variable})")
        )
    match = symbol_matches(patterns, candidates)
    if match:
        return match
    normalized_candidates = [normalize_symbol(name) for name in candidates]
    for pattern in patterns:
        normalized_pattern = normalize_symbol(pattern)
        for candidate in normalized_candidates:
            if (
                candidate.startswith(normalized_pattern + "[")
                or candidate.startswith(normalized_pattern + "(")
                or candidate.startswith(normalized_pattern + ".")
            ):
                return pattern
    return None
