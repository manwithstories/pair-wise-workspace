"""Directed processing graph with topology validation and cycle detection."""

from __future__ import annotations

from dataclasses import dataclass, field
from collections import deque
from collections.abc import Iterable

from .errors import GraphError


NODE_TYPES = frozenset({"source", "gain", "bus", "sum", "resample", "output"})


@dataclass
class Node:
    id: str
    type: str
    params: dict = field(default_factory=dict)


@dataclass(frozen=True)
class Edge:
    source: str
    target: str


class Graph:
    """A compact single-port graph.

    Sum nodes accept one or more incoming edges. Every other node has a fixed
    arity, which makes dangling ports and accidental fan-in detectable before
    any audio work starts.
    """

    def __init__(self) -> None:
        self.nodes: dict[str, Node] = {}
        self.edges: list[Edge] = []
        self._incoming: dict[str, list[str]] = {}
        self._outgoing: dict[str, list[str]] = {}

    def add_node(self, node_id: str, node_type: str, params: dict | None = None) -> Node:
        if not isinstance(node_id, str) or not node_id:
            raise GraphError("node id must be a non-empty string", "graph.nodes.id")
        if node_id in self.nodes:
            raise GraphError(f"duplicate node id {node_id!r}", f"nodes.{node_id}")
        if params is not None and not isinstance(params, dict):
            raise GraphError("node params must be a mapping", f"nodes.{node_id}")
        node = Node(node_id, node_type, dict(params or {}))
        self.nodes[node_id] = node
        self._incoming[node_id] = []
        self._outgoing[node_id] = []
        return node

    def connect(self, source: str, target: str) -> Edge:
        if source not in self.nodes:
            raise GraphError(f"unknown source node {source!r}", f"edges.{source}")
        if target not in self.nodes:
            raise GraphError(f"unknown target node {target!r}", f"edges.{target}")
        edge = Edge(source, target)
        if edge in self.edges:
            raise GraphError(
                f"duplicate edge {source!r} -> {target!r}", f"edges.{source}"
            )
        self.edges.append(edge)
        self._incoming[target].append(source)
        self._outgoing[source].append(target)
        return edge

    def inputs(self, node_id: str) -> tuple[str, ...]:
        return tuple(self._incoming.get(node_id, ()))

    def outputs(self, node_id: str) -> tuple[str, ...]:
        return tuple(self._outgoing.get(node_id, ()))

    def validate(self) -> list[str]:
        if not self.nodes:
            raise GraphError("graph has no nodes", "graph.nodes")
        for node in self.nodes.values():
            if node.type not in NODE_TYPES:
                raise GraphError(
                    f"unknown node type {node.type!r}; expected one of "
                    + ", ".join(sorted(NODE_TYPES)),
                    f"nodes.{node.id}.type",
                )

        for node in self.nodes.values():
            inputs = self._incoming[node.id]
            outputs = self._outgoing[node.id]
            if node.type == "source":
                if inputs:
                    raise GraphError("source nodes cannot have inputs", f"nodes.{node.id}.input")
                if not outputs:
                    raise GraphError("source output is dangling", f"nodes.{node.id}.output")
            elif node.type == "sum":
                if not inputs:
                    raise GraphError("sum has no connected inputs", f"nodes.{node.id}.input")
                if not outputs:
                    raise GraphError("sum output is dangling", f"nodes.{node.id}.output")
            elif node.type == "output":
                if len(inputs) != 1:
                    raise GraphError(
                        "output must have exactly one input", f"nodes.{node.id}.input"
                    )
                if outputs:
                    raise GraphError("output cannot feed another node", f"nodes.{node.id}.output")
            else:
                if len(inputs) != 1:
                    raise GraphError(
                        f"{node.type} must have exactly one input", f"nodes.{node.id}.input"
                    )
                if not outputs:
                    raise GraphError("node output is dangling", f"nodes.{node.id}.output")
            if node.type != "sum" and len(inputs) > 1:
                raise GraphError(
                    f"{node.type} does not accept multiple inputs", f"nodes.{node.id}.input"
                )

        order = self.topological_order()
        outputs = [node.id for node in self.nodes.values() if node.type == "output"]
        if not outputs:
            raise GraphError("graph has no output node", "graph.outputs")
        return order

    def topological_order(self) -> list[str]:
        indegree = {node_id: len(self._incoming[node_id]) for node_id in self.nodes}
        queue = deque(node_id for node_id in self.nodes if indegree[node_id] == 0)
        order: list[str] = []
        while queue:
            node_id = queue.popleft()
            order.append(node_id)
            for target in self._outgoing[node_id]:
                indegree[target] -= 1
                if indegree[target] == 0:
                    queue.append(target)
        if len(order) != len(self.nodes):
            cyclic = sorted(node_id for node_id, degree in indegree.items() if degree)
            raise GraphError(
                "cycle detected involving " + ", ".join(cyclic), "graph.edges"
            )
        return order

    def ancestors(self, node_id: str) -> set[str]:
        seen: set[str] = set()
        stack = list(self._incoming.get(node_id, ()))
        while stack:
            current = stack.pop()
            if current in seen:
                continue
            seen.add(current)
            stack.extend(self._incoming.get(current, ()))
        return seen

    def find_by_type(self, node_type: str) -> list[Node]:
        return [node for node in self.nodes.values() if node.type == node_type]

    def from_manifest(self, nodes: Iterable[dict], edges: Iterable[dict]) -> "Graph":
        for index, raw in enumerate(nodes):
            if not isinstance(raw, dict):
                raise GraphError("node entry must be a mapping", f"nodes[{index}]")
            try:
                node_id = raw["id"]
                node_type = raw["type"]
            except KeyError as exc:
                raise GraphError(
                    f"node is missing {exc.args[0]!r}", f"nodes[{index}]"
                ) from exc
            params = {key: value for key, value in raw.items() if key not in {"id", "type"}}
            self.add_node(node_id, node_type, params)
        for index, raw in enumerate(edges):
            if not isinstance(raw, dict):
                raise GraphError("edge entry must be a mapping", f"edges[{index}]")
            try:
                source, target = raw["from"], raw["to"]
            except KeyError as exc:
                raise GraphError(
                    f"edge is missing {exc.args[0]!r}", f"edges[{index}]"
                ) from exc
            self.connect(source, target)
        return self

