"""Dependency graph: node construction, cycle detection, processing order.

Nodes come from a validated :class:`~mediacore.timeline.Timeline`:

* ``clip:<id>`` for every explicit clip,
* ``transition:<id>`` for every transition, depending on both component
  clips.

Two properties are required for recovery to be useful:

* **Cycle detection before any work.**  A cyclic graph is reported with the
  exact cycle, never partially processed.
* **Isolation.**  When an upstream node fails, only its transitive dependants
  are skipped; independent tracks keep going.  :meth:`Plan.ready_nodes` and
  :meth:`Plan.fail` implement that policy, and the pipeline never has to reason
  about it.

The plan is deterministic: nodes are emitted in a stable topological order
(track id, then source position) so a resumed run reproduces the same order.
"""

from __future__ import annotations

from collections import deque
from dataclasses import dataclass, field

from .errors import GraphError
from .timeline import Timeline

__all__ = ["Node", "Plan", "build_plan"]


@dataclass(frozen=True)
class Node:
    """One unit of processing."""

    id: str
    kind: str  # "clip" | "transition"
    track: str
    deps: tuple[str, ...] = ()
    # Stable ordering key within the timeline.
    sort_key: tuple[int, str] = (0, "")

    def to_dict(self) -> dict[str, object]:
        return {
            "id": self.id,
            "kind": self.kind,
            "track": self.track,
            "deps": list(self.deps),
        }


@dataclass
class Plan:
    """A validated dependency graph plus mutable run state."""

    nodes: dict[str, Node]
    order: list[str]
    _state: dict[str, str] = field(default_factory=dict)
    _failed: dict[str, str] = field(default_factory=dict)

    def __post_init__(self) -> None:
        self._state = {node_id: "pending" for node_id in self.nodes}

    # -- queries ------------------------------------------------------------
    def node(self, node_id: str) -> Node:
        return self.nodes[node_id]

    def state(self, node_id: str) -> str:
        return self._state[node_id]

    def dependants(self, node_id: str) -> list[str]:
        return [
            other.id
            for other in self.nodes.values()
            if node_id in other.deps
        ]

    def transitive_dependants(self, node_id: str) -> list[str]:
        seen: set[str] = set()
        queue = deque(self.dependants(node_id))
        while queue:
            current = queue.popleft()
            if current in seen:
                continue
            seen.add(current)
            queue.extend(self.dependants(current))
        return sorted(seen)

    def ready_nodes(self) -> list[str]:
        """Pending nodes whose dependencies have all completed, in plan order.

        Nodes blocked by a failed/skipped dependency are transitioned to
        ``skipped`` in the same call (to a fixpoint), so a caller looping on
        :meth:`ready_nodes` and :meth:`mark_done` always terminates without
        needing its own isolation logic.
        """
        while True:
            ready: list[str] = []
            newly_skipped = False
            for node_id in self.order:
                if self._state[node_id] != "pending":
                    continue
                node = self.nodes[node_id]
                if any(self._state[dep] in ("failed", "skipped") for dep in node.deps):
                    self._state[node_id] = "skipped"
                    self._skipped_reason(node_id)
                    newly_skipped = True
                elif all(self._state[dep] == "done" for dep in node.deps):
                    ready.append(node_id)
            if not newly_skipped:
                return ready

    def _skipped_reason(self, node_id: str) -> None:
        blocked = [
            dep for dep in self.nodes[node_id].deps
            if self._state.get(dep) in ("failed", "skipped")
        ]
        self._failed[node_id] = f"blocked_by:{','.join(blocked)}"

    def mark_done(self, node_id: str) -> None:
        self._require_pending(node_id)
        self._state[node_id] = "done"

    def fail(self, node_id: str, reason: str) -> list[str]:
        """Mark a node failed and isolate its dependants; returns skipped ids."""
        self._require_pending(node_id)
        self._state[node_id] = "failed"
        self._failed[node_id] = reason
        skipped = []
        for candidate in self.transitive_dependants(node_id):
            if self._state[candidate] == "pending":
                self._state[candidate] = "skipped"
                self._skipped_reason(candidate)
                skipped.append(candidate)
        return skipped

    def _require_pending(self, node_id: str) -> None:
        if node_id not in self.nodes:
            raise GraphError("unknown node", code="graph.unknown_node", node=node_id)
        if self._state[node_id] != "pending":
            raise GraphError(
                "node is not pending",
                code="graph.bad_state",
                node=node_id,
                state=self._state[node_id],
            )

    @property
    def complete(self) -> bool:
        return all(v in ("done", "failed", "skipped") for v in self._state.values())

    def states(self) -> dict[str, str]:
        return dict(self._state)

    def failure_reasons(self) -> dict[str, str]:
        return dict(self._failed)

    def to_dict(self) -> dict[str, object]:
        return {
            "nodes": [self.nodes[n].to_dict() for n in self.order],
            "states": self.states(),
            "failure_reasons": self.failure_reasons(),
        }


