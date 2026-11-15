"""Stateless graph evaluator with explicit, block-safe render state."""

from __future__ import annotations

from dataclasses import dataclass, field

import numpy as np

from .curve import db_to_gain, frame_times, gain_at_db
from .model import GraphError, MixGraph
from .resample import SourceReader, phase_at, resample_source


@dataclass
class RenderState:
    """Explicit cross-block state; render calls never mutate it in place."""

    cursor: int = 0
    sidechain_env: dict[str, float] = field(default_factory=dict)
    resample_phase: dict[str, float] = field(default_factory=dict)
    nonfinite_zeroed: dict[str, int] = field(default_factory=dict)


@dataclass
class RenderResult:
    signals: dict[str, np.ndarray]
    duck_db: dict[str, np.ndarray]
    state: RenderState


def _finite_counts(signal: np.ndarray) -> int:
    return int(np.count_nonzero(~np.isfinite(signal)))


def _sanitize(signal: np.ndarray, node_id: str, counts: dict[str, int]) -> np.ndarray:
    bad = _finite_counts(signal)
    if bad:
        counts[node_id] = counts.get(node_id, 0) + bad
        signal = np.nan_to_num(signal, nan=0.0, posinf=0.0, neginf=0.0)
    return signal


def _smooth_coefficient(seconds: float, sample_rate: int) -> float:
    if seconds <= 0.0:
        return 1.0
    return float(1.0 - np.exp(-1.0 / (seconds * sample_rate)))


def _sidechain_duck(
    detector: np.ndarray,
    spec,
    sample_rate: int,
    initial_env: float,
) -> tuple[np.ndarray, np.ndarray, float]:
    """Sample-recursive peak envelope and dB duck depth."""
    attack = _smooth_coefficient(spec.attack_sec, sample_rate)
    release = _smooth_coefficient(spec.release_sec, sample_rate)
    threshold = max(db_to_gain(float(spec.threshold_db)), 1.0e-12)
    level = np.max(np.abs(detector), axis=1) if detector.size else np.empty(0)
    duck = np.empty(level.shape[0], dtype=np.float64)
    gains = np.empty(level.shape[0], dtype=np.float64)
    env = float(initial_env)
    for i, value in enumerate(level):
        coefficient = attack if value > env else release
        env += coefficient * (float(value) - env)
        depth = float(spec.max_duck_db) * min(1.0, env / threshold)
        duck[i] = depth
        gains[i] = db_to_gain(-depth)
    return gains, duck, env


class Engine:
    """Evaluate any [start, end) frame range without persistent mutable state."""

    def __init__(self, graph: MixGraph, reader: SourceReader):
        self.graph = graph
        self.reader = reader
        self.order = graph.topological_order()
        self.nodes = graph.node_map()

    def render(
        self,
        start: int,
        end: int,
        output_ids: tuple[str, ...] | list[str] = (),
        state: RenderState | None = None,
    ) -> RenderResult:
        state = state or RenderState()
        if start < 0 or end < start or end > self.graph.frames:
            raise GraphError("render range outside project duration", f"frames[{start}:{end}]")
        if start != state.cursor:
            raise GraphError(f"state cursor {state.cursor} does not match start {start}", "render state")
        length = end - start
        times = frame_times(start, end, self.graph.sample_rate)
        signals: dict[str, np.ndarray] = {}
        ducks: dict[str, np.ndarray] = {}
        counts = dict(state.nonfinite_zeroed)
        envelope = dict(state.sidechain_env)
        phases = dict(state.resample_phase)
        for node_id in self.order:
            node = self.nodes[node_id]
            if node.kind == "source":
                assert node.source is not None
                signal = resample_source(
                    node.source, start, end, self.graph.sample_rate, self.graph.channels, self.reader
                ).astype(np.float64)
            elif node.kind == "track":
                signal = signals[node.inputs[0]] * gain_at_db(node.curves, times, node.gain_db)[:, None]
            else:
                signal = np.zeros((length, self.graph.channels), dtype=np.float64)
                for input_id in node.inputs:
                    signal += signals[input_id]
                if node.sidechain is not None:
                    gains, duck, envelope[node_id] = _sidechain_duck(
                        signals[node.sidechain.source],
                        node.sidechain,
                        self.graph.sample_rate,
                        envelope.get(node_id, 0.0),
                    )
                    signal *= gains[:, None]
                    ducks[node_id] = duck
                signal *= gain_at_db(node.curves, times, node.gain_db)[:, None]
            signal = _sanitize(signal, node_id, counts)
            signals[node_id] = signal
            if node.kind == "source" and node.source is not None:
                phases[node_id] = phase_at(node.source, max(start, end - 1), self.graph.sample_rate)
        for node_id in output_ids:
            if node_id not in signals:
                raise GraphError(f"unknown render output {node_id!r}", "render output")
        selected = {node_id: signals[node_id] for node_id in output_ids} if output_ids else dict(signals)
        next_state = RenderState(
            cursor=end,
            sidechain_env=envelope,
            resample_phase=phases,
            nonfinite_zeroed=counts,
        )
        return RenderResult(selected, ducks, next_state)
