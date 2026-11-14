"""Mix graph model, validation, and deterministic topological ordering."""
from __future__ import annotations

from dataclasses import dataclass, field
import math
from typing import Any, Iterable

from .automation import Automation, AutomationError


class GraphError(ValueError):
    """Raised when a project graph is structurally invalid."""


@dataclass(frozen=True)
class Send:
    destination: str
    gain_db: float = 0.0


@dataclass
class Track:
    name: str
    source: str
    gain_db: float = 0.0
    mute: bool = False
    solo: bool = False
    automation: Automation | None = None
    sends: list[Send] = field(default_factory=list)


@dataclass
class Bus:
    name: str
    gain_db: float = 0.0
    mute: bool = False
    automation: Automation | None = None
    sends: list[Send] = field(default_factory=list)


@dataclass
class Master:
    name: str = "master"
    gain_db: float = 0.0
    mute: bool = False
    automation: Automation | None = None


def _finite(value: Any, label: str) -> float:
    try:
        number = float(value)
    except (TypeError, ValueError) as exc:
        raise GraphError(f"{label} must be a number") from exc
    if not math.isfinite(number):
        raise GraphError(f"{label} must be finite")
    return number


def _sends(raw: Any, node: str, shorthand_bus: Any = None) -> list[Send]:
    entries: list[tuple[str, Any]] = []
    if shorthand_bus:
        entries.append((str(shorthand_bus), 0.0))
    if raw is not None:
        if not isinstance(raw, list):
            raise GraphError(f"node '{node}' sends must be a list")
        for index, item in enumerate(raw):
            if not isinstance(item, dict):
                raise GraphError(f"send {index} on node '{node}' must be an object")
            destination = item.get("bus", item.get("destination"))
            if not destination:
                raise GraphError(f"send {index} on node '{node}' has no destination")
            entries.append((str(destination), item.get("gain_db", 0.0)))
    result: list[Send] = []
    seen: set[str] = set()
    for destination, gain in entries:
        if destination in seen:
            raise GraphError(f"node '{node}' sends twice to '{destination}'")
        seen.add(destination)
        result.append(Send(destination, _finite(gain, f"send gain on node '{node}'")))
    return result


def _automation(raw: Any, node: str) -> Automation | None:
    if raw is None:
        return None
    try:
        return Automation.from_json(raw, node)
    except AutomationError as exc:
        raise GraphError(str(exc)) from exc


class MixGraph:
    def __init__(self, sample_rate: int, tracks: Iterable[Track], buses: Iterable[Bus], master: Master, channels: int = 2):
        self.sample_rate = int(sample_rate)
        self.channels = int(channels)
        self.tracks = list(tracks)
        self.buses = list(buses)
        self.master = master
        self._validate()
        self.bus_order = self._topological_bus_order()
        self.process_order = [t.name for t in self.tracks] + self.bus_order + [self.master.name]

    @property
    def solo_active(self) -> bool:
        return any(track.solo for track in self.tracks)

    def node(self, name: str) -> Track | Bus | Master:
        for track in self.tracks:
            if track.name == name:
                return track
        for bus in self.buses:
            if bus.name == name:
                return bus
        if self.master.name == name:
            return self.master
        raise GraphError(f"unknown node '{name}'")

    def track_factor(self, track: Track, automation_gain: float) -> float:
        if track.mute:
            return 0.0
        static = 10.0 ** (track.gain_db / 20.0)
        solo = 0.0 if self.solo_active and not track.solo else 1.0
        return static * automation_gain * solo

    def report_track_factor(self, track: Track, automation_gain: float) -> float:
        """Track contribution without the solo gate; solo never hides details."""
        if track.mute:
            return 0.0
        return (10.0 ** (track.gain_db / 20.0)) * automation_gain

    def _validate(self) -> None:
        if self.sample_rate <= 0:
            raise GraphError("project sample_rate must be positive")
        if self.channels not in (1, 2):
            raise GraphError("project channels must be 1 or 2")
        names: set[str] = set()
        for node in [*self.tracks, *self.buses]:
            if not node.name:
                raise GraphError("node name must not be empty")
            if node.name in names:
                raise GraphError(f"duplicate node name '{node.name}'")
            names.add(node.name)
        if not self.master.name:
            raise GraphError("master node name must not be empty")
        if self.master.name in names:
            raise GraphError(f"duplicate node name '{self.master.name}'")
        names.add(self.master.name)

        bus_names = {bus.name for bus in self.buses}
        routable = bus_names | {self.master.name}
        for node in [*self.tracks, *self.buses]:
            for send in node.sends:
                if send.destination not in routable:
                    raise GraphError(
                        f"node '{node.name}' sends to missing bus '{send.destination}'"
                    )
        self._topological_bus_order()
        for bus in self.buses:
            if not self._can_reach_master(bus.name):
                raise GraphError(f"dangling bus '{bus.name}': no path to master")

    def _adjacency(self) -> dict[str, list[str]]:
        adjacency = {bus.name: [] for bus in self.buses}
        for bus in self.buses:
            for send in bus.sends:
                if send.destination in adjacency:
                    adjacency[bus.name].append(send.destination)
        return adjacency

    def _can_reach_master(self, start: str) -> bool:
        adjacency = {
            bus.name: [send.destination for send in bus.sends]
            for bus in self.buses
        }
        stack = [start]
        seen: set[str] = set()
        while stack:
            current = stack.pop()
            if current in seen:
                continue
            seen.add(current)
            for destination in adjacency.get(current, []):
                if destination == self.master.name:
                    return True
                stack.append(destination)
        return False

    def _topological_bus_order(self) -> list[str]:
        adjacency = self._adjacency()
        indegree = {bus.name: 0 for bus in self.buses}
        for destinations in adjacency.values():
            for destination in destinations:
                indegree[destination] += 1
        ready = [bus.name for bus in self.buses if indegree[bus.name] == 0]
        order: list[str] = []
        while ready:
            current = ready.pop(0)
            order.append(current)
            for destination in adjacency[current]:
                indegree[destination] -= 1
                if indegree[destination] == 0:
                    ready.append(destination)
        if len(order) != len(self.buses):
            cycle = self._cycle_path(adjacency)
            raise GraphError(f"cycle detected: {' -> '.join(cycle)}")
        return order

    def _cycle_path(self, adjacency: dict[str, list[str]]) -> list[str]:
        state: dict[str, int] = {}
        stack: list[str] = []

        def visit(node: str) -> list[str] | None:
            state[node] = 1
            stack.append(node)
            for destination in adjacency.get(node, []):
                if state.get(destination) == 1:
                    start = stack.index(destination)
                    return stack[start:] + [destination]
                if state.get(destination, 0) == 0:
                    found = visit(destination)
                    if found:
                        return found
            stack.pop()
            state[node] = 2
            return None

        for bus in self.buses:
            if state.get(bus.name, 0) == 0:
                found = visit(bus.name)
                if found:
                    return found
        return ["<unknown>"]


