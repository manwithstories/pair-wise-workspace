"""Function summaries, SCC fixed-point iteration and taint semantics."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable

from .alias import AliasIndex
from .cache import AstNode
from .callgraph import CallGraph, CallSite
from .policy import (
    Policy,
    environment_match,
    parameter_matches,
    sanitizer_match,
    sink_match,
    source_attribute_match,
    source_call_match,
)
from .resolver import FunctionDef, ModuleGraph, ModuleInfo


@dataclass(frozen=True)
class SourceSite:
    file: str
    line: int


@dataclass(frozen=True)
class SinkSite:
    file: str
    line: int
    name: str


@dataclass(frozen=True)
class Taint:
    kind: str
    source: SourceSite | None = None
    param_owner: str | None = None
    param_index: int = -1
    chain: tuple[str, ...] = ()


@dataclass(frozen=True)
class SinkTarget:
    sink: SinkSite
    chain: tuple[str, ...]


@dataclass(frozen=True)
class ReturnSource:
    source: SourceSite
    chain: tuple[str, ...]


@dataclass(frozen=True)
class FunctionSummary:
    param_sinks: frozenset[tuple[int, SinkTarget]] = frozenset()
    param_returns: frozenset[int] = frozenset()
    source_returns: frozenset[ReturnSource] = frozenset()
    truncated: bool = False

    def merged(self, other: FunctionSummary) -> FunctionSummary:
        return FunctionSummary(
            param_sinks=self.param_sinks | other.param_sinks,
            param_returns=self.param_returns | other.param_returns,
            source_returns=self.source_returns | other.source_returns,
            truncated=self.truncated or other.truncated,
        )


@dataclass(frozen=True)
class TaintPath:
    source: SourceSite
    chain: tuple[str, ...]
    sink: SinkSite

    @property
    def key(self) -> tuple[Any, ...]:
        return (
            self.source.file,
            self.source.line,
            self.chain,
            self.sink.file,
            self.sink.line,
            self.sink.name,
        )


@dataclass
class SummaryResult:
    summaries: dict[str, FunctionSummary] = field(default_factory=dict)
    truncated: bool = False
    diagnostics: list[dict[str, Any]] = field(default_factory=list)
    analyses: int = 0


@dataclass
class EventIndex:
    source_keys: set[tuple[str, int, str]] = field(default_factory=set)
    sink_sites: list[SinkSite] = field(default_factory=list)


def _text(node: AstNode | None) -> str:
    if node is None:
        return ""
    return node.text.decode("utf-8", errors="replace").strip()


def _literal_string(node: AstNode | None) -> str | None:
    if node is None or node.type != "string":
        return None
    text = _text(node)
    for quote in ('"""', "'''", '"', "'"):
        if (
            text.startswith(quote)
            and text.endswith(quote)
            and len(text) >= 2 * len(quote)
        ):
            return text[len(quote) : -len(quote)]
    return None


_TRANSPARENT_NODE_TYPES = {
    "parenthesized_expression",
    "binary_operator",
    "boolean_operator",
    "comparison_operator",
    "unary_operator",
    "not_operator",
    "concatenated_string",
    "string",
    "f_string",
    "interpolation",
    "await",
    "ternary_operator",
    "conditional_expression",
    "list",
    "tuple",
    "set",
    "dictionary",
    "pair",
    "slice",
    "named_expression",
}


def _compose_chain(
    base: tuple[str, ...], suffix: tuple[str, ...], limit: int
) -> tuple[tuple[str, ...], bool]:
    result = list(base)
    truncated = False
    for item in suffix:
        if item in result:
            continue
        if len(result) >= limit:
            truncated = True
            break
        result.append(item)
    return tuple(result), truncated


