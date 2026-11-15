"""Runtime mix nodes and the chunked offline render path."""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import cast

import numpy as np

from mixer.automation import AutomationCurve, load_automation
from mixer.graph import Graph, GraphError, NodeSpec
from mixer.io import (
    AudioWriter,
    SourceReader,
    resolve_input_path,
    validate_sample_rate,
)
from mixer.metrics import MetricsAccumulator, MetricsSnapshot
from mixer.resample import (
    design_lowpass,
    rational_ratio,
    resampled_length,
)


class RuntimeNode:
    """Common interface used by both render paths."""

    def __init__(self, spec: NodeSpec, children: list["RuntimeNode"]) -> None:
        self.spec = spec
        self.children = children

    @property
    def channels(self) -> int:
        raise NotImplementedError

    @property
    def total_frames(self) -> int:
        raise NotImplementedError

    def process(self, nframes: int, start_frame: int) -> np.ndarray:
        raise NotImplementedError

    def close(self) -> None:
        for child in self.children:
            child.close()

    def _empty(self, nframes: int) -> np.ndarray:
        return np.zeros((max(0, int(nframes)), self.channels), dtype=np.float64)


class SourceNode(RuntimeNode):
    def __init__(
        self,
        spec: NodeSpec,
        master_sr: int,
        workspace: Path,
        base_dir: Path | None = None,
    ) -> None:
        super().__init__(spec, [])
        raw_path = spec.get("path", spec.get("file"))
        if not isinstance(raw_path, str) or not raw_path.strip():
            raise GraphError(f"source node {spec.id} has no path")
        self.path = resolve_input_path(raw_path, workspace, base_dir)
        expected = spec.get("channels")
        if expected is not None:
            if not isinstance(expected, int) or expected not in (1, 2):
                raise GraphError(f"source node {spec.id} channels must be 1 or 2")
        self.reader = SourceReader(self.path, expected_channels=expected)
        self.master_sr = validate_sample_rate(master_sr)
        self.source_sr = self.reader.sample_rate
        self.ratio: Fraction = rational_ratio(self.source_sr, self.master_sr)
        self._total_frames = resampled_length(
            self.reader.frames, self.ratio.numerator, self.ratio.denominator
        )
        self._kernel = None
        if self.ratio.numerator != self.ratio.denominator:
            self._kernel = design_lowpass(
                self.ratio.numerator, self.ratio.denominator
            )

    @property
    def channels(self) -> int:
        return self.reader.channels

    @property
    def total_frames(self) -> int:
        return self._total_frames

    def close(self) -> None:
        self.reader.close()

    def _read_padded(self, start: int, count: int) -> np.ndarray:
        data = self.reader.read(start, count)
        if data.shape[0] == count:
            return data
        padded = np.zeros((count, self.channels), dtype=np.float64)
        if data.shape[0]:
            padded[: data.shape[0]] = data
        return padded

    def _resample_range(self, start_frame: int, nframes: int) -> np.ndarray:
        kernel = self._kernel
        if kernel is None:
            return np.zeros((nframes, self.channels), dtype=np.float64)
        up = self.ratio.numerator
        down = self.ratio.denominator
        out_numbers = np.arange(start_frame, start_frame + nframes, dtype=np.int64)
        positions = out_numbers * down
        remainder = positions % up
        base_input = (positions - remainder) // up
        max_q = (kernel.size - 1) // up + 1
        input_min = max(0, int(base_input.min()) - max_q)
        input_max = min(self.reader.frames - 1, int(base_input.max()))
        if input_max < input_min:
            return np.zeros((nframes, self.channels), dtype=np.float64)
        segment = self.reader.read(input_min, input_max - input_min + 1)
        result = np.zeros((nframes, self.channels), dtype=np.float64)
        for q in range(max_q):
            kernel_index = remainder + q * up
            index = base_input - input_min - q
            valid = (
                (kernel_index < kernel.size)
                & (index >= 0)
                & (index < segment.shape[0])
            )
            if not np.any(valid):
                continue
            coeff = kernel[kernel_index[valid]]
            result[valid] += coeff[:, None] * segment[index[valid]]
        return result * float(up)

    def process(self, nframes: int, start_frame: int) -> np.ndarray:
        nframes = max(0, int(nframes))
        if nframes == 0:
            return np.empty((0, self.channels), dtype=np.float64)
        if start_frame >= self.total_frames:
            return self._empty(nframes)
        if self.ratio.numerator == self.ratio.denominator:
            return self._read_padded(start_frame, nframes)
        return self._resample_range(start_frame, nframes)


