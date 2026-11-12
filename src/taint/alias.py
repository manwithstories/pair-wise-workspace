"""Flow-insensitive alias sets for assignments, imports and class fields."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Iterable

from .cache import AstNode
from .resolver import ModuleGraph, ModuleInfo


def _text(node: AstNode) -> str:
    return node.text.decode("utf-8", errors="replace").strip()


def _scope_key(module: str, owner: str | None) -> str:
    return f"{module}::{owner or '<module>'}"


@dataclass
class AliasIndex:
    """Alias graph keyed by (module, function scope, name).

    Public values are closed over direct assignment edges.  Field aliases are
    stored per class and expanded when ``self.attr`` participates in a chain.
    """

    edges: dict[str, dict[str, set[str]]] = field(default_factory=dict)
    field_edges: dict[tuple[str, str], dict[str, set[str]]] = field(
        default_factory=dict
    )

    def add_edge(
        self,
        module: str,
        owner: str | None,
        target: str,
        source: Iterable[str],
    ) -> None:
        if not source:
            return
        scope = _scope_key(module, owner)
        bucket = self.edges.setdefault(scope, {}).setdefault(target, set())
        bucket.update(item for item in source if item)

    def add_field_edge(
        self,
        module: str,
        class_name: str,
        field_name: str,
        source: Iterable[str],
    ) -> None:
        if not source:
            return
        bucket = self.field_edges.setdefault((module, class_name), {}).setdefault(
            field_name, set()
        )
        bucket.update(item for item in source if item)

    def _field_targets(self, module: str, owner: str | None, name: str) -> set[str]:
        class_name = owner.split(".", 1)[0] if owner and "." in owner else owner
        if not class_name or not name.startswith("self."):
            return set()
        return set(self.field_edges.get((module, class_name), {}).get(name, ()))

    def _scope_keys(self, module: str, owner: str | None) -> list[str]:
        scopes: list[str] = []
        if owner:
            scopes.append(_scope_key(module, owner))
            if "." in owner:
                scopes.append(_scope_key(module, owner.split(".", 1)[0]))
        scopes.append(_scope_key(module, None))
        return list(dict.fromkeys(scopes))

    def aliases_for(
        self,
        module: str,
        owner: str | None,
        name: str,
        _stack: frozenset[tuple[str, str, str]] | None = None,
    ) -> frozenset[str]:
        stack = _stack or frozenset()
        marker = (_scope_key(module, owner), name, name)
        if marker in stack:
            return frozenset({name})

        result: set[str] = {name}
        direct: set[str] = set()
        for scope in self._scope_keys(module, owner):
            direct.update(self.edges.get(scope, {}).get(name, ()))
        direct.update(self._field_targets(module, owner, name))
        next_stack = stack | {marker}
        for target in sorted(direct):
            result.add(target)
            result.update(self.aliases_for(module, owner, target, next_stack))
        return frozenset(result)

    def resolve_expr(
        self, module: str, owner: str | None, expression: str
    ) -> frozenset[str]:
        if not expression:
            return frozenset()
        result = set(self.aliases_for(module, owner, expression))
        # A dotted expression may start with an aliased module/function.
        if "." in expression:
            head, tail = expression.split(".", 1)
            for alias in sorted(self.aliases_for(module, owner, head)):
                if alias == head:
                    continue
                result.add(f"{alias}.{tail}")
        return frozenset(result)

    def function_aliases(
        self, module: str, owner: str | None = None
    ) -> dict[str, frozenset[str]]:
        scope = _scope_key(module, owner)
        names = sorted(self.edges.get(scope, {}))
        return {name: self.aliases_for(module, owner, name) for name in names}

    def field_aliases(
        self, module: str, class_name: str, field_name: str
    ) -> frozenset[str]:
        result: set[str] = {field_name}
        stack: frozenset[str] = frozenset()

        def visit(name: str) -> None:
            nonlocal stack
            if name in stack:
                return
            stack = stack | {name}
            for target in sorted(
                self.field_edges.get((module, class_name), {}).get(name, ())
            ):
                result.add(target)
                visit(target)

        visit(field_name)
        return frozenset(result)


def _target_names(node: AstNode) -> list[str]:
    if node.type in {"identifier", "attribute"}:
        return [_text(node)]
    if node.type in {
        "pattern_list",
        "tuple_pattern",
        "list_pattern",
        "expression_list",
        "tuple",
        "list",
    }:
        names: list[str] = []
        for child in node.named_children:
            names.extend(_target_names(child))
        return names
    return []


def _expression_names(node: AstNode | None) -> list[str]:
    if node is None:
        return []
    if node.type in {"identifier", "attribute"}:
        return [_text(node)]
    if node.type in {
        "tuple",
        "list",
        "expression_list",
        "pattern_list",
        "tuple_pattern",
        "list_pattern",
    }:
        names: list[str] = []
        for child in node.named_children:
            names.extend(_expression_names(child))
        return names
    if node.type == "parenthesized_expression":
        return _expression_names(
            node.named_children[0] if node.named_children else None
        )
    return []


def _class_for_owner(owner: str | None) -> str | None:
    if owner and "." in owner:
        return owner.split(".", 1)[0]
    return None


def _process_assignment(
    index: AliasIndex,
    module: str,
    owner: str | None,
    node: AstNode,
) -> None:
    left = node.child_by_field_name("left")
    right = node.child_by_field_name("right")
    if left is None or right is None:
        return

    if right.type == "assignment":
        _process_assignment(index, module, owner, right)
        alias_sources = [
            name for name in _target_names(right.child_by_field_name("left") or right)
        ]
        if not alias_sources:
            alias_sources = _expression_names(right)
    else:
        alias_sources = _expression_names(right)

    left_names = _target_names(left)
    if len(left_names) == 1 and len(alias_sources) > 1:
        alias_sources = [alias_sources[0]]
    elif len(left_names) > 1:
        rhs_items: list[list[str]] = []
        if right.type in {"tuple", "list", "expression_list"}:
            rhs_items = [_expression_names(child) for child in right.named_children]
        if len(rhs_items) != len(left_names):
            rhs_items = [alias_sources] * len(left_names)
        for target, sources in zip(left_names, rhs_items):
            _add_target_alias(index, module, owner, target, sources)
        return

    for target in left_names:
        _add_target_alias(index, module, owner, target, alias_sources)


def _add_target_alias(
    index: AliasIndex,
    module: str,
    owner: str | None,
    target: str,
    sources: Iterable[str],
) -> None:
    class_name = _class_for_owner(owner)
    if class_name and target.startswith("self."):
        index.add_field_edge(module, class_name, target, sources)
    else:
        index.add_edge(module, owner, target, sources)


def _walk_scope(
    index: AliasIndex, module: str, owner: str | None, node: AstNode
) -> None:
    for child in node.named_children:
        if child.type in {
            "function_definition",
            "class_definition",
            "decorated_definition",
        }:
            continue
        if child.type == "assignment":
            _process_assignment(index, module, owner, child)
        _walk_scope(index, module, owner, child)


def _seed_imports(index: AliasIndex, module: ModuleInfo) -> None:
    for binding in module.imports:
        if binding.target:
            index.add_edge(module.relpath, None, binding.local, (binding.target,))
        else:
            index.add_edge(module.relpath, None, binding.local, (binding.local,))


def _seed_definitions(index: AliasIndex, module: ModuleInfo) -> None:
    for function in module.functions:
        # Local function names are terminal aliases for call resolution.
        cardinal = f"{module.module_name}.{function.qualname}"
        if function.class_name is None:
            index.add_edge(module.relpath, None, function.name, (cardinal,))
        if function.class_name is not None:
            index.add_edge(
                module.relpath,
                function.class_name,
                function.name,
                (f"{module.module_name}.{function.qualname}",),
            )


def build_alias_index(graph: ModuleGraph) -> AliasIndex:
    index = AliasIndex()
    for module in graph.modules.values():
        _seed_imports(index, module)
        _seed_definitions(index, module)

    for module in graph.modules.values():
        _walk_scope(index, module.relpath, None, module.root)
        for function in module.functions:
            if function.body is not None:
                _walk_scope(index, module.relpath, function.qualname, function.body)
    return index
