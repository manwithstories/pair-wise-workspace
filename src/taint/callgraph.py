"""Call-site extraction and alias-aware callee resolution."""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Any

from .alias import AliasIndex
from .cache import AstNode
from .resolver import FunctionDef, ModuleGraph, ModuleInfo


def _text(node: AstNode | None) -> str:
    if node is None:
        return ""
    return re.sub(r"\s+", "", node.text.decode("utf-8", errors="replace"))


@dataclass
class CallArg:
    index: int
    name: str | None
    node: AstNode
    splat: bool = False


@dataclass
class CallSite:
    module: str
    owner: str
    owner_id: str
    line: int
    start_byte: int
    expression: str
    canonical_names: tuple[str, ...]
    internal_ids: tuple[str, ...]
    args: tuple[CallArg, ...]
    node: AstNode
    resolved_targets: tuple[str, ...] = ()


@dataclass
class CallGraph:
    sites: list[CallSite]
    by_owner: dict[str, list[CallSite]]
    by_start: dict[tuple[str, int], CallSite]
    edges: dict[str, set[str]]
    diagnostics: list[dict[str, Any]] = field(default_factory=list)

    def calls_for(self, owner_id: str) -> list[CallSite]:
        return self.by_owner.get(owner_id, [])

    def call_at(self, module: str, start_byte: int) -> CallSite | None:
        return self.by_start.get((module, start_byte))


def _arguments(call: AstNode) -> tuple[CallArg, ...]:
    arguments = call.child_by_field_name("arguments")
    if arguments is None:
        return ()
    result: list[CallArg] = []
    position = 0
    for child in arguments.named_children:
        if child.type == "keyword_argument":
            name_node = child.child_by_field_name("name")
            value_node = child.child_by_field_name("value")
            if value_node is None:
                value_node = child.named_children[-1] if child.named_children else child
            result.append(CallArg(position, _text(name_node), value_node))
            position += 1
        elif child.type in {"list_splat", "dictionary_splat"}:
            value = child.named_children[0] if child.named_children else child
            result.append(CallArg(position, None, value, splat=True))
            position += 1
        else:
            result.append(CallArg(position, None, child))
            position += 1
    return tuple(result)


def _module_function_ids(
    graph: ModuleGraph, module: ModuleInfo, name: str
) -> tuple[str, ...]:
    matches = []
    for function in module.functions:
        if function.qualname == name or function.name == name:
            matches.append(function.id)
    return tuple(sorted(matches))


def _module_prefixes(graph: ModuleGraph) -> set[str]:
    result: set[str] = set()
    for module_name in graph.by_name:
        result.add(module_name)
        parts = module_name.split(".")
        for index in range(1, len(parts)):
            result.add(".".join(parts[:index]))
    return result


def _is_internal_target(graph: ModuleGraph, target: str, prefixes: set[str]) -> bool:
    if graph.resolve_dotted_symbol(target) is not None:
        return True
    return any(
        target == prefix or target.startswith(prefix + ".") for prefix in prefixes
    )


def _candidate_targets(
    graph: ModuleGraph,
    aliases: AliasIndex,
    module: ModuleInfo,
    owner: FunctionDef | None,
    raw: str,
) -> list[str]:
    candidates: set[str] = {raw}
    resolved = set(
        aliases.resolve_expr(module.relpath, owner.qualname if owner else None, raw)
    )
    candidates.update(resolved)

    if owner is not None and owner.class_name and raw.startswith("self."):
        method = raw.split(".", 1)[1]
        candidates.add(f"{module.module_name}.{owner.class_name}.{method}")
    if owner is not None and "." in raw:
        head, tail = raw.split(".", 1)
        if head == owner.class_name:
            candidates.add(f"{module.module_name}.{raw}")

    # Imported-module aliases can occur as the head of a dotted call.
    if "." in raw:
        head, tail = raw.split(".", 1)
        for head_target in aliases.aliases_for(
            module.relpath, owner.qualname if owner else None, head
        ):
            if head_target != head:
                candidates.add(f"{head_target}.{tail}")

    for candidate in list(candidates):
        if candidate.startswith(module.module_name + "."):
            candidates.add(candidate[len(module.module_name) + 1 :])
        elif "." not in candidate:
            candidates.add(f"{module.module_name}.{candidate}")
    return sorted(candidate for candidate in candidates if candidate)


