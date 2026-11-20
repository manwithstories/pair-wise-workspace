"""Explicit media intervals, track dependency graphs and topological planning.

Every track contributes one or more half-open intervals in 90 kHz ticks.  An
interval may depend on others (for example a camera track that needs the
program audio to anchor it, or a boundary segment that needs its keyframe
neighbour).  The planner validates the graph before any recovery runs:

* cycles are rejected,
* dependencies on unknown tracks are rejected,
* overlapping intervals on one track are rejected.

The resulting plan lists intervals in topological order, so recovery can
satisfy every dependency before its dependent is rebuilt.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Iterable, Mapping, Sequence

from .errors import Gap, PlanError

MAX_INTERVALS = 20_000


@dataclass(frozen=True)
class Interval:
    """A half-open media interval ``[start, end)`` on one track."""

    interval_id: str
    track_id: str
    start_ticks: int
    end_ticks: int
    kind: str = "segment"
    depends_on: tuple[str, ...] = ()

    def __post_init__(self) -> None:
        if not self.interval_id:
            raise PlanError("interval_id cannot be empty")
        if not self.track_id:
            raise PlanError(f"{self.interval_id}: track_id cannot be empty")
        if self.start_ticks < 0:
            raise PlanError(
                f"{self.interval_id}: negative start {self.start_ticks}"
            )
        if self.end_ticks <= self.start_ticks:
            raise PlanError(
                f"{self.interval_id}: empty interval "
                f"[{self.start_ticks}, {self.end_ticks})"
            )
        if not self.kind:
            raise PlanError(f"{self.interval_id}: kind cannot be empty")

    @property
    def duration_ticks(self) -> int:
        return self.end_ticks - self.start_ticks

    def overlaps(self, other: "Interval") -> bool:
        return (
            self.track_id == other.track_id
            and self.start_ticks < other.end_ticks
            and other.start_ticks < self.end_ticks
        )

    def to_dict(self) -> dict[str, object]:
        return {
            "interval_id": self.interval_id,
            "track_id": self.track_id,
            "start_ticks": self.start_ticks,
            "end_ticks": self.end_ticks,
            "duration_ticks": self.duration_ticks,
            "kind": self.kind,
            "depends_on": list(self.depends_on),
        }


@dataclass(frozen=True)
class Dependency:
    """A directed dependency between two intervals."""

    dependent: str
    prerequisite: str

    def to_dict(self) -> dict[str, str]:
        return {"dependent": self.dependent, "prerequisite": self.prerequisite}


@dataclass
class Timeline:
    intervals: dict[str, Interval] = field(default_factory=dict)
    dependencies: list[Dependency] = field(default_factory=list)

    def add(self, interval: Interval) -> None:
        if len(self.intervals) >= MAX_INTERVALS:
            raise PlanError(f"more than {MAX_INTERVALS} intervals requested")
        if interval.interval_id in self.intervals:
            raise PlanError(f"duplicate interval id {interval.interval_id!r}")
        self.intervals[interval.interval_id] = interval
        for prerequisite in interval.depends_on:
            self.dependencies.append(Dependency(interval.interval_id, prerequisite))

    def intervals_for(self, track_id: str) -> list[Interval]:
        return sorted(
            (item for item in self.intervals.values() if item.track_id == track_id),
            key=lambda item: item.start_ticks,
        )

    def tracks(self) -> list[str]:
        return sorted({item.track_id for item in self.intervals.values()})

    def validate(self) -> None:
        """Reject missing dependencies, cross-track dependencies and overlaps."""

        known = set(self.intervals)
        for dependency in self.dependencies:
            if dependency.prerequisite not in known:
                raise PlanError(
                    f"{dependency.dependent} depends on unknown interval "
                    f"{dependency.prerequisite!r}"
                )
            if dependency.dependent == dependency.prerequisite:
                raise PlanError(
                    f"{dependency.dependent} depends on itself"
                )
        for track_id in self.tracks():
            items = self.intervals_for(track_id)
            for earlier, later in zip(items, items[1:]):
                if earlier.overlaps(later):
                    raise PlanError(
                        f"{track_id}: intervals {earlier.interval_id} and "
                        f"{later.interval_id} overlap "
                        f"([{earlier.start_ticks}, {earlier.end_ticks}) vs "
                        f"[{later.start_ticks}, {later.end_ticks}))"
                    )
                if earlier.end_ticks > later.start_ticks:
                    raise PlanError(
                        f"{track_id}: intervals {earlier.interval_id} and "
                        f"{later.interval_id} are not sorted by start"
                    )
        self.topological_order()

    def adjacency(self) -> dict[str, list[str]]:
        """Dependent -> prerequisites, filtered to known intervals."""

        graph: dict[str, list[str]] = {key: [] for key in self.intervals}
        for dependency in self.dependencies:
            if dependency.prerequisite in graph:
                graph[dependency.dependent].append(dependency.prerequisite)
        return graph

    def topological_order(self) -> list[str]:
        """Kahn's algorithm over prerequisites; raises on a cycle."""

        graph = self.adjacency()
        indegree = {node: 0 for node in graph}
        dependents: dict[str, list[str]] = {node: [] for node in graph}
        for node, prerequisites in graph.items():
            indegree[node] = len(prerequisites)
            for prerequisite in prerequisites:
                dependents[prerequisite].append(node)
        ready = sorted(node for node, degree in indegree.items() if degree == 0)
        order: list[str] = []
        while ready:
            node = ready.pop(0)
            order.append(node)
            for dependent in sorted(dependents[node]):
                indegree[dependent] -= 1
                if indegree[dependent] == 0:
                    ready.append(dependent)
            ready.sort()
        if len(order) != len(graph):
            cycle = _find_cycle(graph)
            raise PlanError(f"dependency cycle detected: {' -> '.join(cycle)}")
        return order

    def recovery_order(self) -> list[Interval]:
        return [self.intervals[key] for key in self.topological_order()]

    def to_dict(self) -> dict[str, object]:
        return {
            "intervals": [item.to_dict() for item in self.intervals.values()],
            "dependencies": [item.to_dict() for item in self.dependencies],
            "order": self.topological_order(),
        }


