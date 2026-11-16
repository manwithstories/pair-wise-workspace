"""Mix graph: track -> bus -> master nodes with topology validation.

Every node carries its own gain stage, so the offline and streaming engines can
evaluate one shared graph. Validation rejects cycles, unreachable nodes and
outputs that never reach the master before a single sample is rendered.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Iterable, Iterator

from .errors import MixerError, argument_error
from .model import Project, Track, db_to_gain

MASTER = "master"
KIND_TRACK = "track"
KIND_BUS = "bus"
KIND_MASTER = "master"


@dataclass
class GraphNode:
    name: str
    kind: str
    inputs: list[str] = field(default_factory=list)
    gain_db: float = 0.0
    track: Track | None = None

    @property
    def gain(self) -> float:
        return db_to_gain(self.gain_db)


@dataclass(init=False)
class TrackNode(GraphNode):
    def __init__(self, track: Track, gain_db: float | None = None) -> None:
        super().__init__(
            name=track.name,
            kind=KIND_TRACK,
            inputs=[],
            gain_db=track.gain_db if gain_db is None else gain_db,
            track=track,
        )


@dataclass(init=False)
class BusNode(GraphNode):
    def __init__(self, name: str, inputs: Iterable[str], gain_db: float = 0.0) -> None:
        super().__init__(name=name, kind=KIND_BUS, inputs=list(inputs), gain_db=gain_db)


@dataclass(init=False)
class MasterNode(GraphNode):
    def __init__(self, inputs: Iterable[str] = (), gain_db: float = 0.0) -> None:
        super().__init__(name=MASTER, kind=KIND_MASTER, inputs=list(inputs), gain_db=gain_db)


@dataclass
class TopologySummary:
    order: list[str]
    layers: list[list[str]]
    track_nodes: int
    bus_nodes: int
    edges: int

    def to_dict(self) -> dict:
        return {
            "order": list(self.order),
            "layers": [list(layer) for layer in self.layers],
            "track_nodes": self.track_nodes,
            "bus_nodes": self.bus_nodes,
            "edges": self.edges,
        }

    def lines(self) -> list[str]:
        lines = [
            f"graph: {self.track_nodes} tracks + {self.bus_nodes} buses -> {MASTER}"
            f" ({self.edges} edges)"
        ]
        for depth, layer in enumerate(self.layers):
            lines.append(f"  layer {depth}: {' '.join(layer)}")
        lines.append(f"  order: {' -> '.join(self.order)}")
        return lines


class MixGraph:
    """A validated DAG of track, bus and master nodes."""

    def __init__(self) -> None:
        self._nodes: dict[str, GraphNode] = {}
        self._master: MasterNode | None = None

    # -- construction -----------------------------------------------------
    @property
    def master(self) -> MasterNode:
        if self._master is None:
            raise argument_error("graph_incomplete", "mix graph has no master node")
        return self._master

    def add(self, node: GraphNode) -> None:
        if node.name in self._nodes:
            raise argument_error("graph_duplicate_node", f"duplicate node {node.name!r}")
        if isinstance(node, MasterNode):
            if self._master is not None:
                raise argument_error("graph_duplicate_master", "mix graph already has a master")
            self._master = node
        self._nodes[node.name] = node

    def add_bus(self, name: str, inputs: Iterable[str], gain_db: float = 0.0) -> BusNode:
        """Add a bus. Forward references are allowed; they resolve on validate."""
        node = BusNode(name, inputs, gain_db)
        self.add(node)
        return node

    def connect(self, upstream: str, downstream: str) -> None:
        node = self._nodes.get(downstream)
        if node is None:
            raise argument_error("graph_unknown_node", f"unknown node {downstream!r}")
        if node.kind not in (KIND_BUS, KIND_MASTER):
            raise argument_error("graph_invalid_edge", f"node {downstream!r} cannot accept inputs")
        if upstream not in self._nodes:
            raise argument_error("graph_unknown_node", f"unknown upstream node {upstream!r}")
        if upstream == downstream:
            raise argument_error("graph_cycle", f"node {upstream!r} cannot feed itself")
        if upstream in node.inputs:
            raise argument_error(
                "graph_duplicate_edge", f"{upstream!r} is already connected to {downstream!r}"
            )
        node.inputs.append(upstream)

    def nodes(self) -> Iterator[GraphNode]:
        return iter(self._nodes.values())

    def __contains__(self, name: str) -> bool:
        return name in self._nodes

    def get(self, name: str) -> GraphNode:
        try:
            return self._nodes[name]
        except KeyError:
            raise argument_error("graph_unknown_node", f"unknown node {name!r}") from None

    # -- validation -------------------------------------------------------
    def validate(self) -> TopologySummary:
        master = self.master
        if not master.inputs:
            raise MixerError(
                "graph_no_output", "master has no connected inputs; nothing would be rendered"
            )
        for node in self._nodes.values():
            if not math.isfinite(node.gain_db):
                raise MixerError(
                    "graph_invalid_gain", f"node {node.name!r} has a non-finite gain",
                    track=node.name if node.kind == KIND_TRACK else None,
                )
            for upstream in node.inputs:
                if upstream not in self._nodes:
                    raise MixerError(
                        "graph_unknown_node",
                        f"{node.name!r} references missing node {upstream!r}",
                    )

        order = self._topological_order()
        layers = self._layers(order)
        reached = self._upstream_of_master()
        orphans = sorted(n for n in self._nodes if n != MASTER and n not in reached)
        if orphans:
            raise MixerError(
                "graph_orphan_node",
                "nodes never reach the master: " + ", ".join(orphans),
                details={"orphans": orphans},
            )

        edges = sum(len(n.inputs) for n in self._nodes.values())
        return TopologySummary(
            order=order,
            layers=layers,
            track_nodes=sum(1 for n in self._nodes.values() if n.kind == KIND_TRACK),
            bus_nodes=sum(1 for n in self._nodes.values() if n.kind == KIND_BUS),
            edges=edges,
        )

    def _topological_order(self) -> list[str]:
        indegree = {name: len(node.inputs) for name, node in self._nodes.items()}
        consumers: dict[str, list[str]] = {name: [] for name in self._nodes}
        for name, node in self._nodes.items():
            for upstream in node.inputs:
                consumers[upstream].append(name)
        ready = sorted(name for name, deg in indegree.items() if deg == 0)
        order: list[str] = []
        while ready:
            name = ready.pop(0)
            order.append(name)
            for consumer in sorted(consumers[name]):
                indegree[consumer] -= 1
                if indegree[consumer] == 0:
                    ready.append(consumer)
        if len(order) != len(self._nodes):
            stuck = sorted(
                name for name, deg in indegree.items() if deg > 0 and self._in_cycle(name)
            ) or sorted(name for name, deg in indegree.items() if deg > 0)
            raise MixerError(
                "graph_cycle",
                "mix graph contains a cycle among: " + ", ".join(stuck),
                details={"nodes": stuck},
            )
        return order

    def _in_cycle(self, start: str) -> bool:
        """True when ``start`` can reach itself through upstream edges."""
        stack = list(self._nodes[start].inputs)
        seen: set[str] = set()
        while stack:
            name = stack.pop()
            if name == start:
                return True
            if name in seen or name not in self._nodes:
                continue
            seen.add(name)
            stack.extend(self._nodes[name].inputs)
        return False

    def _layers(self, order: list[str]) -> list[list[str]]:
        depth: dict[str, int] = {}
        for name in order:
            node = self._nodes[name]
            depth[name] = 0 if not node.inputs else 1 + max(depth[u] for u in node.inputs)
        layers: list[list[str]] = []
        for name in order:
            while len(layers) <= depth[name]:
                layers.append([])
            layers[depth[name]].append(name)
        return layers

    def _upstream_of_master(self) -> set[str]:
        seen: set[str] = set()
        stack = [MASTER]
        while stack:
            name = stack.pop()
            if name in seen or name not in self._nodes:
                continue
            seen.add(name)
            stack.extend(self._nodes[name].inputs)
        return seen

    # -- execution --------------------------------------------------------
    def gain_path(self, track_name: str) -> list[tuple[str, float]]:
        """The (node, gain) chain from a track up to the master.

        A track feeding several buses has one path per bus; each path's gain
        product is applied to that branch, which is how bus summing works.
        """
        path = self._find_path(track_name, set())
        if path is None:
            raise MixerError(
                "graph_unconnected_output",
                f"track {track_name!r} has no path to the master",
                track=track_name,
            )
        return path

    def _find_path(self, name: str, seen: set[str]) -> list[tuple[str, float]] | None:
        if name in seen:
            return None
        seen = seen | {name}
        node = self._nodes[name]
        if node.kind == KIND_MASTER:
            return [(MASTER, node.gain)]
        consumers = sorted(
            (n for n in self._nodes.values() if name in n.inputs), key=lambda n: n.name
        )
        for consumer in consumers:
            tail = self._find_path(consumer.name, seen)
            if tail is not None:
                return [(name, node.gain), *tail]
        return None

    def consumer_buses(self, track_name: str) -> list[str]:
        """Bus names directly fed by a track (used for bus-level gain)."""
        return sorted(
            n.name for n in self._nodes.values() if n.kind == KIND_BUS and track_name in n.inputs
        )

    def unconnected_outputs(self) -> list[str]:
        reached = self._upstream_of_master()
        return sorted(
            n.name for n in self._nodes.values() if n.kind == KIND_TRACK and n.name not in reached
        )


def build_default_graph(
    project: Project,
    bus_map: dict[str, list[str]] | None = None,
    bus_gain_db: dict[str, float] | None = None,
) -> MixGraph:
    """Build track -> bus(es) -> master for a parsed project.

    ``bus_map`` groups track names under bus names; unmapped tracks connect
    straight to the master. Bus gain defaults to unity (0 dB).
    """
    graph = MixGraph()
    graph.add(MasterNode())
    names = {track.name for track in project.tracks}
    for track in project.tracks:
        graph.add(TrackNode(track))

    grouped: set[str] = set()
    for bus_name, members in (bus_map or {}).items():
        if not members:
            raise argument_error("graph_empty_bus", f"bus {bus_name!r} has no inputs")
        if bus_name == MASTER:
            raise argument_error("graph_invalid_bus", "a bus cannot be named 'master'")
        for member in members:
            if member not in names:
                raise argument_error(
                    "graph_unknown_node", f"bus {bus_name!r} names unknown track {member!r}"
                )
            grouped.add(member)
        graph.add_bus(bus_name, members)
        graph.connect(bus_name, MASTER)

    for name in sorted(names - grouped):
        graph.connect(name, MASTER)
    for bus_name, gain in (bus_gain_db or {}).items():
        if bus_name not in graph:
            raise argument_error("graph_unknown_node", f"unknown bus {bus_name!r}")
        graph.get(bus_name).gain_db = float(gain)
    return graph
