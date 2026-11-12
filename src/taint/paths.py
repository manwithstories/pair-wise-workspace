"""Path normalization, stable ordering, diagnostics and report assembly."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterable

from .policy import Policy
from .summary import SinkSite, SourceSite, TaintPath


@dataclass(frozen=True)
class ReportPath:
    identifier: str
    source: SourceSite
    chain: tuple[str, ...]
    sink: SinkSite

    @property
    def chain_text(self) -> str:
        return "->".join(self.chain)

    def terminal_line(self) -> str:
        return (
            f"{self.identifier} {self.source.file}:{self.source.line} "
            f"{self.chain_text} {self.sink.file}:{self.sink.line} {self.sink.name}"
        )

    def as_dict(self) -> dict[str, Any]:
        return {
            "id": self.identifier,
            "source": {"file": self.source.file, "line": self.source.line},
            "functions": list(self.chain),
            "function_chain": self.chain_text,
            "sink": {
                "file": self.sink.file,
                "line": self.sink.line,
                "name": self.sink.name,
            },
        }


def _diagnostic_key(diagnostic: dict[str, Any]) -> tuple[Any, ...]:
    return (
        str(diagnostic.get("severity", "warning")),
        str(diagnostic.get("code", "")),
        str(diagnostic.get("file", "")),
        int(diagnostic.get("line", 0) or 0),
        str(diagnostic.get("message", "")),
    )


def normalize_diagnostics(
    diagnostics: Iterable[dict[str, Any]],
) -> list[dict[str, Any]]:
    unique: dict[tuple[Any, ...], dict[str, Any]] = {}
    for diagnostic in diagnostics:
        key = _diagnostic_key(diagnostic)
        unique.setdefault(
            key,
            {
                "severity": key[0],
                "code": key[1],
                "message": key[4],
                **({"file": key[2]} if key[2] else {}),
                **({"line": key[3]} if key[3] else {}),
            },
        )
    return [unique[key] for key in sorted(unique)]


def finalize_paths(paths: Iterable[TaintPath], max_paths: int) -> list[ReportPath]:
    unique: dict[tuple[Any, ...], TaintPath] = {}
    for path in paths:
        unique.setdefault(path.key, path)
    ordered = sorted(unique.values(), key=lambda path: path.key)
    if len(ordered) > max_paths:
        ordered = ordered[:max_paths]
    return [
        ReportPath(f"P{number}", path.source, path.chain, path.sink)
        for number, path in enumerate(ordered, start=1)
    ]


def build_report(
    *,
    policy: Policy,
    files: int,
    parsed: int,
    sources: int,
    sinks: int,
    report_paths: list[ReportPath],
    diagnostics: list[dict[str, Any]],
    sanitized_candidates: int,
    truncated: bool,
) -> dict[str, Any]:
    return {
        "schema_version": 1,
        "summary": {
            "files": files,
            "parsed": parsed,
            "sources": sources,
            "sinks": sinks,
            "paths": len(report_paths),
            "diagnostics": len(diagnostics),
            "sanitized_candidates": sanitized_candidates,
            "truncated": bool(truncated),
        },
        "paths": [path.as_dict() for path in report_paths],
        "diagnostics": diagnostics,
        "policy": {
            "sources": len(policy.sources.functions) + len(policy.sources.attributes),
            "sinks": len(policy.sinks),
            "sanitizers": len(policy.sanitizers),
        },
    }