class Analyzer:
    """Abstract evaluator used by both summary construction and final paths."""

    def __init__(
        self,
        *,
        graph: ModuleGraph,
        aliases: AliasIndex,
        callgraph: CallGraph,
        policy: Policy,
        summaries: dict[str, FunctionSummary] | None = None,
        mode: str = "summary",
    ) -> None:
        self.graph = graph
        self.aliases = aliases
        self.callgraph = callgraph
        self.policy = policy
        self.summaries = summaries or {}
        self.mode = mode
        self.paths: set[TaintPath] = set()
        self.sanitized_keys: set[tuple[Any, ...]] = set()
        self.truncated = False

        self.module: ModuleInfo | None = None
        self.owner: FunctionDef | None = None
        self.owner_id = ""
        self.owner_display = "<module>"
        self.env: dict[str, set[Taint]] = {}
        self.param_returns: set[int] = set()
        self.source_returns: set[ReturnSource] = set()
        self.param_sinks: set[tuple[int, SinkTarget]] = set()

    def analyze_module(self, module: ModuleInfo) -> FunctionSummary:
        self._reset(module, None)
        self._analyze_block(module.root)
        return self._summary()

    def analyze_function(
        self, function: FunctionDef, module: ModuleInfo
    ) -> FunctionSummary:
        self._reset(module, function)
        for index, parameter in enumerate(function.params):
            parameter_name = parameter.name
            self.env.setdefault(parameter_name, set()).add(
                Taint(
                    kind="param", param_owner=function.id, param_index=index, chain=()
                )
            )
            if parameter_matches(self.policy, function.qualname, parameter_name):
                atom = self._source_at(
                    SourceSite(module.relpath, parameter.line), function.qualname
                )
                self.env.setdefault(parameter_name, set()).add(atom)
        if function.body is not None:
            self._analyze_block(function.body)
        return self._summary()

    def run_final_function(
        self,
        function: FunctionDef,
        module: ModuleInfo,
        field_seeds: dict[str, set[Taint]] | None = None,
    ) -> set[TaintPath]:
        self._reset(module, function)
        for index, parameter in enumerate(function.params):
            self.env.setdefault(parameter.name, set()).add(
                Taint(kind="param", param_owner=function.id, param_index=index)
            )
            if parameter_matches(self.policy, function.qualname, parameter.name):
                atom = self._source_at(
                    SourceSite(module.relpath, parameter.line), function.qualname
                )
                self.env.setdefault(parameter.name, set()).add(atom)
        for field_name, values in (field_seeds or {}).items():
            self.env.setdefault(field_name, set()).update(values)
        if function.body is not None:
            self._analyze_block(function.body)
        return set(self.paths)

    def run_final_module(self, module: ModuleInfo) -> set[TaintPath]:
        self._reset(module, None)
        self._analyze_block(module.root)
        return set(self.paths)

    def _reset(self, module: ModuleInfo, owner: FunctionDef | None) -> None:
        self.module = module
        self.owner = owner
        self.owner_id = owner.id if owner else f"{module.relpath}::<module>"
        self.owner_display = owner.qualname if owner else "<module>"
        self.env = {}
        self.param_returns = set()
        self.source_returns = set()
        self.param_sinks = set()
        self.paths = set()
        self.sanitized_keys = set()
        self.truncated = False

    def _summary(self) -> FunctionSummary:
        return FunctionSummary(
            param_sinks=frozenset(self.param_sinks),
            param_returns=frozenset(self.param_returns),
            source_returns=frozenset(self.source_returns),
            truncated=self.truncated,
        )

    def _source_at(self, source: SourceSite, owner_display: str | None = None) -> Taint:
        return Taint(
            kind="source",
            source=source,
            chain=(owner_display or self.owner_display,),
        )

    def _read_env(self, name: str) -> set[Taint]:
        result: set[Taint] = set()
        if not name:
            return result
        module = self.module
        if module is None:
            return result
        aliases = self.aliases.aliases_for(
            module.relpath, self.owner.qualname if self.owner else None, name
        )
        for alias in aliases:
            result.update(self.env.get(alias, ()))
        return result

    def _merge_target(self, target: str, value: Iterable[Taint]) -> None:
        if value:
            self.env.setdefault(target, set()).update(value)

    def _assign_target(self, target: AstNode | None, value: set[Taint]) -> None:
        if target is None:
            return
        if target.type in {"identifier", "attribute"}:
            self._merge_target(_text(target), value)
        elif target.type in {
            "pattern_list",
            "tuple_pattern",
            "list_pattern",
            "expression_list",
            "tuple",
            "list",
        }:
            for child in target.named_children:
                self._assign_target(child, value)
        elif target.type == "subscript":
            self._merge_target(_text(target), value)

    def _merge_envs(
        self, envs: Iterable[dict[str, set[Taint]]]
    ) -> dict[str, set[Taint]]:
        merged: dict[str, set[Taint]] = {}
        for env in envs:
            for name, values in env.items():
                merged.setdefault(name, set()).update(values)
        return merged

    def _analyze_block(self, node: AstNode | None) -> None:
        if node is None:
            return
        for statement in node.named_children:
            if statement.type in {
                "function_definition",
                "class_definition",
                "decorated_definition",
            }:
                continue
            self._analyze_statement(statement)

    def _analyze_statement(self, node: AstNode) -> None:
        node_type = node.type
        if node_type == "expression_statement":
            for child in node.named_children:
                if child.type in {"assignment", "augmented_assignment"}:
                    self._analyze_statement(child)
                else:
                    self.eval_expr(child)
        elif node_type == "assignment":
            right = node.child_by_field_name("right")
            value = self.eval_expr(right) if right is not None else set()
            self._assign_target(node.child_by_field_name("left"), value)
        elif node_type == "augmented_assignment":
            right = node.child_by_field_name("right")
            left = node.child_by_field_name("left")
            value = set(self.eval_expr(right)) if right is not None else set()
            if left is not None:
                value.update(self.eval_expr(left))
                self._assign_target(left, value)
        elif node_type == "return_statement":
            value = set()
            for child in node.named_children:
                value.update(self.eval_expr(child))
            for atom in value:
                if atom.kind == "param" and atom.param_owner == self.owner_id:
                    self.param_returns.add(atom.param_index)
                elif atom.kind == "source" and atom.source is not None:
                    chain, hit_limit = _compose_chain(
                        atom.chain, (), self.policy.budgets.max_chain
                    )
                    self.source_returns.add(ReturnSource(atom.source, chain))
                    self.truncated = self.truncated or hit_limit
        elif node_type == "if_statement":
            condition = node.child_by_field_name("condition")
            if condition is not None:
                self.eval_expr(condition)
            body = node.child_by_field_name("consequence")
            alternative = node.child_by_field_name("alternative")
            base_env = {name: set(values) for name, values in self.env.items()}
            self.env = {name: set(values) for name, values in base_env.items()}
            self._analyze_block(body)
            body_env = self.env
            self.env = {name: set(values) for name, values in base_env.items()}
            if alternative is not None:
                if alternative.type == "else_clause":
                    self._analyze_block(alternative)
                elif alternative.type == "elif_clause":
                    self._analyze_statement(alternative)
                else:
                    self._analyze_block(alternative)
            self.env = self._merge_envs((body_env, self.env))
        elif node_type in {"for_statement", "while_statement"}:
            right = node.child_by_field_name("right")
            if right is not None:
                iterable = self.eval_expr(right)
                self._assign_target(node.child_by_field_name("left"), set(iterable))
            body = node.child_by_field_name("body")
            base_env = {name: set(values) for name, values in self.env.items()}
            self._analyze_block(body)
            self.env = self._merge_envs((base_env, self.env))
        elif node_type == "try_statement":
            body = node.child_by_field_name("body")
            base_env = {name: set(values) for name, values in self.env.items()}
            self._analyze_block(body)
            branches = [self.env]
            for child in node.named_children:
                if child.type in {"except_clause", "else_clause", "finally_clause"}:
                    self.env = {name: set(values) for name, values in base_env.items()}
                    self._analyze_block(child)
                    branches.append(self.env)
            self.env = self._merge_envs(branches)
        elif node_type == "with_statement":
            for child in node.named_children:
                if child.type == "with_clause":
                    for item in child.named_children:
                        self.eval_expr(item)
            body = node.child_by_field_name("body")
            self._analyze_block(body)
        elif node_type == "elif_clause":
            condition = node.child_by_field_name("condition")
            if condition is not None:
                self.eval_expr(condition)
            self._analyze_block(node.child_by_field_name("consequence"))
        elif node_type in {"else_clause", "except_clause", "finally_clause"}:
            self._analyze_block(node)
        elif node_type == "assert_statement":
            for child in node.named_children:
                self.eval_expr(child)
        elif node_type == "raise_statement":
            for child in node.named_children:
                self.eval_expr(child)
        else:
            # Expression-like statements and wrapped blocks are evaluated
            # conservatively so control-flow helpers still see taint.
            for child in node.named_children:
                if child.type in {"block", "expression_statement", "assignment"}:
                    self._analyze_statement(child)
                elif child.type in {
                    "if_statement",
                    "for_statement",
                    "while_statement",
                    "try_statement",
                    "with_statement",
                }:
                    self._analyze_statement(child)

    def eval_expr(self, node: AstNode | None) -> set[Taint]:
        if node is None:
            return set()
        node_type = node.type
        if node_type == "identifier":
            return self._read_env(_text(node))
        if node_type == "attribute":
            return self._eval_attribute(node)
        if node_type == "subscript":
            return self._eval_subscript(node)
        if node_type == "call":
            return self._eval_call(node)
        if node_type in _TRANSPARENT_NODE_TYPES:
            result: set[Taint] = set()
            for child in node.named_children:
                result.update(self.eval_expr(child))
            return result
        if node_type in {
            "lambda",
            "list_comprehension",
            "set_comprehension",
            "dictionary_comprehension",
            "generator_expression",
        }:
            result = set()
            for child in node.named_children:
                if child.type in {"lambda", "function_definition"}:
                    continue
                result.update(self.eval_expr(child))
            return result
        result = set()
        for child in node.named_children:
            result.update(self.eval_expr(child))
        return result

    def _eval_attribute(self, node: AstNode) -> set[Taint]:
        name = _text(node)
        result = self._read_env(name)
        base = node.child_by_field_name("object")
        if base is not None:
            result.update(self.eval_expr(base))
        candidates: set[str] = {name}
        if self.module is not None:
            candidates.update(
                self.aliases.resolve_expr(
                    self.module.relpath,
                    self.owner.qualname if self.owner else None,
                    name,
                )
            )
        match = source_attribute_match(self.policy, tuple(sorted(candidates)))
        if match is not None and self.module is not None:
            source = SourceSite(self.module.relpath, node.start_point[0] + 1)
            result.add(self._source_at(source))
        return result

    def _eval_subscript(self, node: AstNode) -> set[Taint]:
        name = _text(node)
        value = node.child_by_field_name("value")
        index = node.child_by_field_name("subscript")
        result = self._read_env(name)
        if value is not None:
            result.update(self.eval_expr(value))
        if index is not None:
            result.update(self.eval_expr(index))
        if self.module is not None:
            candidates_set: set[str] = {name, _text(value)}
            candidates_set.update(
                self.aliases.resolve_expr(
                    self.module.relpath,
                    self.owner.qualname if self.owner else None,
                    _text(value),
                )
            )
            candidates = tuple(sorted(candidates_set))
            variable = _text(index).strip("'\"") if index is not None else None
            env_match = environment_match(self.policy, candidates, variable)
            attr_match = source_attribute_match(self.policy, candidates)
            if env_match or attr_match:
                result.add(
                    self._source_at(
                        SourceSite(self.module.relpath, node.start_point[0] + 1)
                    )
                )
        return result

    def _eval_call(self, node: AstNode) -> set[Taint]:
        site = (
            self.callgraph.call_at(self.module.relpath, node.start_byte)
            if self.module
            else None
        )
        if site is None:
            result: set[Taint] = set()
            for child in node.named_children:
                result.update(self.eval_expr(child))
            return result
        if self.module is None:
            return set()

        arg_taints = [self.eval_expr(arg.node) for arg in site.args]
        names = (*site.canonical_names, site.expression)
        sink_spec = sink_match(self.policy, names)
        if sink_spec is not None:
            tainted_args: set[Taint] = set()
            for arg, values in zip(site.args, arg_taints):
                if sink_spec.applies_to(arg.index):
                    tainted_args.update(values)
            if tainted_args:
                self._record_sink(site, sink_spec.name, tainted_args)

        sanitizer = sanitizer_match(self.policy, names)
        if sanitizer is not None:
            tainted_input = set().union(*arg_taints) if arg_taints else set()
            if tainted_input:
                signatures = tuple(
                    sorted(
                        (
                            atom.kind,
                            atom.source.file if atom.source else "",
                            atom.source.line if atom.source else -1,
                            atom.param_owner or "",
                            atom.param_index,
                            atom.chain,
                        )
                        for atom in tainted_input
                    )
                )
                self.sanitized_keys.add(
                    (self.module.relpath, node.start_byte, sanitizer, signatures)
                )
            return set()

        source_match = source_call_match(self.policy, names)
        attr_source = source_attribute_match(self.policy, names)
        literal = _literal_string(site.args[0].node) if site.args else None
        env_match = environment_match(self.policy, names, literal)
        if source_match is not None or attr_source is not None or env_match is not None:
            return {
                self._source_at(
                    SourceSite(self.module.relpath, node.start_point[0] + 1)
                )
            }

        if site.internal_ids:
            result: set[Taint] = set()
            for callee_id in site.internal_ids:
                summary = self.summaries.get(callee_id)
                if summary is None:
                    continue
                callee = self.graph.functions.get(callee_id)
                if callee is None:
                    continue
                actual_by_param = self._map_call_arguments(site, callee, arg_taints)
                for param_index, target in summary.param_sinks:
                    actual = actual_by_param.get(param_index, set())
                    for atom in actual:
                        self._consume_param_sink(atom, target)
                for param_index in summary.param_returns:
                    if param_index < len(callee.params) and parameter_matches(
                        self.policy, callee.qualname, callee.params[param_index].name
                    ):
                        # A declared source parameter already contributes a
                        # concrete source through summary.source_returns.  Do
                        # not duplicate the caller's source provenance through
                        # the same always-tainted parameter.
                        continue
                    actual = actual_by_param.get(param_index, set())
                    for atom in actual:
                        if atom.kind == "source":
                            result.add(atom)
                        elif atom.kind == "param" and atom.param_owner == self.owner_id:
                            result.add(atom)
                for returned in summary.source_returns:
                    chain, hit_limit = _compose_chain(
                        returned.chain,
                        (self.owner_display,),
                        self.policy.budgets.max_chain,
                    )
                    self.truncated = self.truncated or hit_limit
                    result.add(
                        Taint(kind="source", source=returned.source, chain=chain)
                    )
            return result
        return set()

    def _map_call_arguments(
        self,
        site: CallSite,
        callee: FunctionDef,
        arg_taints: list[set[Taint]],
    ) -> dict[int, set[Taint]]:
        offset = 0
        if (
            callee.class_name
            and callee.params
            and callee.params[0].name in {"self", "cls"}
        ):
            offset = 1
        mapped: dict[int, set[Taint]] = {}
        positional_index = 0
        for arg, values in zip(site.args, arg_taints):
            if arg.name:
                for param_index, parameter in enumerate(callee.params):
                    if parameter.name == arg.name:
                        mapped.setdefault(param_index, set()).update(values)
                        break
            else:
                param_index = positional_index + offset
                if param_index < len(callee.params):
                    mapped.setdefault(param_index, set()).update(values)
                positional_index += 1
        return mapped

    def _consume_param_sink(self, atom: Taint, target: SinkTarget) -> None:
        if self.mode == "summary":
            if atom.kind == "param" and atom.param_owner == self.owner_id:
                chain, hit_limit = _compose_chain(
                    (self.owner_display,), target.chain, self.policy.budgets.max_chain
                )
                self.param_sinks.add((atom.param_index, SinkTarget(target.sink, chain)))
                self.truncated = self.truncated or hit_limit
            return
        if atom.kind == "source" and atom.source is not None:
            chain, hit_limit = _compose_chain(
                atom.chain, target.chain, self.policy.budgets.max_chain
            )
            self.truncated = self.truncated or hit_limit
            self.paths.add(TaintPath(atom.source, chain, target.sink))

    def _record_sink(self, site: CallSite, sink_name: str, taints: set[Taint]) -> None:
        sink = SinkSite(site.module, site.line, sink_name)
        for atom in taints:
            if self.mode == "summary":
                if atom.kind == "param" and atom.param_owner == self.owner_id:
                    chain, hit_limit = _compose_chain(
                        (self.owner_display,), (), self.policy.budgets.max_chain
                    )
                    self.param_sinks.add((atom.param_index, SinkTarget(sink, chain)))
                    self.truncated = self.truncated or hit_limit
                continue
            if atom.kind == "source" and atom.source is not None:
                chain, hit_limit = _compose_chain(
                    atom.chain, (self.owner_display,), self.policy.budgets.max_chain
                )
                self.truncated = self.truncated or hit_limit
                self.paths.add(TaintPath(atom.source, chain, sink))


