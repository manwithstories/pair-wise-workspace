"""Command-line interface for fixture generation, rendering, and analysis."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import numpy as np

from .analysis import analyze_wav
from .fixtures import generate_fixtures
from .io import AudioIOError
from .model import GraphError, load_graph
from .offline import render_offline
from .stream import render_stream


def _bit_depth(value: str) -> int | str:
    if value == "float":
        return "float"
    try:
        depth = int(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("bit depth must be 16, 24, 32, or float") from exc
    if depth not in (16, 24, 32):
        raise argparse.ArgumentTypeError("bit depth must be 16, 24, 32, or float")
    return depth


def _positive_float(value: str) -> float:
    try:
        number = float(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("expected a number") from exc
    if number <= 0:
        raise argparse.ArgumentTypeError("value must be positive")
    return number


def _table(headers: list[str], rows: list[list[object]]) -> str:
    text_rows = [[str(item) for item in row] for row in rows]
    widths = [len(header) for header in headers]
    for row in text_rows:
        widths = [max(width, len(cell)) for width, cell in zip(widths, row)]
    lines = ["  ".join(header.rjust(width) for header, width in zip(headers, widths))]
    lines.extend("  ".join(cell.rjust(width) for cell, width in zip(row, widths)) for row in text_rows)
    return "\n".join(lines)


def _print_render(report: dict, title: str) -> None:
    print(f"{title}: frames={report['frames']} rate={report['sample_rate']} channels={report['channels']}")
    rows = []
    for row in report["tracks"].values():
        rows.append([row["track_id"], f"{row['peak_dbfs']:.2f}", f"{row['rms_dbfs']:.2f}", row["clip_samples"], row["clip_runs"], f"{row['true_peak_dbtp']:.2f}"])
    if rows:
        print("\nTRACKS")
        print(_table(["track_id", "peak_dbfs", "rms_dbfs", "clip_samples", "clip_runs", "true_peak_dbtp"], rows))
    bus_rows = []
    for row in report["buses"].values():
        bus_rows.append([row["bus_id"], f"{row['peak_dbfs']:.2f}", f"{row['rms_dbfs']:.2f}", f"{row['duck_max_db']:.2f}"])
    if bus_rows:
        print("\nBUSES")
        print(_table(["bus_id", "peak_dbfs", "rms_dbfs", "duck_max_db"], bus_rows))
    master = report["master"]
    print(f"\nMASTER peak_dbfs={master['peak_dbfs']:.2f} rms_dbfs={master['rms_dbfs']:.2f} true_peak_dbtp={master['true_peak_dbtp']:.2f}")
    align = report["alignment"]
    print("\nALIGNMENT")
    alignment_rows = [
        [row["source_id"], row["source_rate"], row["start_frame"], f"{row['fractional_delay_samples']:.3f}", f"{row['max_alignment_error_samples']:.6f}"]
        for row in align["sources"]
    ]
    print(_table(["source_id", "source_rate", "start_frame", "fractional_delay", "max_error"], alignment_rows) if alignment_rows else "(no sources)")
    print(f"NONFINITE zeroed={report['nonfinite']['total']} short_sources={len(report['short_sources'])}")


def _read_raw(path: str, channels: int, block_frames: int = 65536):
    with open(path, "rb") as handle:
        while True:
            raw = handle.read(block_frames * channels * 4)
            if not raw:
                return
            if len(raw) % (channels * 4):
                raise AudioIOError(f"raw file {path} has a partial float32 sample")
            yield np.frombuffer(raw, dtype="<f4").astype(np.float64).reshape(-1, channels)


def compare_raw(path_a: str, path_b: str, tol: float, channels: int = 2) -> dict:
    if channels < 1:
        raise ValueError("channels must be positive")
    max_diff = 0.0
    first_mismatch = -1
    frame = 0
    left_iter = _read_raw(path_a, channels)
    right_iter = _read_raw(path_b, channels)
    while True:
        left = next(left_iter, None)
        right = next(right_iter, None)
        if left is None and right is None:
            break
        if left is None or right is None:
            if first_mismatch < 0:
                first_mismatch = frame
            max_diff = float("inf")
            break
        diff = np.abs(left - right)
        chunk_max = float(np.max(diff)) if diff.size else 0.0
        max_diff = max(max_diff, chunk_max)
        if first_mismatch < 0:
            bad = np.flatnonzero(np.any(diff > tol, axis=1))
            if bad.size:
                first_mismatch = frame + int(bad[0])
        frame += len(left)
    return {
        "max_abs_diff": max_diff,
        "first_mismatch": first_mismatch,
        "tol": tol,
        "passed": max_diff <= tol,
        "channels": channels,
        "frames": frame,
    }


def _load_stems(path: str) -> dict:
    sidecar = Path(f"{path}.stems.json")
    if not sidecar.exists():
        return {"tracks": [], "buses": [], "curves": [], "alignment": {}, "sources": [], "short_sources": [], "nonfinite": {"total": 0}}
    with sidecar.open("r", encoding="utf-8") as handle:
        data = json.load(handle)
    for key in ("tracks", "buses"):
        if isinstance(data.get(key), dict):
            data[key] = list(data[key].values())
    return data


def _analyze(input_path: str, window_ms: float, report_path: str | None) -> dict:
    summary = analyze_wav(input_path, window_ms)
    stems = _load_stems(input_path)
    report = {
        "input": input_path,
        "window_ms": window_ms,
        "tracks": stems.get("tracks", []),
        "buses": stems.get("buses", []),
        "curves": stems.get("curves", []),
        "alignment": stems.get("alignment", {}),
        "sources": stems.get("sources", []),
        "short_sources": stems.get("short_sources", []),
        "nonfinite": stems.get("nonfinite", {"total": 0}),
        "summary": summary,
    }
    if report_path:
        output = Path(report_path)
        output.parent.mkdir(parents=True, exist_ok=True)
        with output.open("w", encoding="utf-8") as handle:
            json.dump(report, handle, indent=2, ensure_ascii=False)
            handle.write("\n")
    print("TRACKS")
    track_rows = [
        [r.get("track_id", "?"), f"{r['peak_dbfs']:.2f}", f"{r['rms_dbfs']:.2f}", r["clip_samples"], r["clip_runs"], f"{r['true_peak_dbtp']:.2f}"]
        for r in report["tracks"]
    ]
    print(_table(["track_id", "peak_dbfs", "rms_dbfs", "clip_samples", "clip_runs", "true_peak_dbtp"], track_rows) if track_rows else "(no stem metadata)")
    print("\nBUSES")
    bus_rows = [
        [r.get("bus_id", "?"), f"{r['peak_dbfs']:.2f}", f"{r['rms_dbfs']:.2f}", f"{r.get('duck_max_db', 0.0):.2f}"]
        for r in report["buses"]
    ]
    print(_table(["bus_id", "peak_dbfs", "rms_dbfs", "duck_max_db"], bus_rows) if bus_rows else "(no bus metadata)")
    print("\nCURVES")
    curve_rows = [
        [r["curve_id"], r["samples"], f"{r['min_db']:.2f}", f"{r['max_db']:.2f}", f"{r['first_value']:.2f}"]
        for r in report["curves"]
    ]
    print(_table(["curve_id", "samples", "min_db", "max_db", "first_value"], curve_rows) if curve_rows else "(no curve metadata)")
    print("\nALIGNMENT")
    alignment = report.get("alignment", {})
    alignment_rows = [
        [row["source_id"], row["source_rate"], row["start_frame"], f"{row['fractional_delay_samples']:.3f}", f"{row['max_alignment_error_samples']:.6f}"]
        for row in alignment.get("sources", [])
    ]
    print(_table(["source_id", "source_rate", "start_frame", "fractional_delay", "max_error"], alignment_rows) if alignment_rows else "(no alignment metadata)")
    print(
        "\nSUMMARY "
        f"master peak_dbfs={summary['peak_dbfs']:.2f} rms_dbfs={summary['rms_dbfs']:.2f} "
        f"clip_samples={summary['clip_samples']} clip_runs={summary['clip_runs']} "
        f"true_peak_dbtp={summary['true_peak_dbtp']:.2f} nonfinite={report['nonfinite'].get('total', 0)}"
    )
    return report


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="clipgraph", description="Offline/streaming audio graph renderer")
    sub = parser.add_subparsers(dest="command", required=True)
    gen = sub.add_parser("gen-fixtures", help="generate a deterministic demo session")
    gen.add_argument("--out", required=True)
    gen.add_argument("--seed", type=int, default=7)
    render = sub.add_parser("render", help="render the full graph offline")
    render.add_argument("--graph", required=True)
    render.add_argument("--out", required=True)
    render.add_argument("--raw")
    render.add_argument("--report")
    render.add_argument("--block-size", type=int, default=8192)
    render.add_argument("--bit-depth", type=_bit_depth, default=24)
    render.add_argument("--window-ms", type=_positive_float, default=400.0)
    stream = sub.add_parser("stream", help="render with variable pull sizes")
    stream.add_argument("--graph", required=True)
    stream.add_argument("--blocks", required=True)
    stream.add_argument("--raw")
    stream.add_argument("--report")
    stream.add_argument("--window-ms", type=_positive_float, default=400.0)
    compare = sub.add_parser("compare", help="compare two float32 raw files")
    compare.add_argument("--a", required=True)
    compare.add_argument("--b", required=True)
    compare.add_argument("--tol", type=_positive_float, default=1e-9)
    compare.add_argument("--channels", type=int, default=2)
    analyze = sub.add_parser("analyze", help="measure a rendered WAV")
    analyze.add_argument("--input", required=True)
    analyze.add_argument("--window-ms", type=_positive_float, default=400.0)
    analyze.add_argument("--report")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "gen-fixtures":
            result = generate_fixtures(args.out, args.seed)
            print(f"generated graph={result['graph']} assets={result['assets']} seed={result['seed']}")
            return 0
        if args.command == "render":
            graph = load_graph(args.graph)
            report = render_offline(
                graph,
                Path(args.graph).resolve().parent,
                out_wav=args.out,
                raw_path=args.raw,
                report_path=args.report,
                block_size=args.block_size,
                bit_depth=args.bit_depth,
                window_ms=args.window_ms,
            )
            _print_render(report, "RENDER")
            return 0
        if args.command == "stream":
            graph = load_graph(args.graph)
            report = render_stream(
                graph,
                Path(args.graph).resolve().parent,
                args.blocks,
                raw_path=args.raw,
                report_path=args.report,
                window_ms=args.window_ms,
            )
            _print_render(report, "STREAM")
            return 0
        if args.command == "compare":
            result = compare_raw(args.a, args.b, args.tol, args.channels)
            print(f"max_abs_diff: {result['max_abs_diff']:.3e}")
            print(f"first_mismatch: {result['first_mismatch']}")
            return 0 if result["passed"] else 3
        if args.command == "analyze":
            _analyze(args.input, args.window_ms, args.report)
            return 0
    except GraphError as exc:
        print(f"graph error: {exc}", file=sys.stderr)
        return 2
    except AudioIOError as exc:
        print(f"audio error: {exc}", file=sys.stderr)
        return 4
    except (ValueError, OSError, json.JSONDecodeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    return 2
