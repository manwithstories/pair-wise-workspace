"""Block-oriented mix engine shared by streaming and offline rendering."""
from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np
import soundfile as sf

from .assets import AssetError, AudioReader
from .graph import MixGraph
from .meters import MeterBank, Metric


class StreamingError(ValueError):
    """Raised for invalid streaming options or state."""


def db_gain(db: float) -> float:
    return float(10.0 ** (float(db) / 20.0))


class MixEngine:
    """Process a graph one block at a time with O(nodes * block) memory."""

    def __init__(self, graph: MixGraph, project_dir: str | Path):
        self.graph = graph
        self.project_dir = Path(project_dir)
        self.readers: dict[str, AudioReader] = {}
        try:
            for track in graph.tracks:
                source = Path(track.source)
                if not source.is_absolute():
                    source = self.project_dir / source
                self.readers[track.name] = AudioReader(
                    source, graph.sample_rate, graph.channels
                )
        except Exception:
            self.close()
            raise
        self.total_frames = max(
            (reader.total_frames for reader in self.readers.values()), default=0
        )
        names = [track.name for track in graph.tracks]
        names += [bus.name for bus in graph.buses]
        names.append(graph.master.name)
        self.meters = MeterBank(names)
        self.position = 0
        self._last_metrics: dict[str, Metric] = {}

    def close(self) -> None:
        for reader in self.readers.values():
            reader.close()
        self.readers.clear()

    def _automation_gain(self, node: Any, frames: int) -> np.ndarray:
        if node.automation is None:
            return np.ones(frames, dtype=np.float64)
        times = (
            np.arange(self.position, self.position + frames, dtype=np.float64)
            / self.graph.sample_rate
        )
        return np.asarray(node.automation.gain_at(times), dtype=np.float64)

    def process(self, frames: int) -> np.ndarray:
        if frames <= 0:
            return np.zeros((self.graph.channels, 0), dtype=np.float64)
        remaining = self.total_frames - self.position
        if frames > remaining:
            raise StreamingError(
                f"requested {frames} frames with only {remaining} remaining"
            )
        zeros = np.zeros((self.graph.channels, frames), dtype=np.float64)
        inputs: dict[str, np.ndarray] = {}

        first_solo = self.graph.solo_active
        for track in self.graph.tracks:
            source = self.readers[track.name].read(frames)
            actual = source.shape[1]
            automation = self._automation_gain(track, frames)
            report_factor = self.graph.report_track_factor(track, automation[:actual])
            self.meters.update(track.name, source * report_factor[None, :])
            audible_factor = report_factor.copy()
            if first_solo and not track.solo:
                audible_factor = np.zeros_like(audible_factor)
            audible = source * audible_factor[None, :]
            if actual < frames:
                padded = zeros.copy()
                padded[:, :actual] = audible
                audible = padded
            for send in track.sends:
                contribution = audible * db_gain(send.gain_db)
                inputs[send.destination] = (
                    inputs.get(send.destination, zeros.copy()) + contribution
                )

        for bus in self.graph.buses:
            bus_input = inputs.pop(bus.name, zeros.copy())
            automation = self._automation_gain(bus, frames)
            factor = db_gain(bus.gain_db) * automation
            if bus.mute:
                factor = np.zeros_like(factor)
            bus_output = bus_input * factor[None, :]
            self.meters.update(bus.name, bus_output)
            for send in bus.sends:
                contribution = bus_output * db_gain(send.gain_db)
                inputs[send.destination] = (
                    inputs.get(send.destination, zeros.copy()) + contribution
                )

        master_input = inputs.pop(self.graph.master.name, zeros.copy())
        automation = self._automation_gain(self.graph.master, frames)
        master_factor = db_gain(self.graph.master.gain_db) * automation
        if self.graph.master.mute:
            master_factor = np.zeros_like(master_factor)
        master_output = master_input * master_factor[None, :]
        self.meters.update(self.graph.master.name, master_output)
        if inputs:
            raise StreamingError(
                "graph routing left unhandled nodes: " + ", ".join(sorted(inputs))
            )
        self._last_metrics = self.meters.last_all()
        self.position += frames
        return master_output

    def finish(self) -> None:
        if self.position != self.total_frames:
            raise StreamingError(
                f"engine finished at {self.position} of {self.total_frames} frames"
            )
        self.close()

    def metrics(self) -> dict[str, Any]:
        tracks = [self.meters.total(track.name) for track in self.graph.tracks]
        buses = [self.meters.total(bus.name) for bus in self.graph.buses]
        master = self.meters.total(self.graph.master.name)
        return {"tracks": tracks, "buses": buses, "master": master}

    def last_metrics(self) -> dict[str, Metric]:
        return dict(self._last_metrics)

    def __enter__(self) -> "MixEngine":
        return self

    def __exit__(self, *_: object) -> None:
        self.close()


def _incremental_object(block: int, frames: int, metrics: dict[str, Metric]) -> dict[str, Any]:
    return {
        "block": block,
        "frames": frames,
        "metrics": {name: metric.as_dict() for name, metric in metrics.items()},
    }


def stream_project(
    graph: MixGraph,
    project_dir: str | Path,
    block_size: int,
    output_path: str | Path,
    print_metrics: bool = False,
) -> dict[str, Any]:
    if block_size <= 0 or block_size & (block_size - 1):
        raise StreamingError("block size must be a positive power of two")
    output = Path(output_path)
    output.parent.mkdir(parents=True, exist_ok=True)
    engine = MixEngine(graph, project_dir)
    block = 0
    try:
        with sf.SoundFile(
            str(output),
            mode="w",
            samplerate=graph.sample_rate,
            channels=graph.channels,
            subtype="FLOAT",
            format="WAV",
        ) as handle:
            while engine.position < engine.total_frames:
                frames = min(block_size, engine.total_frames - engine.position)
                mixed = engine.process(frames)
                handle.write(mixed.T)
                block += 1
                if print_metrics:
                    print(
                        json.dumps(
                            _incremental_object(block, frames, engine.last_metrics()),
                            ensure_ascii=False,
                            sort_keys=True,
                        ),
                        flush=True,
                    )
        engine.finish()
        return engine.metrics()
    except (RuntimeError, OSError) as exc:
        engine.close()
        raise AssetError(f"cannot write stream WAV '{output}': {exc}") from exc
