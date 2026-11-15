"""Mix-graph schema, validation, and deterministic topological sorting."""

from __future__ import annotations

import heapq
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable, Mapping


NODE_TYPES = ("source", "gain", "pan", "sum", "limiter")
SUPPORTED_NODE_TYPES = NODE_TYPES


class GraphError(ValueError):
    """Raised when a mix graph cannot be validated."""


GraphValidationError = GraphError


@dataclass(frozen=True)
class NodeSpec:
    id: str
    type: str
    inputs: tuple[str, ...]
    params: Mapping[str, Any] = field(default_factory=dict)

    def get(self, key: str, default: Any = None) -> Any:
        return self.params.get(key, default)


@dataclass
class Graph:
    nodes: dict[str, NodeSpec]
    output: str
    order: tuple[str, ...]
    consumers: dict[str, tuple[str, ...]]
    master_sr: int | None = None
    block_size: int | None = None
    base_dir: Path | None = None
    raw: Mapping[str, Any] = field(default_factory=dict)

    @property
    def output_node(self) -> NodeSpec:
        return self.nodes[self.output]

    def sources(self) -> list[NodeSpec]:
        return [self.nodes[name] for name in self.order if self.nodes[name].type == "source"]

    def downstream(self, node_id: str) -> tuple[str, ...]:
        return self.consumers.get(node_id, ())


def _node_alias(raw: Mapping[str, Any], index: int) -> str:
    aliases = [
        raw.get("id"),
        raw.get("alias"),
        raw.get("name"),
    ]
    names = [str(value).strip() for value in aliases if value is not None and str(value).strip()]
    if not names:
        raise GraphError(f"node at index {index} has no id/alias")
    return names[0]


def _node_inputs(node_type: str, raw: Mapping[str, Any], node_id: str) -> tuple[str, ...]:
    if node_type == "source":
        return ()
    if node_type == "sum":
        value = raw.get("inputs", raw.get("input"))
        if value is None:
            raise GraphError(f"sum node {node_id} has no inputs")
        if isinstance(value, str):
            inputs = [value]
        elif isinstance(value, (list, tuple)):
            inputs = list(value)
        else:
            raise GraphError(f"sum node {node_id} inputs must be a list of aliases")
    else:
        value = raw.get("input")
        if value is None:
            value = raw.get("inputs")
        if value is None:
            raise GraphError(f"{node_type} node {node_id} has no input")
        if isinstance(value, str):
            inputs = [value]
        elif isinstance(value, (list, tuple)):
            inputs = list(value)
        else:
            raise GraphError(f"{node_type} node {node_id} input must be an alias")
    if not inputs:
        raise GraphError(f"node {node_id} has no inputs")
    result: list[str] = []
    for reference in inputs:
        if not isinstance(reference, str) or not reference.strip():
            raise GraphError(f"node {node_id} contains an invalid input reference")
        result.append(reference.strip())
    if node_type != "sum" and len(result) != 1:
        raise GraphError(f"{node_type} node {node_id} must have exactly one input")
    return tuple(result)


def _topological_order(
    nodes: Mapping[str, NodeSpec], consumers: Mapping[str, tuple[str, ...]]
) -> tuple[str, ...]:
    indices = {name: index for index, name in enumerate(nodes)}
    indegree = {name: len(node.inputs) for name, node in nodes.items()}
    ready = [(indices[name], name) for name, degree in indegree.items() if degree == 0]
    heapq.heapify(ready)
    order: list[str] = []
    while ready:
        _, name = heapq.heappop(ready)
        order.append(name)
        for child in consumers.get(name, ()):
            indegree[child] -= 1
            if indegree[child] == 0:
                heapq.heappush(ready, (indices[child], child))
    if len(order) != len(nodes):
        remaining = [name for name in nodes if name not in set(order)]
        raise GraphError(f"graph contains a cycle involving: {', '.join(remaining)}")
    return tuple(order)