def parse_graph(data: Any) -> MixGraph:
    if not isinstance(data, dict):
        raise GraphError("project must be a JSON object")
    if "sample_rate" not in data:
        raise GraphError("project is missing sample_rate")
    try:
        sample_rate = int(data["sample_rate"])
    except (TypeError, ValueError) as exc:
        raise GraphError("project sample_rate must be an integer") from exc
    try:
        channels = int(data.get("channels", 2))
    except (TypeError, ValueError) as exc:
        raise GraphError("project channels must be an integer") from exc
    raw_tracks = data.get("tracks")
    raw_buses = data.get("buses", [])
    if not isinstance(raw_tracks, list):
        raise GraphError("project tracks must be a list")
    if not isinstance(raw_buses, list):
        raise GraphError("project buses must be a list")

    tracks: list[Track] = []
    for index, raw in enumerate(raw_tracks):
        if not isinstance(raw, dict):
            raise GraphError(f"track {index} must be an object")
        name = str(raw.get("name", ""))
        if "source" not in raw:
            raise GraphError(f"track '{name or index}' has no source")
        tracks.append(
            Track(
                name=name,
                source=str(raw["source"]),
                gain_db=_finite(raw.get("gain_db", 0.0), f"gain on track '{name}'"),
                mute=bool(raw.get("mute", False)),
                solo=bool(raw.get("solo", False)),
                automation=_automation(raw.get("automation"), name),
                sends=_sends(raw.get("sends"), name, raw.get("bus")),
            )
        )

    buses: list[Bus] = []
    for index, raw in enumerate(raw_buses):
        if not isinstance(raw, dict):
            raise GraphError(f"bus {index} must be an object")
        name = str(raw.get("name", ""))
        buses.append(
            Bus(
                name=name,
                gain_db=_finite(raw.get("gain_db", 0.0), f"gain on bus '{name}'"),
                mute=bool(raw.get("mute", False)),
                automation=_automation(raw.get("automation"), name),
                sends=_sends(raw.get("sends"), name),
            )
        )

    raw_master = data.get("master", {})
    if not isinstance(raw_master, dict):
        raise GraphError("master must be an object")
    master_name = str(raw_master.get("name", "master"))
    master = Master(
        name=master_name,
        gain_db=_finite(raw_master.get("gain_db", 0.0), "gain on master"),
        mute=bool(raw_master.get("mute", False)),
        automation=_automation(raw_master.get("automation"), master_name),
    )
    return MixGraph(sample_rate, tracks, buses, master, channels)