def _tarjan_components(edges: dict[str, set[str]]) -> list[list[str]]:
    index = 0
    indices: dict[str, int] = {}
    low: dict[str, int] = {}
    stack: list[str] = []
    on_stack: set[str] = set()
    components: list[list[str]] = []

    def visit(node: str) -> None:
        nonlocal index
        indices[node] = index
        low[node] = index
        index += 1
        stack.append(node)
        on_stack.add(node)
        for target in sorted(edges.get(node, ())):
            if target not in edges:
                continue
            if target not in indices:
                visit(target)
                low[node] = min(low[node], low[target])
            elif target in on_stack:
                low[node] = min(low[node], indices[target])
        if low[node] == indices[node]:
            component: list[str] = []
            while True:
                item = stack.pop()
                on_stack.remove(item)
                component.append(item)
                if item == node:
                    break
            components.append(sorted(component))

    for node in sorted(edges):
        if node not in indices:
            visit(node)
    return sorted(components, key=lambda value: (len(value), value))


def _component_order(
    components: list[list[str]], edges: dict[str, set[str]]
) -> list[list[str]]:
    owner_of: dict[str, int] = {}
    for number, component in enumerate(components):
        for node in component:
            owner_of[node] = number
    dependencies: dict[int, set[int]] = {
        number: set() for number in range(len(components))
    }
    for node, targets in edges.items():
        source_number = owner_of.get(node)
        if source_number is None:
            continue
        for target in targets:
            target_number = owner_of.get(target)
            if target_number is not None and target_number != source_number:
                dependencies[source_number].add(target_number)

    order: list[int] = []
    visited: set[int] = set()

    def visit(number: int) -> None:
        if number in visited:
            return
        visited.add(number)
        for dependency in sorted(dependencies.get(number, ())):
            visit(dependency)
        order.append(number)

    for number in range(len(components)):
        visit(number)
    return [components[number] for number in order]


