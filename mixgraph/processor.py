"""Shared pull processor used by both offline and streamed renderers."""

from __future__ import annotations

from typing import TYPE_CHECKING

import numpy as np
import soundfile as sf

from .errors import ManifestError
from .graph import Graph, Node
from .meters import Meter
from .resample import RationalResampler

if TYPE_CHECKING:
    from .project import Project


class SourceRuntime:
    def __init__(self, node: Node, project: "Project") -> None:
        self.node = node
        path = node.params.get("_path")
        if not path:
            raise ManifestError("source path was not resolved", f"nodes.{node.id}.path")
        self.path = path
        self.sample_rate = int(node.params["_source_rate"])
        self.channels = int(node.params["_channels"])
        self.event_offset = int(node.params.get("_file_offset", 0))
        self.event_frames = int(node.params.get("_event_frames", project.duration_frames))
        try:
            self.handle = sf.SoundFile(path, "r")
        except (OSError, RuntimeError) as exc:
            raise ManifestError(str(exc), f"nodes.{node.id}.path") from exc
        if self.handle.channels != self.channels:
            self.handle.close()
            raise ManifestError(
                f"file has {self.handle.channels} channels, expected {self.channels}",
                f"nodes.{node.id}.channels",
            )
        self.file_frames = len(self.handle)

    def read(self, _processor: "MixProcessor", start: int, count: int) -> np.ndarray:
        if count < 0:
            raise ValueError("source read count must be non-negative")
        output = np.zeros((count, self.channels), dtype=np.float64)
        if count == 0:
            return output
        request_start = int(start)
        request_end = request_start + count
        valid_start = max(0, request_start)
        valid_end = min(
            request_end,
            self.event_frames,
            max(0, self.file_frames - self.event_offset),
        )
        if valid_start >= valid_end:
            return output
        try:
            self.handle.seek(self.event_offset + valid_start)
            data = self.handle.read(
                valid_end - valid_start, dtype="float64", always_2d=True
            )
        except (OSError, RuntimeError) as exc:
            raise ManifestError(str(exc), f"nodes.{self.node.id}.path") from exc
        if not np.all(np.isfinite(data)):
            raise ManifestError(
                "source samples contain NaN or infinity",
                f"nodes.{self.node.id}.samples",
            )
        target = valid_start - request_start
        output[target : target + data.shape[0], :] = data[:, : self.channels]
        return output

    def close(self) -> None:
        self.handle.close()


class ResampleRuntime:
    def __init__(
        self, node: Node, upstream_id: str, project: "Project", source_rate: int
    ) -> None:
        self.node = node
        self.upstream_id = upstream_id
        target_rate = int(node.params.get("rate", project.sample_rate))
        self.resampler = RationalResampler(
            source_rate, target_rate, field=f"nodes.{node.id}.rate"
        )

    @property
    def ratio(self):
        return self.resampler.ratio

    def read(self, processor: "MixProcessor", start: int, count: int) -> np.ndarray:
        return self.resampler.render(
            lambda native_start, native_count: processor.read(
                self.upstream_id, native_start, native_count
            ),
            start,
            count,
        )


class GainRuntime:
    def __init__(self, node: Node, upstream_id: str, project: "Project") -> None:
        self.node = node
        self.upstream_id = upstream_id
        self.base_db = float(node.params.get("gain_db", 0.0))
        self.curve = node.params["_automation"]
        self.active = bool(node.params.get("_active", True))
        self.sample_rate = project.sample_rate

    def read(self, processor: "MixProcessor", start: int, count: int) -> np.ndarray:
        data = processor.read(self.upstream_id, start, count)
        gains = self.curve.gain_linear_at_frames(start, count, self.sample_rate, self.base_db)
        if not self.active:
            gains = np.zeros_like(gains)
        return data * gains[:, None]


class SumRuntime:
    def __init__(self, node: Node, upstream_ids: list[str]) -> None:
        self.node = node
        self.upstream_ids = upstream_ids

    def read(self, processor: "MixProcessor", start: int, count: int) -> np.ndarray:
        if not self.upstream_ids:
            raise RuntimeError("sum has no inputs")
        total = processor.read(self.upstream_ids[0], start, count).copy()
        channels = total.shape[1]
        for upstream_id in self.upstream_ids[1:]:
            data = processor.read(upstream_id, start, count)
            if data.shape[1] != channels:
                raise ManifestError("sum inputs have different channel counts", f"nodes.{self.node.id}")
            total += data
        return total


class OutputRuntime:
    def __init__(self, node: Node, upstream_id: str) -> None:
        self.node = node
        self.upstream_id = upstream_id

    def read(self, processor: "MixProcessor", start: int, count: int) -> np.ndarray:
        return processor.read(self.upstream_id, start, count)


class MixProcessor:
    """Build runtimes once, then pull arbitrary sample-aligned blocks."""

    def __init__(self, project: "Project") -> None:
        self.project = project
        self.graph: Graph = project.graph
        self.order = self.graph.validate()
        self.runtimes: dict[str, object] = {}
        self.meters = {
            node.id: Meter(ceiling_db=project.clip_threshold_db, field=f"nodes.{node.id}")
            for node in self.graph.nodes.values()
        }
        self._cache: dict[tuple[str, int, int], np.ndarray] = {}
        self._build()
        self.output_node = project.output_node

    def _build(self) -> None:
        for node_id in self.order:
            node = self.graph.nodes[node_id]
            upstreams = [self.runtimes[item] for item in self.graph.inputs(node_id)]
            if node.type == "source":
                runtime = SourceRuntime(node, self.project)
            elif node.type == "resample":
                if len(upstreams) != 1 or not isinstance(upstreams[0], SourceRuntime):
                    raise ManifestError(
                        "resample input must be a source node", f"nodes.{node.id}.input"
                    )
                runtime = ResampleRuntime(
                    node,
                    self.graph.inputs(node_id)[0],
                    self.project,
                    upstreams[0].sample_rate,
                )
            elif node.type in {"gain", "bus"}:
                if len(upstreams) != 1:
                    raise ManifestError(
                        f"{node.type} needs one input", f"nodes.{node.id}.input"
                    )
                runtime = GainRuntime(node, self.graph.inputs(node_id)[0], self.project)
            elif node.type == "sum":
                runtime = SumRuntime(node, list(self.graph.inputs(node_id)))
            else:
                runtime = OutputRuntime(node, self.graph.inputs(node_id)[0])
            self.runtimes[node_id] = runtime

    def begin_block(self) -> None:
        self._cache.clear()

    def read(self, node_id: str, start: int, count: int) -> np.ndarray:
        key = (node_id, int(start), int(count))
        if key in self._cache:
            return self._cache[key]
        data = self.runtimes[node_id].read(self, start, count)
        self.meters[node_id].update(data, start)
        self._cache[key] = data
        return data

    def read_block(self, start: int, count: int) -> np.ndarray:
        self.begin_block()
        return self.read(self.output_node, start, count)

    def track_ratio(self, track: dict) -> object:
        gain_id = track["gain_node"]
        for ancestor in self.graph.ancestors(gain_id):
            runtime = self.runtimes.get(ancestor)
            if isinstance(runtime, ResampleRuntime):
                return runtime.ratio
        raise ManifestError("track has no resample node", f"tracks.{track['id']}")

    def close(self) -> None:
        for runtime in self.runtimes.values():
            if isinstance(runtime, SourceRuntime):
                runtime.close()