def _find_cycle(graph: Mapping[str, Sequence[str]]) -> list[str]:
    """Return one concrete cycle for the error message."""

    colour: dict[str, int] = {node: 0 for node in graph}
    stack: list[str] = []

    def visit(node: str) -> list[str] | None:
        colour[node] = 1
        stack.append(node)
        for prerequisite in graph[node]:
            if colour.get(prerequisite, 0) == 0:
                found = visit(prerequisite)
                if found:
                    return found
            elif colour[prerequisite] == 1:
                start = stack.index(prerequisite)
                return stack[start:] + [prerequisite]
        stack.pop()
        colour[node] = 2
        return None

    for node in sorted(graph):
        if colour[node] == 0:
            found = visit(node)
            if found:
                return found
    return []


def build_timeline(tracks: Iterable[Sequence[Interval]]) -> Timeline:
    """Assemble and validate a timeline from per-track interval lists."""

    timeline = Timeline()
    for intervals in tracks:
        for interval in intervals:
            timeline.add(interval)
    timeline.validate()
    return timeline


def gaps_between(
    intervals: Sequence[Interval], track_id: str, reason: str = "gap"
) -> list[Gap]:
    """Report gaps in coverage between consecutive intervals on one track."""

    ordered = sorted(intervals, key=lambda item: item.start_ticks)
    gaps: list[Gap] = []
    for earlier, later in zip(ordered, ordered[1:]):
        if later.start_ticks > earlier.end_ticks:
            gaps.append(
                Gap(track_id, earlier.end_ticks, later.start_ticks, reason)
            )
    return gaps