def build_summaries(
    graph: ModuleGraph,
    aliases: AliasIndex,
    callgraph: CallGraph,
    policy: Policy,
) -> SummaryResult:
    functions = graph.functions
    edges = {
        function_id: set(callgraph.edges.get(function_id, ()))
        for function_id in functions
    }
    components = _component_order(_tarjan_components(edges), edges)
    summaries: dict[str, FunctionSummary] = {
        function_id: FunctionSummary() for function_id in functions
    }
    result = SummaryResult(summaries=summaries)

    analyzer = Analyzer(
        graph=graph,
        aliases=aliases,
        callgraph=callgraph,
        policy=policy,
        summaries=summaries,
        mode="summary",
    )
    for component in components:
        for _round in range(policy.budgets.max_summary_rounds):
            changed = False
            for function_id in component:
                if result.analyses >= policy.budgets.max_iterations:
                    result.truncated = True
                    analyzer.truncated = True
                    break
                function = functions[function_id]
                module = graph.modules[function.module]
                produced = analyzer.analyze_function(function, module)
                result.analyses += 1
                merged = summaries[function_id].merged(produced)
                if merged != summaries[function_id]:
                    summaries[function_id] = merged
                    changed = True
                if analyzer.truncated or produced.truncated:
                    summaries[function_id] = FunctionSummary(
                        param_sinks=summaries[function_id].param_sinks,
                        param_returns=summaries[function_id].param_returns,
                        source_returns=summaries[function_id].source_returns,
                        truncated=True,
                    )
            if result.truncated:
                break
            if not changed:
                break
        else:
            result.truncated = True
            for function_id in component:
                summaries[function_id] = FunctionSummary(
                    param_sinks=summaries[function_id].param_sinks,
                    param_returns=summaries[function_id].param_returns,
                    source_returns=summaries[function_id].source_returns,
                    truncated=True,
                )
        if result.truncated:
            break

    item_count = 0
    for summary in summaries.values():
        item_count += (
            len(summary.param_sinks)
            + len(summary.param_returns)
            + len(summary.source_returns)
        )
    if item_count > policy.budgets.max_summary_items:
        result.truncated = True
    if result.truncated:
        result.diagnostics.append(
            {
                "severity": "warning",
                "code": "summary_truncated",
                "message": "summary fixed point exceeded analysis budget",
            }
        )
    return result


