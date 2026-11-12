"""Final concrete-taint propagation built on top of fixed-point summaries."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .alias import AliasIndex
from .callgraph import CallGraph
from .policy import Policy
from .resolver import ModuleGraph
from .summary import Analyzer, EventIndex, SummaryResult, TaintPath, index_events


@dataclass
class PropagationResult:
    paths: set[TaintPath] = field(default_factory=set)
    sources: int = 0
    sinks: int = 0
    sanitized_candidates: int = 0
    truncated: bool = False
    diagnostics: list[dict[str, Any]] = field(default_factory=list)


def propagate(
    graph: ModuleGraph,
    aliases: AliasIndex,
    callgraph: CallGraph,
    policy: Policy,
    summaries: SummaryResult,
) -> PropagationResult:
    events: EventIndex = index_events(graph, callgraph, policy, aliases)
    result = PropagationResult(
        sources=len(events.source_keys), sinks=len(events.sink_sites)
    )
    analyzer = Analyzer(
        graph=graph,
        aliases=aliases,
        callgraph=callgraph,
        policy=policy,
        summaries=summaries.summaries,
        mode="final",
    )
    sanitized: set[tuple[Any, ...]] = set()
    analyses = 0

    for module in sorted(graph.modules.values(), key=lambda value: value.relpath):
        if analyses >= policy.budgets.max_iterations:
            result.truncated = True
            break
        module_paths = analyzer.run_final_module(module)
        sanitized.update(analyzer.sanitized_keys)
        result.truncated = result.truncated or analyzer.truncated
        result.paths.update(module_paths)
        analyses += 1

    functions = sorted(
        graph.functions.values(), key=lambda value: (value.module, value.qualname)
    )

    # Produce class-field seeds from the method bodies before the emitted path
    # pass.  This catches the common ``self.x = parameter`` / read-in-another-
    # method pattern without introducing object-sensitive call contexts.
    field_analyzer = Analyzer(
        graph=graph,
        aliases=aliases,
        callgraph=callgraph,
        policy=policy,
        summaries=summaries.summaries,
        mode="summary",
    )
    class_methods: dict[tuple[str, str], list[Any]] = {}
    for function in functions:
        if function.class_name:
            class_methods.setdefault((function.module, function.class_name), []).append(
                function
            )
    class_fields: dict[tuple[str, str], dict[str, set[Any]]] = {}
    for key, methods in sorted(class_methods.items()):
        current: dict[str, set[Any]] = {}
        for _round in range(4):
            changed = False
            for function in sorted(methods, key=lambda value: value.qualname):
                field_analyzer.analyze_function(
                    function, graph.modules[function.module]
                )
                for name, values in field_analyzer.env.items():
                    if name.startswith("self."):
                        if not values <= current.get(name, set()):
                            changed = True
                        current.setdefault(name, set()).update(values)
            if not changed:
                break
        class_fields[key] = current

    for function in functions:
        if analyses >= policy.budgets.max_iterations:
            result.truncated = True
            break
        module = graph.modules[function.module]
        seeds = (
            class_fields.get((function.module, function.class_name), {})
            if function.class_name
            else {}
        )
        function_paths = analyzer.run_final_function(function, module, seeds)
        sanitized.update(analyzer.sanitized_keys)
        result.truncated = result.truncated or analyzer.truncated
        result.paths.update(function_paths)
        analyses += 1

    result.sanitized_candidates = len(sanitized)
    result.truncated = result.truncated or summaries.truncated
    if len(result.paths) > policy.budgets.max_paths:
        result.truncated = True
    if result.truncated:
        result.diagnostics.append(
            {
                "severity": "warning",
                "code": "analysis_truncated",
                "message": "analysis budget was reached; results are incomplete",
            }
        )
    return result
