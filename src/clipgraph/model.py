"""Mix graph data model, JSON parsing, validation, and topological ordering."""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

VALID_MODES = {"hold", "linear", "cosine"}
VALID_DOMAINS = {"db", "linear"}
VALID_KINDS = {"source", "track", "bus"}
VALID_RATES = set(range(8000, 384001, 1000)) | {44100, 88200, 96000, 176400, 192000}


class GraphError(ValueError):
    """A user-facing graph/schema error with a node path."""

    def __init__(self, message: str, path: str = "graph"):
        super().__init__(f"{message} at {path}")
        self.message = message
        self.path = path


@dataclass(frozen=True)
class CurvePoint:
    time: float
    value: float
    mode: str = "linear"


@dataclass(frozen=True)
class AutomationCurve:
    curve_id: str
    points: tuple[CurvePoint, ...]
    domain: str = "db"

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.curve_id,
            "domain": self.domain,
            "points": [[p.time, p.value, p.mode] for p in self.points],
        }


@dataclass(frozen=True)
class SourceSpec:
    path: str
    sample_rate: int
    channels: int
    start_sec: float = 0.0


@dataclass(frozen=True)
class SidechainSpec:
    source: str
    attack_sec: float
    release_sec: float
    max_duck_db: float
    threshold_db: float = -40.0


@dataclass(frozen=True)
class Node:
    node_id: str
    kind: str
    inputs: tuple[str, ...] = ()
    curves: tuple[AutomationCurve, ...] = ()
    gain_db: float = 0.0
    source: SourceSpec | None = None
    sidechain: SidechainSpec | None = None

    def to_dict(self) -> dict[str, Any]:
        data: dict[str, Any] = {
            "id": self.node_id,
            "kind": self.kind,
            "gain_db": self.gain_db,
            "curves": [c.to_dict() for c in self.curves],
        }
        if self.inputs:
            data["inputs"] = list(self.inputs)
        if self.source is not None:
            data["source"] = {
                "path": self.source.path,
                "sample_rate": self.source.sample_rate,
                "channels": self.source.channels,
                "start_sec": self.source.start_sec,
            }
        if self.sidechain is not None:
            data["sidechain"] = {
                "source": self.sidechain.source,
                "attack_sec": self.sidechain.attack_sec,
                "release_sec": self.sidechain.release_sec,
                "max_duck_db": self.sidechain.max_duck_db,
                "threshold_db": self.sidechain.threshold_db,
            }
        return data


@dataclass(frozen=True)
class MixGraph:
    sample_rate: int
    channels: int
    duration: float
    nodes: tuple[Node, ...]
    master: str
    tracks: tuple[str, ...] = ()
    buses: tuple[str, ...] = ()
    metadata: dict[str, Any] = field(default_factory=dict)

    @property
    def frames(self) -> int:
        return int(round(self.duration * self.sample_rate))

    def node_map(self) -> dict[str, Node]:
        return {node.node_id: node for node in self.nodes}

    def topological_order(self) -> list[str]:
        _validate(self)
        nodes = self.node_map()
        index = {node.node_id: i for i, node in enumerate(self.nodes)}
        deps = _dependencies(self.nodes)
        pending = {name: len(deps[name]) for name in nodes}
        ready = [index[name] for name, count in pending.items() if count == 0]
        import heapq

        heapq.heapify(ready)
        order: list[str] = []
        while ready:
            current = self.nodes[heapq.heappop(ready)].node_id
            order.append(current)
            for candidate in self.nodes:
                if current in deps[candidate.node_id]:
                    pending[candidate.node_id] -= 1
                    if pending[candidate.node_id] == 0:
                        heapq.heappush(ready, index[candidate.node_id])
        if len(order) != len(nodes):
            raise GraphError("cycle detected", " -> ".join(_find_cycle(deps)))
        return order

    def to_dict(self) -> dict[str, Any]:
        return {
            "project": {
                "sample_rate": self.sample_rate,
                "channels": self.channels,
                "duration": self.duration,
            },
            "nodes": [node.to_dict() for node in self.nodes],
            "outputs": {
                "master": self.master,
                "tracks": list(self.tracks),
                "buses": list(self.buses),
            },
            **({"metadata": self.metadata} if self.metadata else {}),
        }


