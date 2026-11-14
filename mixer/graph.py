"""Mixer graph IR, validation and deterministic topological ordering."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable

from .errors import GraphError, ManifestError


TYPE_ALIASES = {
    "track": "read",
    "source": "read",
    "softclip": "limiter",
}
VALID_TYPES = {"read", "gain", "pan", "sum", "trim", "limiter"}
INPUT_PORTS = {
    "read": frozenset(),
    "gain": frozenset({"in"}),
    "pan": frozenset({"in"}),
    "trim": frozenset({"in"}),
    "limiter": frozenset({"in"}),
}


def normalize_type(raw_type: Any) -> str:
    if not isinstance(raw_type, str) or not raw_type:
        raise ManifestError("node type must be a non-empty string", details={"field": "type"})
    normalized = TYPE_ALIASES.get(raw_type.lower(), raw_type.lower())
    if normalized not in VALID_TYPES:
        raise ManifestError(
            f"unknown node type {raw_type!r}",
            details={"type": raw_type, "allowed": sorted(VALID_TYPES)},
        )
    return normalized


@dataclass(frozen=True)
class Node:
    id: str
    type: str
    inputs: dict[str, str] = field(default_factory=dict)
    params: dict[str, Any] = field(default_factory=dict)
    track: str | None = None


def parse_nodes(raw_nodes: Any) -> dict[str, Node]:
    if raw_nodes is None:
        return {}
    if not isinstance(raw_nodes, list) or not raw_nodes:
        raise ManifestError("nodes must be a non-empty list", details={"field": "nodes"})
    nodes: dict[str, Node] = {}
    for index, raw in enumerate(raw_nodes):
        field_name = f"nodes[{index}]"
        if not isinstance(raw, dict):
            raise ManifestError(f"{field_name} must be an object")
        node_id = raw.get("id")
        if not isinstance(node_id, str) or not node_id:
            raise ManifestError(f"{field_name}.id must be a non-empty string")
        if node_id in nodes:
            raise ManifestError(f"duplicate node id {node_id!r}", details={"node_id": node_id})
        node_type = normalize_type(raw.get("type"))
        inputs = raw.get("inputs", {})
        if "input" in raw:
            if inputs:
                raise ManifestError(f"{field_name} cannot contain both input and inputs")
            inputs = {"in": raw["input"]}
        if not isinstance(inputs, dict):
            raise ManifestError(f"{field_name}.inputs must be an object")
        normalized_inputs: dict[str, str] = {}
        for port, source in inputs.items():
            if not isinstance(port, str) or not port:
                raise ManifestError(f"{field_name}.inputs contains an invalid port")
            if not isinstance(source, str) or not source:
                raise ManifestError(f"{field_name}.inputs[{port!r}] must name a node")
            normalized_inputs[port] = source
        track = raw.get("track")
        if track is not None and (not isinstance(track, str) or not track):
            raise ManifestError(f"{field_name}.track must be a non-empty string")
        nodes[node_id] = Node(node_id, node_type, normalized_inputs, dict(raw), track)
    return nodes


@dataclass
class Graph:
    nodes: dict[str, Node]
    output_node: str
    order: tuple[str, ...] = ()

    def __post_init__(self) -> None:
        self.validate()
        self.order = tuple(self._topological_order())

    def validate(self) -> None:
        if self.output_node not in self.nodes:
            raise GraphError(
                f"output node {self.output_node!r} does not exist",
                details={"output_node": self.output_node},
            )
        for node in self.nodes.values():
            self._validate_ports(node)
            for source in node.inputs.values():
                if source not in self.nodes:
                    raise GraphError(
                        f"node {node.id!r} references missing input node {source!r}",
                        details={"node_id": node.id, "source_id": source},
                    )

    @staticmethod
    def _validate_ports(node: Node) -> None:
        connected = set(node.inputs)
        if node.type == "sum":
            if not connected:
                raise GraphError(
                    f"node {node.id!r} has no connected sum inputs",
                    details={"node_id": node.id, "port": "in0"},
                )
            numerals = sorted(int(name[2:]) for name in connected if name.startswith("in") and name[2:].isdigit())
            if len(numerals) != len(connected):
                raise GraphError(
                    f"sum node {node.id!r} uses non-numeric input ports",
                    details={"node_id": node.id, "ports": sorted(connected)},
                )
            expected = set(range(len(numerals)))
            if set(numerals) != expected:
                missing = min(expected - set(numerals)) if expected - set(numerals) else 0
                raise GraphError(
                    f"sum node {node.id!r} has dangling input port in{missing}",
                    details={"node_id": node.id, "port": f"in{missing}"},
                )
            return
        expected_ports = INPUT_PORTS[node.type]
        extras = connected - expected_ports
        if extras:
            raise GraphError(
                f"node {node.id!r} has unknown input ports",
                details={"node_id": node.id, "ports": sorted(extras)},
            )
        missing = expected_ports - connected
        if missing:
            port = sorted(missing)[0]
            raise GraphError(
                f"node {node.id!r} has dangling input port {port!r}",
                details={"node_id": node.id, "port": port},
            )

    def dependents(self) -> dict[str, list[str]]:
        result = {node_id: [] for node_id in self.nodes}
        for node in self.nodes.values():
            for source in node.inputs.values():
                result[source].append(node.id)
        return result

    def _topological_order(self) -> list[str]:
        dependents = self.dependents()
        indegree = {node_id: len(node.inputs) for node_id, node in self.nodes.items()}
        ready = [node_id for node_id in self.nodes if indegree[node_id] == 0]
        index = 0
        order: list[str] = []
        while index < len(ready):
            node_id = ready[index]
            index += 1
            order.append(node_id)
            for child in dependents[node_id]:
                indegree[child] -= 1
                if indegree[child] == 0:
                    ready.append(child)
        if len(order) != len(self.nodes):
            cycle = self._find_cycle()
            raise GraphError(
                "mix graph contains a cycle",
                details={"node_ids": cycle},
            )
        return order

    def _find_cycle(self) -> list[str]:
        state = {node_id: 0 for node_id in self.nodes}
        stack: list[str] = []

        def visit(node_id: str) -> list[str] | None:
            state[node_id] = 1
            stack.append(node_id)
            for source in self.nodes[node_id].inputs.values():
                if state[source] == 0:
                    found = visit(source)
                    if found:
                        return found
                elif state[source] == 1:
                    start = stack.index(source)
                    return stack[start:] + [source]
            stack.pop()
            state[node_id] = 2
            return None

        for node_id in self.nodes:
            if state[node_id] == 0:
                found = visit(node_id)
                if found:
                    return found
        return sorted(self.nodes)


def build_graph(nodes: dict[str, Node], output_node: str) -> Graph:
    return Graph(nodes, output_node)


def track_meter_nodes(graph: Graph, track_ids: Iterable[str]) -> dict[str, str]:
    """Find the last graph node tagged with each track id."""

    result: dict[str, str] = {}
    for node_id in graph.order:
        track = graph.nodes[node_id].track
        if track is not None:
            result[track] = node_id
    return {track_id: result[track_id] for track_id in track_ids if track_id in result}
