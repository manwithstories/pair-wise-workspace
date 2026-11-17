"""Command-line entry point: argument parsing, orchestration and exit codes.

Exit code contract (single source: :mod:`src.config`):
  0 success; 2 parameter error; 3 numeric/memory constraint failure;
  4 offline/streaming path or tail-length disagreement.
The module exposes :func:`main` returning an int so the unittest suite can
drive the CLI in-process; ``__main__`` maps that to a process exit code.
"""
from __future__ import annotations

import argparse
import sys

import numpy as np

from . import metrics as mt
from .config import (
    DEFAULT_MEMORY_BUDGET_BYTES,
    EXIT_NUMERIC_OR_MEMORY,
    EXIT_OK,
    EXIT_PARAM_ERROR,
    EXIT_PATH_OR_TAIL_MISMATCH,
    ConfigError,
    MemoryGuardError,
    NumericConstraintError,
    PATH_MAX_ABS_ERROR_TOLERANCE,
    PathMismatchError,
    RenderConfig,
    StreamBlockError,
    validate_render_config,
)
from .io_audio import write_json, write_wav
from .render_paths import render_offline, render_streaming
from .signal_gen import build_scene


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="reverb",
        description="Deterministic block-FFT convolution reverb renderer.",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("render", help="generate a scene and render reverb")
    p.add_argument("--duration", type=float, required=True,
                   help="input length in seconds (> 0)")
    p.add_argument("--sample-rate", type=int, required=True)
    p.add_argument("--channels", type=int, required=True, choices=(1, 2))
    p.add_argument("--block-size", type=int, required=True)
    p.add_argument("--ir-length", type=int, required=True)
    p.add_argument("--seed", type=int, required=True)
    p.add_argument("--json-output", type=str, default=None,
                   help="write the full structured result to this JSON path")
    p.add_argument("--no-wav", action="store_true",
                   help="skip writing input/IR/output WAV files")
    p.add_argument("--memory-budget", type=int,
                   default=DEFAULT_MEMORY_BUDGET_BYTES,
                   help="max FFT workspace bytes before the job is rejected")
    return parser


def _build_config(ns) -> RenderConfig:
    return RenderConfig(
        sample_rate=ns.sample_rate,
        channels=ns.channels,
        block_size=ns.block_size,
        ir_length=ns.ir_length,
        duration=ns.duration,
        seed=ns.seed,
        memory_budget_bytes=ns.memory_budget,
    )


def _render_payload(cfg: RenderConfig):
    scene = build_scene(cfg)
    source, ir = scene["input"], scene["ir"]

    stream_errors: list = []
    offline = render_offline(source, ir, cfg.block_size)
    streaming = render_streaming(
        source, ir, cfg.block_size, on_error=stream_errors.append
    )

    # Hard numeric gate first — NaN/Inf must never masquerade as a result.
    mt.assert_healthy(offline.output)
    mt.assert_healthy(streaming.output)

    path_error = mt.assert_path_agreement(
        offline.output, streaming.output, PATH_MAX_ABS_ERROR_TOLERANCE
    )

    expected_len = mt.expected_convolution_length(
        cfg.input_samples, cfg.ir_length
    )
    if streaming.output.shape[0] != expected_len:
        raise PathMismatchError(
            f"tail length mismatch: rendered {streaming.output.shape[0]} "
            f"samples, expected N+M-1={expected_len}"
        )

    anomalies = mt.scan_anomalies(streaming.records, streaming.output)
    if anomalies["global_nan"] or anomalies["global_inf"]:
        raise NumericConstraintError("aggregate NaN/Inf detected in output")

    timing = mt.block_time_stats(streaming.records)
    payload = {
        "status": "ok",
        "config": {
            "sample_rate": cfg.sample_rate,
            "channels": cfg.channels,
            "block_size": cfg.block_size,
            "ir_length": cfg.ir_length,
            "duration": cfg.duration,
            "seed": cfg.seed,
        },
        "ir_length": cfg.ir_length,
        "block_size": cfg.block_size,
        "input_samples": cfg.input_samples,
        "output_samples": streaming.output.shape[0],
        "blocks": streaming.block_count,
        "block_time_ms_min": timing["min_ms"],
        "block_time_ms_max": timing["max_ms"],
        "block_time_ms_mean": timing["mean_ms"],
        "output_peak": mt.output_peak(streaming.output),
        "output_energy": mt.output_energy(streaming.output),
        "path_max_abs_error": path_error,
        "anomalies": anomalies,
        "blocks_detail": [vars(r) for r in streaming.records],
        "wav": [],
        "stream_errors": stream_errors,
    }
    return source, ir, streaming.output, payload