def _dependencies(nodes: tuple[Node, ...]) -> dict[str, tuple[str, ...]]:
    return {
        node.node_id: tuple(dict.fromkeys(node.inputs + ((node.sidechain.source,) if node.sidechain else ())))
        for node in nodes
    }


def _find_cycle(deps: dict[str, tuple[str, ...]]) -> list[str]:
    state: dict[str, int] = {}
    stack: list[str] = []

    def visit(name: str) -> list[str] | None:
        state[name] = 1
        stack.append(name)
        for dep in deps[name]:
            if state.get(dep) == 1:
                return stack[stack.index(dep) :] + [dep]
            if state.get(dep, 0) == 0:
                found = visit(dep)
                if found:
                    return found
        stack.pop()
        state[name] = 2
        return None

    for name in deps:
        if state.get(name, 0) == 0:
            found = visit(name)
            if found:
                return found
    return []


def _number(value: Any, path: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
        raise GraphError("expected a finite number", path)
    return float(value)


def _integer(value: Any, path: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise GraphError("expected an integer", path)
    return value


def _parse_curve(raw: dict[str, Any], path: str) -> AutomationCurve:
    curve_id = raw.get("id")
    if not isinstance(curve_id, str) or not curve_id:
        raise GraphError("curve id must be a non-empty string", f"{path}.id")
    domain = raw.get("domain", "db")
    if domain not in VALID_DOMAINS:
        raise GraphError(f"invalid curve domain {domain!r}", f"{path}.domain")
    points_raw = raw.get("points")
    if not isinstance(points_raw, list) or not points_raw:
        raise GraphError("curve points must be a non-empty list", f"{path}.points")
    points: list[CurvePoint] = []
    for j, item in enumerate(points_raw):
        ppath = f"{path}.points[{j}]"
        if isinstance(item, (list, tuple)) and len(item) in (2, 3):
            time, value = _number(item[0], f"{ppath}[0]"), _number(item[1], f"{ppath}[1]")
            mode = item[2] if len(item) == 3 else "linear"
        elif isinstance(item, dict):
            time = _number(item.get("time"), f"{ppath}.time")
            value = _number(item.get("db", item.get("value")), f"{ppath}.value")
            mode = item.get("mode", "linear")
        else:
            raise GraphError("point must be [time, value, mode] or an object", ppath)
        if not isinstance(mode, str) or mode not in VALID_MODES:
            raise GraphError(f"invalid interpolation mode {mode!r}", f"{ppath}.mode")
        points.append(CurvePoint(time, value, mode))
    return AutomationCurve(curve_id, tuple(points), domain)


def _parse_node(raw: dict[str, Any], i: int) -> Node:
    path = f"nodes[{i}]"
    node_id = raw.get("id")
    if not isinstance(node_id, str) or not node_id:
        raise GraphError("node id must be a non-empty string", f"{path}.id")
    path = f"nodes[{i}] ({node_id})"
    kind = raw.get("kind")
    if kind not in VALID_KINDS:
        raise GraphError(f"invalid node kind {kind!r}", f"{path}.kind")
    inputs_raw = raw.get("inputs", [])
    if not isinstance(inputs_raw, list) or not all(isinstance(x, str) for x in inputs_raw):
        raise GraphError("inputs must be a list of node ids", f"{path}.inputs")
    curves_raw = raw.get("curves", [])
    if not isinstance(curves_raw, list):
        raise GraphError("curves must be a list", f"{path}.curves")
    source = None
    if raw.get("source") is not None:
        sraw = raw["source"]
        if not isinstance(sraw, dict):
            raise GraphError("source must be an object", f"{path}.source")
        source = SourceSpec(
            path=sraw.get("path", ""),
            sample_rate=_integer(sraw.get("sample_rate"), f"{path}.source.sample_rate"),
            channels=_integer(sraw.get("channels"), f"{path}.source.channels"),
            start_sec=_number(sraw.get("start_sec", 0.0), f"{path}.source.start_sec"),
        )
    sidechain = None
    if raw.get("sidechain") is not None:
        sraw = raw["sidechain"]
        if not isinstance(sraw, dict):
            raise GraphError("sidechain must be an object", f"{path}.sidechain")
        sidechain = SidechainSpec(
            source=sraw.get("source"),
            attack_sec=_number(sraw.get("attack_sec"), f"{path}.sidechain.attack_sec"),
            release_sec=_number(sraw.get("release_sec"), f"{path}.sidechain.release_sec"),
            max_duck_db=_number(sraw.get("max_duck_db"), f"{path}.sidechain.max_duck_db"),
            threshold_db=_number(sraw.get("threshold_db", -40.0), f"{path}.sidechain.threshold_db"),
        )
    return Node(
        node_id=node_id,
        kind=kind,
        inputs=tuple(inputs_raw),
        curves=tuple(_parse_curve(c, f"{path}.curves[{j}]") for j, c in enumerate(curves_raw)),
        gain_db=_number(raw.get("gain_db", 0.0), f"{path}.gain_db"),
        source=source,
        sidechain=sidechain,
    )


def graph_from_dict(data: dict[str, Any]) -> MixGraph:
    if not isinstance(data, dict):
        raise GraphError("top-level JSON must be an object")
    project = data.get("project", data)
    if not isinstance(project, dict) or not isinstance(data.get("nodes"), list):
        raise GraphError("graph requires project and nodes")
    outputs = data.get("outputs", {})
    if not isinstance(outputs, dict):
        raise GraphError("outputs must be an object", "outputs")
    graph = MixGraph(
        sample_rate=_integer(project.get("sample_rate"), "project.sample_rate"),
        channels=_integer(project.get("channels"), "project.channels"),
        duration=_number(project.get("duration"), "project.duration"),
        nodes=tuple(_parse_node(raw, i) for i, raw in enumerate(data["nodes"])),
        master=outputs.get("master"),
        tracks=tuple(outputs.get("tracks", [])),
        buses=tuple(outputs.get("buses", [])),
        metadata=data.get("metadata", {}),
    )
    _validate(graph)
    return graph


def load_graph(path: str | Path) -> MixGraph:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            data = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise GraphError(f"cannot read graph: {exc}", str(path)) from exc
    return graph_from_dict(data)


def _validate(graph: MixGraph) -> None:
    if graph.sample_rate not in VALID_RATES:
        raise GraphError(f"invalid project sample rate {graph.sample_rate}", "project.sample_rate")
    if graph.channels < 1 or graph.channels > 32:
        raise GraphError(f"invalid project channel count {graph.channels}", "project.channels")
    if not math.isfinite(graph.duration) or graph.duration <= 0:
        raise GraphError("duration must be positive and finite", "project.duration")
    seen: dict[str, int] = {}
    curve_ids: set[str] = set()
    for i, node in enumerate(graph.nodes):
        path = f"nodes[{i}] ({node.node_id})"
        if node.node_id in seen:
            raise GraphError(f"duplicate node id {node.node_id!r}", f"{path}.id")
        seen[node.node_id] = i
        if node.kind not in VALID_KINDS:
            raise GraphError(f"invalid node kind {node.kind!r}", f"{path}.kind")
        if not math.isfinite(node.gain_db):
            raise GraphError("gain_db must be finite", f"{path}.gain_db")
        for j, curve in enumerate(node.curves):
            cpath = f"{path}.curves[{j}] ({curve.curve_id})"
            if curve.curve_id in curve_ids:
                raise GraphError(f"duplicate curve id {curve.curve_id!r}", f"{cpath}.id")
            curve_ids.add(curve.curve_id)
            if curve.domain not in VALID_DOMAINS:
                raise GraphError(f"invalid curve domain {curve.domain!r}", f"{cpath}.domain")
            previous = -math.inf
            for k, point in enumerate(curve.points):
                ppath = f"{cpath}.points[{k}]"
                if not math.isfinite(point.time) or point.time < 0:
                    raise GraphError("curve time must be finite and non-negative", f"{ppath}.time")
                if point.time < previous:
                    raise GraphError("curve time points are out of order", f"{ppath}.time")
                if point.time == previous:
                    raise GraphError("duplicate curve time", f"{ppath}.time")
                previous = point.time
                if not math.isfinite(point.value):
                    raise GraphError("curve value must be finite", f"{ppath}.value")
                if curve.domain == "linear" and point.value < 0:
                    raise GraphError("linear-domain curve values must be non-negative", f"{ppath}.value")
                if point.mode not in VALID_MODES:
                    raise GraphError(f"invalid interpolation mode {point.mode!r}", f"{ppath}.mode")
        if node.kind == "source":
            if node.inputs:
                raise GraphError("source nodes cannot have inputs", f"{path}.inputs")
            if node.source is None:
                raise GraphError("source node requires source metadata", f"{path}.source")
            src = node.source
            if not isinstance(src.path, str) or not src.path:
                raise GraphError("source path must be non-empty", f"{path}.source.path")
            if src.sample_rate not in VALID_RATES:
                raise GraphError(f"invalid source sample rate {src.sample_rate}", f"{path}.source.sample_rate")
            if not isinstance(src.channels, int) or not 1 <= src.channels <= 32:
                raise GraphError(f"invalid source channel count {src.channels}", f"{path}.source.channels")
            if not math.isfinite(src.start_sec) or src.start_sec < 0:
                raise GraphError("source start_sec must be finite and non-negative", f"{path}.source.start_sec")
        elif node.kind == "track":
            if len(node.inputs) != 1:
                raise GraphError("track requires exactly one input", f"{path}.inputs")
            if node.source is not None:
                raise GraphError("track does not carry source metadata", f"{path}.source")
        elif not node.inputs:
            raise GraphError("bus requires at least one input", f"{path}.inputs")
        if node.sidechain is not None:
            sc = node.sidechain
            spath = f"{path}.sidechain"
            if not isinstance(sc.source, str) or not sc.source:
                raise GraphError("sidechain source must be a non-empty node id", f"{spath}.source")
            for key in ("attack_sec", "release_sec", "max_duck_db", "threshold_db"):
                if not math.isfinite(getattr(sc, key)):
                    raise GraphError(f"{key} must be finite", f"{spath}.{key}")
            if sc.attack_sec < 0 or sc.release_sec < 0:
                raise GraphError("attack/release must be non-negative", spath)
            if sc.max_duck_db < 0:
                raise GraphError("max_duck_db must be non-negative", f"{spath}.max_duck_db")
    for i, node in enumerate(graph.nodes):
        path = f"nodes[{i}] ({node.node_id})"
        for j, ref in enumerate(node.inputs):
            if ref not in seen:
                raise GraphError(f"dangling input reference {ref!r}", f"{path}.inputs[{j}]")
        if node.sidechain is not None and node.sidechain.source not in seen:
            raise GraphError(f"dangling sidechain reference {node.sidechain.source!r}", f"{path}.sidechain.source")
    deps = _dependencies(graph.nodes)
    cycle = _find_cycle(deps)
    if cycle:
        raise GraphError("cycle detected", " -> ".join(cycle))
    if not isinstance(graph.master, str) or graph.master not in seen:
        raise GraphError("outputs.master must reference an existing node", "outputs.master")
    for key, refs in (("tracks", graph.tracks), ("buses", graph.buses)):
        for j, ref in enumerate(refs):
            if not isinstance(ref, str) or ref not in seen:
                raise GraphError(f"dangling output reference {ref!r}", f"outputs.{key}[{j}]")
            expected = "track" if key == "tracks" else "bus"
            if graph.nodes[seen[ref]].kind != expected:
                raise GraphError(f"{key} output {ref!r} must be a {expected}", f"outputs.{key}[{j}]")