def _walk_named(node: AstNode) -> Iterable[AstNode]:
    stack = [node]
    while stack:
        current = stack.pop()
        yield current
        stack.extend(reversed(current.named_children))


def index_events(
    graph: ModuleGraph,
    callgraph: CallGraph,
    policy: Policy,
    aliases: AliasIndex | None = None,
) -> EventIndex:
    index = EventIndex()
    for site in callgraph.sites:
        names = (*site.canonical_names, site.expression)
        if (
            source_call_match(policy, names)
            or source_attribute_match(policy, names)
            or environment_match(policy, names)
        ):
            index.source_keys.add((site.module, site.start_byte, "call"))
        spec = sink_match(policy, names)
        if spec is not None:
            index.sink_sites.append(SinkSite(site.module, site.line, spec.name))

    for module in graph.modules.values():
        for function in module.functions:
            for parameter in function.params:
                if parameter_matches(policy, function.qualname, parameter.name):
                    index.source_keys.add(
                        (
                            module.relpath,
                            function.node.start_byte + function.params.index(parameter),
                            "param",
                        )
                    )
        for node in _walk_named(module.root):
            if node.type not in {"attribute", "subscript"}:
                continue
            parent = node.parent
            if (
                parent is not None
                and parent.type == "call"
                and parent.child_by_field_name("function") is node
            ):
                continue
            if (
                parent is not None
                and parent.type == "attribute"
                and parent.child_by_field_name("object") is node
            ):
                continue
            if (
                parent is not None
                and parent.type == "subscript"
                and parent.child_by_field_name("value") is node
            ):
                continue
            name = _text(node)
            candidate_values: set[str] = {name}
            if aliases is not None:
                candidate_values.update(
                    aliases.resolve_expr(module.relpath, None, name)
                )
            candidates = tuple(sorted(candidate_values))
            if source_attribute_match(policy, candidates) or environment_match(
                policy, candidates
            ):
                index.source_keys.add((module.relpath, node.start_byte, "read"))

    index.sink_sites = sorted(
        set(index.sink_sites), key=lambda sink: (sink.file, sink.line, sink.name)
    )
    return index
