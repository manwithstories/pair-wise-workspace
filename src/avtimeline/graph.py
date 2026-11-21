"""Stage dependency graph with deterministic scheduling.

Stages are nodes; edges encode "must run after".  The graph is validated for
cycles before any work starts, and a cycle is reported as the exact list of
stages (and the tracks that depend on them) on the cycle, so the operator can
find the offending configuration.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable

from .errors import Stage


class GraphError(ValueError):
    """Raised for graph shapes that cannot be scheduled."""


@dataclass(frozen=True)
class Node:
    """One schedulable stage, optionally scoped to a track."""

    stage: Stage
    track: str = ""

    @property
    def name(self) -> str:
        return f"{self.stage.label}:{self.track}" if self.track else self.stage.label

    def __str__(self) -> str:  # pragma: no cover - trivial
        return self.name


@dataclass
class DependencyGraph:
    nodes: dict[str, Node] = field(default_factory=dict)
    edges: dict[str, set[str]] = field(default_factory=dict)

    @classmethod
    def default(cls, tracks: Iterable[str] = ()) -> "DependencyGraph":
        """Build the project's fixed stage graph, optionally per track."""
        graph = cls()
        order = [
            Stage.TIMELINE,
            Stage.PROBE,
            Stage.NORMALIZE,
            Stage.TRANSITION,
            Stage.SEGMENT,
            Stage.ENCODE,
            Stage.MANIFEST,
        ]
        previous: str | None = None
        for stage in order:
            if stage in (Stage.PROBE, Stage.NORMALIZE):
                for track in tracks:
                    node = Node(stage, track)
                    graph.add_node(node)
                    if previous is not None:
                        graph.add_edge(previous, node.name)
                    previous = node.name
            else:
                node = Node(stage)
                graph.add_node(node)
                if previous is not None:
                    graph.add_edge(previous, node.name)
                previous = node.name
        return graph

    def add_node(self, node: Node) -> str:
        if node.name in self.nodes:
            raise GraphError(f"duplicate node {node.name!r}")
        self.nodes[node.name] = node
        self.edges[node.name] = set()
        return node.name

    def add_edge(self, source: str, target: str) -> None:
        if source not in self.nodes:
            raise GraphError(f"unknown source node {source!r}")
        if target not in self.nodes:
            raise GraphError(f"unknown target node {target!r}")
        self.edges[source].add(target)

    def dependencies(self, name: str) -> set[str]:
        return {src for src, targets in self.edges.items() if name in targets}

    def topological_order(self) -> list[str]:
        """Return a deterministic topological order or raise on a cycle."""
        cycle = self.find_cycle()
        if cycle:
            raise GraphError("dependency cycle: " + " -> ".join(cycle))
        indegree = {name: len(self.dependencies(name)) for name in self.nodes}
        ready = sorted(name for name, degree in indegree.items() if degree == 0)
        order: list[str] = []
        while ready:
            current = ready.pop(0)
            order.append(current)
            for target in sorted(self.edges[current]):
                indegree[target] -= 1
                if indegree[target] == 0:
                    ready.append(target)
            ready.sort()
        if len(order) != len(self.nodes):  # pragma: no cover - guarded by find_cycle
            raise GraphError("graph has a cycle")
        return order

    def find_cycle(self) -> list[str] | None:
        """Return one cycle as a node-name list, or ``None``."""
        state: dict[str, int] = {name: 0 for name in self.nodes}
        stack: list[str] = []

        def visit(name: str) -> list[str] | None:
            state[name] = 1
            stack.append(name)
            for target in sorted(self.edges[name]):
                if state[target] == 1:
                    index = stack.index(target)
                    return stack[index:] + [target]
                if state[target] == 0:
                    found = visit(target)
                    if found:
                        return found
            stack.pop()
            state[name] = 2
            return None

        for name in sorted(self.nodes):
            if state[name] == 0:
                found = visit(name)
                if found:
                    return found
        return None

    def stages_on_cycle(self, cycle: Iterable[str]) -> list[str]:
        """Human-readable stage labels for a cycle path."""
        labels: list[str] = []
        for name in cycle:
            node = self.nodes.get(name)
            labels.append(node.stage.label if node else name)
        return labels


def detect_cycle(graph: DependencyGraph) -> str | None:
    """Return a formatted description of the first cycle, if any."""
    cycle = graph.find_cycle()
    if not cycle:
        return None
    stages = graph.stages_on_cycle(cycle)
    tracks = sorted({graph.nodes[name].track for name in cycle if name in graph.nodes and graph.nodes[name].track})
    detail = f"cycle {' -> '.join(cycle)}"
    if tracks:
        detail += f" (tracks: {', '.join(tracks)})"
    if stages:
        detail += f" (stages: {' -> '.join(stages)})"
    return detail


def from_timeline(timeline: Any) -> DependencyGraph:
    """Build the stage graph from a timeline's tracks and their ordering.

    Each track declares optional ``after`` dependencies on other track ids,
    which order normalization.  A cyclic declaration therefore shows up as a
    cycle through ``normalize:<track>`` nodes and is reported with those track
    ids.
    """
    from .timeline import Timeline

    if not isinstance(timeline, Timeline):  # pragma: no cover - defensive
        raise GraphError("from_timeline expects a Timeline")
    graph = DependencyGraph()
    graph.add_node(Node(Stage.TIMELINE))
    previous = Node(Stage.TIMELINE).name
    for track in timeline.tracks.values():
        probe = Node(Stage.PROBE, track.id)
        normalize = Node(Stage.NORMALIZE, track.id)
        graph.add_node(probe)
        graph.add_node(normalize)
        graph.add_edge(previous, probe.name)
        graph.add_edge(probe.name, normalize.name)
    for track in timeline.tracks.values():
        normalize_name = Node(Stage.NORMALIZE, track.id).name
        for dependency in track.after:
            if dependency not in timeline.tracks:
                raise GraphError(
                    f"track {track.id!r} depends on unknown track {dependency!r}"
                )
            graph.add_edge(Node(Stage.NORMALIZE, dependency).name, normalize_name)
    tail_nodes: list[str] = [Node(Stage.NORMALIZE, t.id).name for t in timeline.tracks.values()]
    for stage in (
        Stage.TRANSITION,
        Stage.SEGMENT,
        Stage.ENCODE,
        Stage.MANIFEST,
    ):
        node = Node(stage)
        graph.add_node(node)
        for source in tail_nodes:
            graph.add_edge(source, node.name)
        tail_nodes = [node.name]
    return graph
