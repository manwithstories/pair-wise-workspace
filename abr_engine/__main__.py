"""Command-line entry point: ``python -m abr_engine <subcommand>``.

Exit codes (per the requirements):

* ``0`` — simulation completed and the result was written;
* ``2`` — illegal arguments or an illegal trace (validation errors);
* ``3`` — an unexpected failure while running the simulation.

Two subcommands are provided:

* ``generate-trace`` — write a reproducible seeded network trace JSON;
* ``simulate``        — run the ABR loop over a trace and print both the
  aggregate ``KEY=value`` summary and one line per segment.
"""

from __future__ import annotations

import argparse
import sys
from dataclasses import asdict
from pathlib import Path

from .estimator import BandwidthEstimator  # noqa: F401  (documented re-export)
from .io import (
    load_trace,
    parse_bitrates,
    save_result,
    save_trace,
)
from .metrics import summarize
from .models import (
    ABRConfigError,
    ABRError,
    ABRSimulationError,
    ABRTraceError,
    SimulationConfig,
)
from .policy import ABRPolicy  # noqa: F401  (documented re-export)
from .simulator import Simulator
from .trace import generate_trace

EXIT_OK = 0
EXIT_INVALID = 2
EXIT_RUNTIME = 3

# Beyond this many segments, per-segment lines flood the terminal; the
# JSON result always retains every segment regardless of this threshold.
PRINT_LIMIT = 2000


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m abr_engine",
        description="Local adaptive-bitrate (ABR) simulation engine.",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser(
        "generate-trace", help="generate a reproducible network trace"
    )
    gen.add_argument("--segments", type=int, required=True)
    gen.add_argument("--segment-seconds", type=float, default=2.0)
    gen.add_argument("--seed", type=int, required=True)
    gen.add_argument("--output", type=Path, required=True)
    gen.add_argument("--name", default="trace")

    sim = sub.add_parser("simulate", help="run an ABR simulation on a trace")
    sim.add_argument("--trace", type=Path, required=True)
    sim.add_argument("--bitrates", type=str, required=True,
                     help="comma-separated ascending kbps levels, e.g. 800,1600")
    sim.add_argument("--initial-buffer", type=float, default=6.0)
    sim.add_argument("--max-buffer", type=float, default=30.0)
    sim.add_argument("--window", type=int, default=6)
    sim.add_argument("--upshift-confirm", type=int, default=3)
    sim.add_argument("--downshift-confirm", type=int, default=1)
    sim.add_argument("--safety-margin", type=float, default=0.85)
    sim.add_argument("--output", type=Path, required=True)
    sim.add_argument("--quiet", action="store_true",
                     help="suppress per-segment lines (summary still printed)")
    return parser


def _cmd_generate_trace(args: argparse.Namespace) -> int:
    trace = generate_trace(
        segments=args.segments,
        segment_seconds=args.segment_seconds,
        seed=args.seed,
        name=args.name,
    )
    path = save_trace(trace, args.output)
    print(f"TRACE_FILE={path}")
    print(f"SEGMENTS={len(trace)}")
    print(f"SEGMENT_SECONDS={trace.segment_seconds}")
    print(f"TRACE_SEED={trace.seed}")
    return EXIT_OK


def _print_segment_rows(decisions, quiet: bool) -> None:
    if quiet:
        return
    print("SEG\tBITRATE\tEST_BW\tBUFFER\tDOWNLOAD\tSWITCH\tSTALL")
    for decision in decisions[:PRINT_LIMIT]:
        print(
            f"{decision.index}\t{decision.bitrate_kbps}\t"
            f"{decision.est_bandwidth_kbps:.1f}\t"
            f"{decision.buffer_seconds:.2f}\t"
            f"{decision.download_seconds:.3f}\t"
            f"{int(decision.switch)}\t{int(decision.stall)}"
        )
    if len(decisions) > PRINT_LIMIT:
        print(f"... {len(decisions) - PRINT_LIMIT} more segments in JSON output")


def _cmd_simulate(args: argparse.Namespace) -> int:
    bitrates = parse_bitrates(args.bitrates)
    trace = load_trace(args.trace)

    config = SimulationConfig(
        bitrates=bitrates,
        segment_seconds=trace.segment_seconds,
        initial_buffer=args.initial_buffer,
        max_buffer=args.max_buffer,
        window=args.window,
        upshift_confirm=args.upshift_confirm,
        downshift_confirm=args.downshift_confirm,
        safety_margin=args.safety_margin,
        seed=trace.seed,
    )
    simulator = Simulator(config)
    decisions = simulator.run(trace)
    summary = summarize(decisions)

    save_result(
        trace=trace,
        decisions=decisions,
        summary=summary,
        config={
            key: value
            for key, value in asdict(config).items()
            if key != "extras"
        },
        path=args.output,
    )

    print(f"SEGMENTS={summary.segments}")
    print(f"SWITCHES={summary.switch_count}")
    print(f"STALL_COUNT={summary.stall_episode_count}")
    print(f"STALL_SECONDS={summary.stall_seconds}")
    print(f"AVG_BITRATE={summary.average_bitrate_kbps}")
    print(f"TRACE_SEED={trace.seed if trace.seed is not None else 'none'}")
    print(f"RESULT_FILE={args.output}")
    _print_segment_rows(decisions, args.quiet)
    return EXIT_OK


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate-trace":
            return _cmd_generate_trace(args)
        if args.command == "simulate":
            return _cmd_simulate(args)
        parser.error(f"unknown command {args.command!r}")
        return EXIT_INVALID
    except (ABRConfigError, ABRTraceError) as exc:
        print(f"ERROR [{type(exc).__name__}]: {exc}", file=sys.stderr)
        return EXIT_INVALID
    except ABRSimulationError as exc:
        print(f"ERROR [ABRSimulationError]: {exc}", file=sys.stderr)
        return EXIT_RUNTIME
    except ABRError as exc:  # pragma: no cover - safety net for base type
        print(f"ERROR [{type(exc).__name__}]: {exc}", file=sys.stderr)
        return EXIT_INVALID


if __name__ == "__main__":
    raise SystemExit(main())