class GainNode(RuntimeNode):
    def __init__(
        self, spec: NodeSpec, child: RuntimeNode, curve: AutomationCurve, master_sr: int
    ):
        super().__init__(spec, [child])
        self.child = child
        self.curve = curve
        self.master_sr = int(master_sr)
        self.base_db = float(spec.get("gain_db", 0.0))
        if not math.isfinite(self.base_db):
            raise GraphError(f"gain node {spec.id} gain_db must be finite")

    @property
    def channels(self) -> int:
        return self.child.channels

    @property
    def total_frames(self) -> int:
        return self.child.total_frames

    def gain_db_at(self, time_sec: float | np.ndarray) -> float | np.ndarray:
        return np.asarray(self.curve.evaluate(time_sec)) + self.base_db

    def process(self, nframes: int, start_frame: int) -> np.ndarray:
        data = self.child.process(nframes, start_frame)
        times = (int(start_frame) + np.arange(data.shape[0], dtype=np.float64)) / float(
            self.master_sr
        )
        gain_db = np.asarray(self.gain_db_at(times), dtype=np.float64)
        return data * np.power(10.0, gain_db / 20.0)[:, None]


class PanNode(RuntimeNode):
    def __init__(self, spec: NodeSpec, child: RuntimeNode):
        super().__init__(spec, [child])
        self.child = child
        self.pan = float(spec.get("pan", 0.0))
        if not math.isfinite(self.pan) or not -1.0 <= self.pan <= 1.0:
            raise GraphError(f"pan node {spec.id} pan must be within -1..1")

    @property
    def channels(self) -> int:
        return 2

    @property
    def total_frames(self) -> int:
        return self.child.total_frames

    def process(self, nframes: int, start_frame: int) -> np.ndarray:
        data = self.child.process(nframes, start_frame)
        if self.child.channels == 1:
            angle = (self.pan + 1.0) * math.pi / 4.0
            left = math.sqrt(2.0) * math.cos(angle)
            right = math.sqrt(2.0) * math.sin(angle)
            return np.concatenate((data * left, data * right), axis=1)
        left = 1.0 - max(0.0, self.pan)
        right = 1.0 + min(0.0, self.pan)
        return data * np.array([left, right], dtype=np.float64)[None, :]


class SumNode(RuntimeNode):
    def __init__(self, spec: NodeSpec, children: list[RuntimeNode]):
        super().__init__(spec, children)
        if not children:
            raise GraphError(f"sum node {spec.id} has no inputs")
        self._channels = 2 if any(child.channels == 2 for child in children) else 1
        self._total_frames = max(child.total_frames for child in children)

    @property
    def channels(self) -> int:
        return self._channels

    @property
    def total_frames(self) -> int:
        return self._total_frames

    def process(self, nframes: int, start_frame: int) -> np.ndarray:
        result = np.zeros((max(0, int(nframes)), self.channels), dtype=np.float64)
        for child in self.children:
            block = child.process(nframes, start_frame)
            if self.channels == 2 and child.channels == 1:
                block = np.repeat(block, 2, axis=1)
            result += block
        return result


class LimiterNode(RuntimeNode):
    def __init__(self, spec: NodeSpec, child: RuntimeNode):
        super().__init__(spec, [child])
        self.child = child
        ceiling_db = float(spec.get("ceiling_db", -1.0))
        if not math.isfinite(ceiling_db):
            raise GraphError(f"limiter node {spec.id} ceiling_db must be finite")
        self.ceiling = 10.0 ** (ceiling_db / 20.0)

    @property
    def channels(self) -> int:
        return self.child.channels

    @property
    def total_frames(self) -> int:
        return self.child.total_frames

    def process(self, nframes: int, start_frame: int) -> np.ndarray:
        data = self.child.process(nframes, start_frame)
        return np.clip(data, -self.ceiling, self.ceiling)


@dataclass
class TrackRuntime:
    name: str
    source: SourceNode
    tap: RuntimeNode
    metrics: MetricsAccumulator
    gain_nodes: list[GainNode] = field(default_factory=list)

    def automation_db(self, times: np.ndarray) -> np.ndarray:
        result = np.zeros_like(times, dtype=np.float64)
        for gain in self.gain_nodes:
            result += np.asarray(gain.gain_db_at(times), dtype=np.float64)
        return result


def _automation_for_node(
    spec: NodeSpec, workspace: Path, base_dir: Path | None
) -> AutomationCurve:
    value: object = spec.get("automation", spec.get("curve"))
    if value is None:
        value = spec.get("automation_points")
    if value is None:
        return AutomationCurve.empty()
    if isinstance(value, str):
        path = resolve_input_path(value, workspace, base_dir)
        return load_automation(path)
    if isinstance(value, (list, tuple)):
        return AutomationCurve.from_points(value)
    raise GraphError(f"gain node {spec.id} automation must be a path or point list")