def build_plan(timeline: Timeline) -> Plan:
    """Build and validate the plan for ``timeline``.

    Raises :class:`~mediacore.errors.GraphError` naming the cycle when the
    dependency graph is cyclic.
    """
    nodes: dict[str, Node] = {}
    for clip in timeline.clips.values():
        node_id = f"clip:{clip.id}"
        nodes[node_id] = Node(
            id=node_id,
            kind="clip",
            track=clip.track,
            deps=(),
            sort_key=(clip.start_us, clip.id),
        )
    for transition in timeline.transitions.values():
        node_id = transition.node_id
        nodes[node_id] = Node(
            id=node_id,
            kind="transition",
            track=timeline.clips[transition.left].track,
            deps=(
                f"clip:{transition.left}",
                f"clip:{transition.right}",
                *(f"transition:{dep}" for dep in transition.after),
            ),
            sort_key=(timeline.clips[transition.left].start_us, transition.id),
        )

    order = _topological_order(nodes)
    return Plan(nodes=nodes, order=order)


def _topological_order(nodes: dict[str, Node]) -> list[str]:
    """Kahn's algorithm with a deterministic tie-break."""
    indegree = {node_id: 0 for node_id in nodes}
    for node in nodes.values():
        for dep in node.deps:
            if dep not in nodes:
                raise GraphError(
                    "node depends on unknown node",
                    code="graph.missing_dependency",
                    node=node.id,
                    dependency=dep,
                )
            indegree[node.id] += 1

    def key(node_id: str) -> tuple[str, int, str]:
        node = nodes[node_id]
        return (node.track, node.sort_key[0], node.sort_key[1])

    ready = sorted((n for n, d in indegree.items() if d == 0), key=key)
    order: list[str] = []
    while ready:
        current = ready.pop(0)
        order.append(current)
        newly_ready = []
        for other in nodes.values():
            if current in other.deps:
                indegree[other.id] -= 1
                if indegree[other.id] == 0:
                    newly_ready.append(other.id)
        ready.extend(newly_ready)
        ready.sort(key=key)

    if len(order) != len(nodes):
        cycle = _find_cycle(nodes, set(order))
        raise GraphError(
            "dependency cycle detected",
            code="graph.cycle",
            cycle=cycle,
            nodes=len(nodes),
            ordered=len(order),
        )
    return order


def _find_cycle(nodes: dict[str, Node], ordered: set[str]) -> list[str]:
    """Return one concrete cycle among the nodes Kahn could not order."""
    remaining = {n: [d for d in node.deps if d not in ordered] for n, node in nodes.items()
                 if n not in ordered}
    for start in sorted(remaining):
        path: list[str] = []
        seen: dict[str, int] = {}
        current = start
        while current not in seen and current in remaining:
            seen[current] = len(path)
            path.append(current)
            deps = remaining[current]
            current = deps[0] if deps else ""
        if current in seen:
            return path[seen[current] :] + [current]
    return sorted(remaining)