def _resolve_call(
    graph: ModuleGraph,
    aliases: AliasIndex,
    module: ModuleInfo,
    owner: FunctionDef | None,
    raw: str,
    prefixes: set[str],
) -> tuple[tuple[str, ...], tuple[str, ...], tuple[str, ...]]:
    candidates = _candidate_targets(graph, aliases, module, owner, raw)
    canonical: set[str] = set(candidates)
    internal: dict[str, str] = {}
    resolved_targets: set[str] = set()

    for candidate in candidates:
        local_ids = _module_function_ids(graph, module, candidate)
        for function_id in local_ids:
            internal[function_id] = candidate
        resolved = graph.resolve_dotted_symbol(candidate)
        if resolved is not None:
            resolved_targets.add(resolved)
            canonical.add(resolved)
            function = graph.function_by_dotted(resolved)
            if function is not None:
                internal[function.id] = resolved
        function = graph.function_by_canonical(candidate)
        if function is not None:
            internal[function.id] = candidate

    return (
        tuple(sorted(canonical)),
        tuple(sorted(internal)),
        tuple(sorted(resolved_targets)),
    )


def _collect_module_calls(
    module: ModuleInfo,
    sites: list[CallSite],
    diagnostics: list[dict[str, Any]],
) -> None:
    def scan(node: AstNode, owner: FunctionDef | None, root: AstNode) -> None:
        for child in node.named_children:
            if child is not root and child.type in {
                "function_definition",
                "class_definition",
                "decorated_definition",
            }:
                continue
            if child.type == "call":
                sites.append(
                    CallSite(
                        module=module.relpath,
                        owner=owner.qualname if owner else "<module>",
                        owner_id=owner.id if owner else f"{module.relpath}::<module>",
                        line=child.start_point[0] + 1,
                        start_byte=child.start_byte,
                        expression="",
                        canonical_names=(),
                        internal_ids=(),
                        args=_arguments(child),
                        node=child,
                    )
                )
            scan(child, owner, root)

    scan(module.root, None, module.root)
    for function in module.functions:
        if function.body is not None:
            scan(function.body, function, function.body)


def build_callgraph(
    graph: ModuleGraph,
    aliases: AliasIndex,
) -> CallGraph:
    raw_sites: list[CallSite] = []
    diagnostics: list[dict[str, Any]] = []
    prefixes = _module_prefixes(graph)
    for relpath in sorted(graph.modules):
        _collect_module_calls(graph.modules[relpath], raw_sites, diagnostics)

    resolved_sites: list[CallSite] = []
    for site in raw_sites:
        function_node = site.node.child_by_field_name("function")
        raw = _text(function_node)
        owner = graph.functions.get(site.owner_id)
        module = graph.modules[site.module]
        canonical, internal, resolved = _resolve_call(
            graph, aliases, module, owner, raw, prefixes
        )
        site.expression = raw
        site.canonical_names = canonical
        site.internal_ids = internal
        site.resolved_targets = resolved
        resolved_sites.append(site)
        if not internal:
            import_binding = next(
                (
                    binding
                    for binding in module.imports
                    if binding.local == raw.split(".", 1)[0]
                ),
                None,
            )
            if import_binding is not None and import_binding.target:
                target = import_binding.target
                if (
                    _is_internal_target(graph, target, prefixes)
                    and graph.resolve_dotted_symbol(target) is None
                ):
                    diagnostics.append(
                        {
                            "severity": "warning",
                            "code": "unresolved_call",
                            "message": f"cannot resolve call target {raw!r}",
                            "file": site.module,
                            "line": site.line,
                        }
                    )

    resolved_sites.sort(
        key=lambda site: (site.module, site.start_byte, site.owner, site.expression)
    )
    by_owner: dict[str, list[CallSite]] = {}
    by_start: dict[tuple[str, int], CallSite] = {}
    edges: dict[str, set[str]] = {
        function.id: set() for function in graph.functions.values()
    }
    for site in resolved_sites:
        by_owner.setdefault(site.owner_id, []).append(site)
        by_start[(site.module, site.start_byte)] = site
        if site.owner_id in edges:
            edges[site.owner_id].update(site.internal_ids)

    return CallGraph(
        sites=resolved_sites,
        by_owner=by_owner,
        by_start=by_start,
        edges=edges,
        diagnostics=diagnostics,
    )
