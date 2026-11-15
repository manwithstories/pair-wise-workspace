"""Streaming-to-disk offline render driver."""

from __future__ import annotations

import json
from pathlib import Path

from .analysis import MetricsAccumulator
from .curve import curve_stats
from .engine import Engine, RenderState
from .io import WavSourceProvider, WavStreamWriter, open_raw_writer, write_raw
from .model import MixGraph
from .resample import alignment_summary


def _source_inventory(graph: MixGraph, infos: dict) -> tuple[list[dict], list[dict]]:
    inventory: list[dict] = []
    short: list[dict] = []
    by_path = {node.source.path: node for node in graph.nodes if node.kind == "source" and node.source is not None}
    for path, info in infos.items():
        node = by_path[path]
        assert node.source is not None
        duration = info.frames / info.sample_rate if info.sample_rate else 0.0
        tail = max(0.0, graph.duration - (node.source.start_sec + duration))
        row = {
            **info.to_dict(),
            "source_id": node.node_id,
            "start_sec": node.source.start_sec,
            "duration_sec": duration,
            "zero_padded_tail_sec": tail,
            "empty": info.frames == 0,
        }
        inventory.append(row)
        if tail > 0.0 or info.frames == 0:
            short.append(
                {
                    "source_id": node.node_id,
                    "path": path,
                    "reason": "empty_source" if info.frames == 0 else "short_source",
                    "zero_padded_tail_sec": tail,
                }
            )
    return inventory, short


def _curve_rows(graph: MixGraph) -> list[dict]:
    rows: list[dict] = []
    for node in graph.nodes:
        for curve in node.curves:
            stats = curve_stats(curve, graph.duration, graph.sample_rate)
            rows.append({**stats.to_dict(), "node_id": node.node_id, "domain": curve.domain})
    return rows


def _write_json(path: str | Path, data: dict) -> None:
    output = Path(path)
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open("w", encoding="utf-8") as handle:
        json.dump(data, handle, indent=2, ensure_ascii=False)
        handle.write("\n")


def render_offline(
    graph: MixGraph,
    base_dir: str | Path,
    out_wav: str | Path | None = None,
    raw_path: str | Path | None = None,
    report_path: str | Path | None = None,
    block_size: int = 8192,
    bit_depth: int | str = 24,
    window_ms: float = 400.0,
) -> dict:
    if block_size < 1:
        raise ValueError("block_size must be positive")
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
    wav_writer = WavStreamWriter(out_wav, graph.sample_rate, graph.channels, bit_depth) if out_wav else None
    raw_handle = open_raw_writer(raw_path) if raw_path else None
    try:
        start = 0
        while start < graph.frames:
            end = min(graph.frames, start + block_size)
            result = engine.render(start, end, output_ids, state)
            state = result.state
            if wav_writer is not None:
                wav_writer.write(result.signals[graph.master])
            if raw_handle is not None:
                write_raw(raw_handle, result.signals[graph.master])
            for node_id, meter in meters.items():
                meter.add(result.signals[node_id])
            for node_id, duck in result.duck_db.items():
                if duck.size:
                    duck_max[node_id] = max(duck_max.get(node_id, 0.0), float(duck.max()))
            start = end
    finally:
        provider.close()
        if wav_writer is not None:
            wav_writer.close()
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
        "block_size": block_size,
        "tracks": tracks,
        "buses": buses,
        "master": meters[graph.master].summary(),
        "curves": _curve_rows(graph),
        "alignment": alignment_summary(graph),
        "sources": inventory,
        "short_sources": short_sources,
        "nonfinite": nonfinite,
        "wav": str(out_wav) if out_wav else None,
        "raw": str(raw_path) if raw_path else None,
    }
    if report_path:
        _write_json(report_path, report)
    if out_wav:
        _write_json(
            f"{out_wav}.stems.json",
            {
                "graph": graph.to_dict(),
                "tracks": tracks,
                "buses": buses,
                "curves": report["curves"],
                "alignment": report["alignment"],
                "sources": inventory,
                "short_sources": short_sources,
                "nonfinite": nonfinite,
            },
        )
    return report
