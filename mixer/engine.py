"""The single block-pulling executor shared by offline and streaming paths."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Callable

import numpy as np

from .automation import db_to_gain
from .errors import RenderError
from .graph import Graph, track_meter_nodes
from .manifest import Project
from .nodes import gain_db_block, pan_block, soft_limit_block
from .sources import TrackSource
from .stats import MixEvidence, TrackEvidence


@dataclass
class ExecutionResult:
    audio: np.ndarray | None
    report: dict
    chunks: int
    streamed_frames: int


class Executor:
    def __init__(self, project: Project, graph: Graph) -> None:
        self.project = project
        self.graph = graph
        self.sources: dict[str, TrackSource] = {}
        self.evidence = {
            track.id: TrackEvidence(track, project.sample_rate, project.duration_frames)
            for track in project.tracks
        }
        self.mix = MixEvidence(project)
        self._meters = track_meter_nodes(graph, [track.id for track in project.tracks])
        for track in project.tracks:
            if track.meter_node is not None:
                self._meters[track.id] = track.meter_node
        self._meter_tracks: dict[str, list[str]] = {}
        for track_id, node_id in self._meters.items():
            self._meter_tracks.setdefault(node_id, []).append(track_id)
        for node in graph.nodes.values():
            if node.type == "read" and node.track is not None:
                try:
                    self.sources[node.id] = TrackSource(project, project.track(node.track))
                except BaseException:
                    self.close()
                    raise

    def run(
        self,
        chunk_frames: int,
        *,
        collect_audio: bool = True,
        sink: Callable[[np.ndarray], None] | None = None,
    ) -> ExecutionResult:
        if chunk_frames <= 0:
            raise RenderError("chunk size must be positive", details={"chunk": chunk_frames})
        output_parts: list[np.ndarray] = []
        chunks = 0
        streamed = 0
        start = 0
        try:
            while start < self.project.duration_frames:
                length = min(chunk_frames, self.project.duration_frames - start)
                cache: dict[str, np.ndarray] = {}
                remaining = {node_id: 0 for node_id in self.graph.nodes}
                for node in self.graph.nodes.values():
                    for source in node.inputs.values():
                        remaining[source] += 1
                output = self._eval(self.graph.output_node, start, length, cache, remaining)
                if output.shape[0] != self.project.channels:
                    raise RenderError(
                        "graph output channel count does not match project",
                        details={"expected": self.project.channels, "actual": int(output.shape[0])},
                    )
                if output.size and not np.all(np.isfinite(output)):
                    raise RenderError("rendered output contains NaN or Inf")
                self.mix.output.update(output)
                if sink is not None:
                    sink(output)
                if collect_audio:
                    output_parts.append(output)
                streamed += output.shape[1]
                chunks += 1
                start += length
        except BaseException:
            self.close()
            raise
        self.close()
        if collect_audio and output_parts:
            audio = output_parts[0] if len(output_parts) == 1 else np.concatenate(output_parts, axis=1)
        elif collect_audio:
            audio = np.zeros((self.project.channels, 0), dtype=np.float32)
        else:
            audio = None
        report = self.report()
        return ExecutionResult(audio, report, chunks, streamed)

    def _eval(
        self,
        node_id: str,
        start: int,
        length: int,
        cache: dict[str, np.ndarray],
        remaining: dict[str, int],
    ) -> np.ndarray:
        if node_id in cache:
            return cache[node_id]
        node = self.graph.nodes[node_id]
        if node.type == "read":
            result = self.sources[node.id].read(start, length)
        elif node.type == "gain":
            source = self._eval(node.inputs["in"], start, length, cache, remaining)
            gain_db, gains = self._gain_for_node(node, start, source.shape[1])
            if node.track in self.evidence:
                self.evidence[node.track].update_gain(gain_db)
            result = gain_db_block(source, gains)
            self._release(node.inputs["in"], cache, remaining)
        elif node.type == "pan":
            source = self._eval(node.inputs["in"], start, length, cache, remaining)
            pan = self._pan_for_node(node)
            result = pan_block(source, pan, self.project.channels)
            self._release(node.inputs["in"], cache, remaining)
        elif node.type == "sum":
            ports = sorted(node.inputs, key=lambda name: int(name[2:]))
            result = np.zeros((self.project.channels, 0), dtype=np.float32)
            for index, port in enumerate(ports):
                block = self._eval(node.inputs[port], start, length, cache, remaining)
                if index == 0:
                    result = np.zeros((self.project.channels, block.shape[1]), dtype=np.float32)
                elif block.shape[1] != result.shape[1]:
                    raise RenderError("sum node received blocks with different lengths")
                if block.shape[0] == 1 and self.project.channels == 2:
                    result += block
                elif block.shape[0] != self.project.channels:
                    raise RenderError(
                        "sum input channel layout does not match project channels",
                        details={"expected": self.project.channels, "actual": int(block.shape[0])},
                    )
                else:
                    result += block
                self._release(node.inputs[port], cache, remaining)
        elif node.type == "trim":
            source = self._eval(node.inputs["in"], start, length, cache, remaining)
            maximum = node.params.get("max_samples")
            allowed = length if maximum is None else max(0, int(maximum) - start)
            result = source[:, : min(source.shape[1], allowed)]
            self._release(node.inputs["in"], cache, remaining)
        elif node.type == "limiter":
            source = self._eval(node.inputs["in"], start, length, cache, remaining)
            if node.id == self.graph.output_node:
                self.mix.has_limiter = True
                self.mix.pre_limiter.update(source)
            result = soft_limit_block(
                source,
                float(node.params.get("ceiling", 1.0)),
                float(node.params.get("softness", 1.0)),
            )
            self._release(node.inputs["in"], cache, remaining)
        else:
            raise RenderError(f"unsupported node type {node.type!r}", details={"node_id": node.id})
        cache[node_id] = result
        for track_id in self._meter_tracks.get(node_id, []):
            self.evidence[track_id].level.update(result)
        return result

    @staticmethod
    def _release(node_id: str, cache: dict[str, np.ndarray], remaining: dict[str, int]) -> None:
        remaining[node_id] -= 1
        if remaining[node_id] == 0:
            cache.pop(node_id, None)

    def _gain_for_node(self, node: object, start: int, length: int) -> tuple[np.ndarray, np.ndarray]:
        track = self.project.track(node.track) if node.track is not None else None
        static = (track.gain_db if track else 0.0) + float(node.params.get("gain_db", 0.0))
        if track is not None and track.automation is not None:
            samples = np.arange(start, start + length, dtype=np.int64)
            gain_db = track.automation.gain_db_at_samples(
                samples, self.project.sample_rate, static
            ).astype(np.float32)
            return gain_db, db_to_gain(gain_db)
        gain_db = np.full(length, static, dtype=np.float32)
        return gain_db, np.full(length, float(db_to_gain(static)), dtype=np.float32)

    def _pan_for_node(self, node: object) -> float:
        if node.track is not None:
            return float(self.project.track(node.track).pan)
        return float(node.params.get("pan", 0.0))

    def report(self) -> dict:
        tracks = [self.evidence[track.id].as_dict(self.project) for track in self.project.tracks]
        automated = [
            {
                "id": item["id"],
                "points": item["automation_points"],
                "breakpoints": item["automation_breakpoints"],
                "gain_db_min": item["gain_db_min"],
                "gain_db_max": item["gain_db_max"],
            }
            for item in tracks
            if item["automation_points"]
        ]
        first_automation = next(
            (track.automation for track in self.project.tracks if track.automation is not None),
            None,
        )
        return {
            "sample_rate": self.project.sample_rate,
            "channels": self.project.channels,
            "duration_s": self.project.duration_s,
            "tracks": tracks,
            "automation": first_automation.breakpoints if first_automation else [],
            "automation_points": first_automation.points if first_automation else 0,
            "automation_tracks": automated,
            "mix": self.mix.as_dict(),
            "warnings": self.project.warnings,
        }

    def close(self) -> None:
        for source in self.sources.values():
            try:
                source.close()
            except Exception:
                pass


def run_offline(project: Project) -> ExecutionResult:
    return Executor(project, project.graph()).run(project.duration_frames, collect_audio=True)


def run_stream(project: Project, chunk: int, sink: Callable[[np.ndarray], None] | None = None) -> ExecutionResult:
    return Executor(project, project.graph()).run(chunk, collect_audio=False, sink=sink)