def build_runtime_nodes(
    graph: Graph, master_sr: int, workspace: str | Path
) -> dict[str, RuntimeNode]:
    workspace_path = Path(workspace).resolve()
    master_sr = validate_sample_rate(master_sr)
    nodes: dict[str, RuntimeNode] = {}
    for name in graph.order:
        spec = graph.nodes[name]
        if spec.type == "source":
            nodes[name] = SourceNode(
                spec, master_sr, workspace_path, base_dir=graph.base_dir
            )
            continue
        missing = [reference for reference in spec.inputs if reference not in nodes]
        if missing:
            raise GraphError(f"node {name} has unresolved inputs: {missing}")
        if spec.type in ("gain", "pan", "limiter"):
            child = nodes[spec.inputs[0]]
            if spec.type == "gain":
                curve = _automation_for_node(spec, workspace_path, graph.base_dir)
                nodes[name] = GainNode(spec, child, curve, master_sr)
            elif spec.type == "pan":
                nodes[name] = PanNode(spec, child)
            else:
                nodes[name] = LimiterNode(spec, child)
        elif spec.type == "sum":
            nodes[name] = SumNode(spec, [nodes[ref] for ref in spec.inputs])
        else:
            raise GraphError(f"unknown node type {spec.type!r} on node {name}")
    return nodes


def _track_tap(graph: Graph, source_id: str) -> tuple[RuntimeNode, list[str]]:
    current = source_id
    gain_ids: list[str] = []
    visited = {source_id}
    while True:
        children = graph.downstream(current)
        if len(children) != 1:
            break
        child_id = children[0]
        if child_id in visited:
            break
        child = graph.nodes[child_id]
        if child.type == "sum":
            break
        visited.add(child_id)
        if child.type == "gain":
            gain_ids.append(child_id)
        current = child_id
    return current, gain_ids


class MixSession:
    """State-free per-block node evaluation shared by offline and stream."""

    def __init__(
        self,
        graph: Graph,
        master_sr: int,
        workspace: str | Path,
        seek_frame: int = 0,
    ) -> None:
        self.graph = graph
        self.master_sr = validate_sample_rate(master_sr)
        self.workspace = Path(workspace).resolve()
        self.nodes = build_runtime_nodes(graph, self.master_sr, self.workspace)
        self.output = self.nodes[graph.output]
        self.total_frames = int(self.output.total_frames)
        self.seek_frame = max(0, int(seek_frame))
        self.render_frames = max(0, self.total_frames - self.seek_frame)
        self.bus = MetricsAccumulator(self.master_sr)
        self.tracks: list[TrackRuntime] = []
        for source in graph.sources():
            tap_id, gain_ids = _track_tap(graph, source.id)
            track = TrackRuntime(
                name=source.id,
                source=cast(SourceNode, self.nodes[source.id]),
                tap=self.nodes[tap_id],
                gain_nodes=[cast(GainNode, self.nodes[gid]) for gid in gain_ids],
                metrics=MetricsAccumulator(self.master_sr),
            )
            self.tracks.append(track)

    def process_block(self, nframes: int, start_frame: int) -> np.ndarray:
        block = self.output.process(nframes, start_frame)
        self.bus.update(block, start_frame=start_frame)
        for track in self.tracks:
            tap_block = track.tap.process(nframes, start_frame)
            track.metrics.update(tap_block, start_frame=start_frame)
        return block

    def iter_blocks(self, block_size: int):
        if block_size <= 0:
            raise ValueError("block size must be positive")
        position = self.seek_frame
        end = self.total_frames
        while position < end:
            count = min(block_size, end - position)
            yield position, self.process_block(count, position)
            position += count

    def close(self) -> None:
        self.output.close()

    def bus_snapshot(self) -> MetricsSnapshot:
        return self.bus.snapshot()


@dataclass
class RenderResult:
    output_path: Path
    frames_written: int
    channels: int
    session: MixSession


def render_to_file(
    graph: Graph,
    output_path: str | Path,
    master_sr: int,
    block_size: int = 1024,
    *,
    workspace: str | Path = ".",
    seek_frame: int = 0,
) -> RenderResult:
    if block_size <= 0:
        raise ValueError("block size must be positive")
    session = MixSession(graph, master_sr, workspace, seek_frame=seek_frame)
    writer = AudioWriter(output_path, master_sr, session.output.channels)
    frames = 0
    try:
        for _, block in session.iter_blocks(block_size):
            writer.write(block)
            frames += int(block.shape[0])
    finally:
        try:
            writer.close()
        finally:
            session.close()
    return RenderResult(
        output_path=Path(output_path),
        frames_written=frames,
        channels=session.output.channels,
        session=session,
    )


render_graph = render_to_file


class OfflineRenderer:
    """Small object wrapper around the offline chunked render function."""

    def __init__(
        self,
        graph: Graph,
        master_sr: int,
        block_size: int = 1024,
        *,
        workspace: str | Path = ".",
        seek_frame: int = 0,
    ) -> None:
        self.graph = graph
        self.master_sr = validate_sample_rate(master_sr)
        self.block_size = int(block_size)
        self.workspace = workspace
        self.seek_frame = int(seek_frame)

    def render(self, output_path: str | Path) -> RenderResult:
        return render_to_file(
            self.graph,
            output_path,
            self.master_sr,
            self.block_size,
            workspace=self.workspace,
            seek_frame=self.seek_frame,
        )


Renderer = OfflineRenderer