def parse_graph(
    data: Mapping[str, Any], *, base_dir: str | Path | None = None
) -> Graph:
    if not isinstance(data, Mapping):
        raise GraphError("graph root must be a JSON object")
    raw_nodes = data.get("nodes")
    if isinstance(raw_nodes, Mapping):
        node_items: Iterable[tuple[str | None, Any]] = raw_nodes.items()
    elif isinstance(raw_nodes, list):
        node_items = ((None, item) for item in raw_nodes)
    else:
        raise GraphError("graph must contain a nodes list or object")

    nodes: dict[str, NodeSpec] = {}
    alias_to_primary: dict[str, str] = {}
    for index, (mapping_alias, item) in enumerate(node_items):
        if not isinstance(item, Mapping):
            raise GraphError(f"node at index {index} must be an object")
        raw = dict(item)
        if mapping_alias is not None:
            declared = raw.get("id", raw.get("alias", raw.get("name")))
            if declared is None:
                raw["id"] = mapping_alias
        alias = _node_alias(raw, index)
        aliases = {
            str(raw[key]).strip()
            for key in ("id", "alias", "name")
            if raw.get(key) is not None and str(raw.get(key)).strip()
        }
        if mapping_alias is not None:
            aliases.add(str(mapping_alias).strip())
        for name in aliases:
            if name in alias_to_primary:
                raise GraphError(f"duplicate node alias: {name}")
        node_type = str(raw.get("type", "")).strip().lower()
        if node_type not in NODE_TYPES:
            raise GraphError(f"unknown node type {raw.get('type')!r} on node {alias}")
        inputs = _node_inputs(node_type, raw, alias)
        nodes[alias] = NodeSpec(alias, node_type, inputs, raw)
        for name in aliases:
            alias_to_primary[name] = alias

    if not nodes:
        raise GraphError("graph contains no nodes")
    resolved_nodes: dict[str, NodeSpec] = {}
    for node in nodes.values():
        resolved_inputs: list[str] = []
        for reference in node.inputs:
            target = alias_to_primary.get(reference)
            if target is None:
                raise GraphError(
                    f"node {node.id} references missing node {reference!r}"
                )
            if target == node.id:
                raise GraphError(f"node {node.id} cannot reference itself")
            resolved_inputs.append(target)
        resolved_nodes[node.id] = NodeSpec(
            node.id, node.type, tuple(resolved_inputs), node.params
        )
    nodes = resolved_nodes

    consumers_mutable = {name: [] for name in nodes}
    for node in nodes.values():
        for reference in node.inputs:
            consumers_mutable[reference].append(node.id)
    consumers = {name: tuple(children) for name, children in consumers_mutable.items()}
    order = _topological_order(nodes, consumers)
    sinks = [name for name, children in consumers.items() if not children]
    if not sinks:
        raise GraphError("graph has no output node (cycle or closed component)")
    if len(sinks) != 1:
        raise GraphError(f"graph must have exactly one output node; found {len(sinks)}")
    declared_output = data.get("output")
    resolved_output = (
        None if declared_output is None else alias_to_primary.get(str(declared_output))
    )
    if declared_output is not None and resolved_output is None:
        raise GraphError(f"declared output node not found: {declared_output}")
    if resolved_output is not None and resolved_output != sinks[0]:
        raise GraphError(
            f"declared output {declared_output!r} is not the unique output node {sinks[0]!r}"
        )
    master_sr = data.get("master_sr", data.get("sample_rate"))
    block_size = data.get("block_size")
    if block_size is not None and (not isinstance(block_size, int) or block_size <= 0):
        raise GraphError("graph block_size must be a positive integer")
    return Graph(
        nodes=nodes,
        output=sinks[0],
        order=order,
        consumers=consumers,
        master_sr=master_sr,
        block_size=block_size,
        base_dir=None if base_dir is None else Path(base_dir).resolve(),
        raw=data,
    )


def load_graph(path: str | Path) -> Graph:
    path = Path(path)
    try:
        with path.open("r", encoding="utf-8-sig") as handle:
            data = json.load(handle)
    except FileNotFoundError as exc:
        raise GraphError(f"graph file not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise GraphError(f"invalid graph JSON in {path}: {exc}") from exc
    except OSError as exc:
        raise GraphError(f"cannot read graph {path}: {exc}") from exc
    return parse_graph(data, base_dir=path.resolve().parent)


def topological_sort(graph: Graph) -> tuple[str, ...]:
    """Return a validated graph's deterministic topological order."""

    return _topological_order(graph.nodes, graph.consumers)


def validate_graph(value: Mapping[str, Any] | str | Path) -> Graph:
    if isinstance(value, Mapping):
        return parse_graph(value)
    return load_graph(value)