def _print_summary(payload: dict) -> None:
    order = [
        "ir_length", "block_size", "input_samples", "output_samples", "blocks",
        "block_time_ms_min", "block_time_ms_max", "block_time_ms_mean",
        "output_peak", "output_energy", "path_max_abs_error",
    ]
    for key in order:
        value = payload[key]
        if isinstance(value, float):
            print(f"{key}={value:.6f}")
        else:
            print(f"{key}={value}")
    a = payload["anomalies"]
    print(f"nan_blocks={a['nan_block_indexes']} inf_blocks={a['inf_block_indexes']} "
          f"peak_over_one_blocks={len(a['peak_over_one_block_indexes'])}")


def run_render(ns) -> int:
    try:
        cfg = _build_config(ns)
        validate_render_config(cfg)
    except ConfigError as exc:
        print(f"error: parameter error: {exc}", file=sys.stderr)
        if ns.json_output:
            write_json(ns.json_output,
                       {"status": "error", "exit_code": EXIT_PARAM_ERROR,
                        "error": {"code": "config_error", "message": str(exc)}})
        return EXIT_PARAM_ERROR
    except MemoryGuardError as exc:
        print(f"error: memory guard: {exc}", file=sys.stderr)
        if ns.json_output:
            write_json(ns.json_output,
                       {"status": "error", "exit_code": EXIT_NUMERIC_OR_MEMORY,
                        "error": {"code": "memory_guard", "message": str(exc)}})
        return EXIT_NUMERIC_OR_MEMORY

    try:
        source, ir, output, payload = _render_payload(cfg)
    except StreamBlockError as exc:
        print(f"error: stream block failure: {exc}", file=sys.stderr)
        if ns.json_output:
            write_json(ns.json_output,
                       {"status": "error", "exit_code": EXIT_NUMERIC_OR_MEMORY,
                        "error": {"code": "stream_block",
                                  "message": str(exc), "details": exc.details}})
        return EXIT_NUMERIC_OR_MEMORY
    except PathMismatchError as exc:
        print(f"error: path/tail mismatch: {exc}", file=sys.stderr)
        if ns.json_output:
            write_json(ns.json_output,
                       {"status": "error",
                        "exit_code": EXIT_PATH_OR_TAIL_MISMATCH,
                        "error": {"code": "path_mismatch",
                                  "message": str(exc)}})
        return EXIT_PATH_OR_TAIL_MISMATCH
    except NumericConstraintError as exc:
        print(f"error: numeric constraint: {exc}", file=sys.stderr)
        if ns.json_output:
            write_json(ns.json_output,
                       {"status": "error",
                        "exit_code": EXIT_NUMERIC_OR_MEMORY,
                        "error": {"code": "numeric_constraint",
                                  "message": str(exc)}})
        return EXIT_NUMERIC_OR_MEMORY

    if not ns.no_wav and ns.json_output:
        base = ns.json_output.rsplit(".", 1)[0]
        for tag, arr in (("input", source), ("ir", ir), ("output", output)):
            payload["wav"].append(
                write_wav(f"{base}_{tag}.wav", arr, cfg.sample_rate)
            )

    _print_summary(payload)
    if ns.json_output:
        path = write_json(ns.json_output, payload)
        print(f"json_output={path}")
    return EXIT_OK


def main(argv=None) -> int:
    parser = _build_parser()
    # argparse itself exits 2 on bad arguments, matching our parameter code.
    ns = parser.parse_args(argv)
    if ns.command == "render":
        return run_render(ns)
    parser.error(f"unknown command {ns.command!r}")
    return EXIT_PARAM_ERROR  # pragma: no cover - parser.error exits


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
