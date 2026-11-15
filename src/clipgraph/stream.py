"""Variable-block streaming driver using the same engine as offline rendering."""

from __future__ import annotations

from pathlib import Path

from .analysis import MetricsAccumulator
from .engine import Engine, RenderState
from .io import WavSourceProvider, open_raw_writer, write_raw
from .model import MixGraph
from .offline import _curve_rows, _source_inventory, _write_json
from .resample import alignment_summary


def parse_blocks(value: str | list[int] | tuple[int, ...]) -> list[int]:
    if isinstance(value, str):
        parts = [part.strip() for part in value.split(",") if part.strip()]
        try:
            blocks = [int(part) for part in parts]
        except ValueError as exc:
            raise ValueError("block sizes must be comma-separated integers") from exc
    else:
        blocks = [int(part) for part in value]
    if not blocks or any(block < 1 for block in blocks):
        raise ValueError("block sizes must be positive")
    return blocks


def render_stream(
    graph: MixGraph,
    base_dir: str | Path,
    blocks: str | list[int] | tuple[int, ...],
    raw_path: str | Path | None = None,
    report_path: str | Path | None = None,
    window_ms: float = 400.0,
) -> dict:
    sequence = parse_blocks(blocks)
    coalesce_max = 256
    pull_count = 0
    remaining = graph.frames
    while remaining > 0:
        remaining -= min(remaining, sequence[pull_count % len(sequence)])
        pull_count += 1
    output_ids = list(dict.fromkeys([graph.master, *graph.tracks, *graph.buses]))
    provider = WavSourceProvider(graph, base_dir)
    infos = provider.open_all()
    inventory, short_sources = _source_inventory(graph, infos)
    meters = {
        node_id: MetricsAccumulator(graph.sample_rate, window_ms, graph.channels) for node_id in output_ids
    }
    duck_max = {node_id: 0.0 for node_id in graph.buses}
    state = RenderState()
    engine = Engine(graph, provider)
    raw_handle = open_raw_writer(raw_path) if raw_path else None
    engine_windows = 0
    try:
        start = 0
        pull_index = 0
        while start < graph.frames:
            requested = sequence[pull_index % len(sequence)]
            end = min(graph.frames, start + requested)
            consumed = 1
            while end - start < coalesce_max and end < graph.frames:
                end = min(graph.frames, end + sequence[(pull_index + consumed) % len(sequence)])
                consumed += 1
            result = engine.render(start, end, output_ids, state)
            state = result.state
            if raw_handle is not None:
                write_raw(raw_handle, result.signals[graph.master])
            for node_id, meter in meters.items():
                meter.add(result.signals[node_id])
            for node_id, duck in result.duck_db.items():
                if duck.size:
                    duck_max[node_id] = max(duck_max.get(node_id, 0.0), float(duck.max()))
            engine_windows += 1
            pull_index += consumed
            start = end
    finally:
        provider.close()
        if raw_handle is not None:
            raw_handle.close()
    for meter in meters.values():
        meter.finish()
    tracks = {node_id: {**meters[node_id].summary(), "track_id": node_id} for node_id in graph.tracks}
    buses = {
        node_id: {
            **meters[node_id].summary(),
            "bus_id": node_id,
            "duck_max_db": -duck_max.get(node_id, 0.0) if duck_max.get(node_id, 0.0) else 0.0,
        }
        for node_id in graph.buses
    }
    nonfinite = {"total": sum(state.nonfinite_zeroed.values()), "by_node": state.nonfinite_zeroed}
    report = {
        "sample_rate": graph.sample_rate,
        "channels": graph.channels,
        "frames": graph.frames,
        "duration": graph.duration,
        "blocks_requested": sequence,
        "block_count": pull_count,
        "engine_windows": engine_windows,
        "coalesce_max_frames": coalesce_max,
        "tracks": tracks,
        "buses": buses,
        "master": meters[graph.master].summary(),
        "curves": _curve_rows(graph),
        "alignment": alignment_summary(graph),
        "sources": inventory,
        "short_sources": short_sources,
        "nonfinite": nonfinite,
        "raw": str(raw_path) if raw_path else None,
    }
    if report_path:
        _write_json(report_path, report)
    return report
