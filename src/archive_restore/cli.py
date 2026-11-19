"""Command line interface: fixtures / restore / verify.

Exit codes:
    0  success (verification passed)
    2  input format or index error
    3  checkpoint mismatch (refused resume)
    4  output verification failed
"""

from __future__ import annotations

import argparse
import json
import sys

from .pipeline import (
    EXIT_INPUT,
    EXIT_OK,
    run_restore,
    run_verify,
)


def _print(summary: dict, as_json: bool) -> None:
    if as_json:
        print(json.dumps(summary, indent=2, sort_keys=True))
    else:
        for key, value in summary.items():
            if isinstance(value, (dict, list)):
                print(f"{key}: {json.dumps(value)}")
            else:
                print(f"{key}: {value}")


def _cmd_fixtures(args: argparse.Namespace) -> int:
    from .fixtures import write_fixtures

    meta = write_fixtures(args.out, n_packets=args.packets, seed=args.seed)
    summary = {
        "out": args.out,
        "packets": args.packets,
        "files": meta["files"],
        "damages": meta["damages"],
    }
    _print(summary, args.json)
    return EXIT_OK


def _cmd_restore(args: argparse.Namespace) -> int:
    summary, code = run_restore(
        input_path=args.input,
        index_path=args.index,
        state_dir=args.state,
        window=args.window,
        fallback=args.fallback,
        output_path=args.output,
    )
    _print(summary, args.json)
    return code


def _cmd_verify(args: argparse.Namespace) -> int:
    summary, code = run_verify(args.input, args.index)
    _print(summary, args.json)
    return code


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="archive_restore",
        description="Recover RTP/RTCP archival streams: parse, repair, "
                    "index, resume.",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    fx = sub.add_parser("fixtures", help="generate sample streams")
    fx.add_argument("--out", required=True, help="output directory")
    fx.add_argument("--packets", type=int, default=10_000)
    fx.add_argument("--seed", type=int, default=12797)
    fx.add_argument("--json", action="store_true")
    fx.set_defaults(func=_cmd_fixtures)

    rs = sub.add_parser("restore", help="recover a stream (resumable)")
    rs.add_argument("--input", required=True)
    rs.add_argument("--index", required=True, help="index path (rebuilt)")
    rs.add_argument("--state", required=True, help="checkpoint directory")
    rs.add_argument("--window", type=int, default=64,
                    help="reorder window in packets")
    rs.add_argument("--fallback", default="zero",
                    help="clock fallback when no RTCP SR (default: zero)")
    rs.add_argument("--output", default=None,
                    help="recovered stream output path")
    rs.add_argument("--json", action="store_true")
    rs.set_defaults(func=_cmd_restore)

    vf = sub.add_parser("verify", help="validate an index against input")
    vf.add_argument("--input", required=True)
    vf.add_argument("--index", required=True)
    vf.add_argument("--json", action="store_true")
    vf.set_defaults(func=_cmd_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except BrokenPipeError:
        return EXIT_OK
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_INPUT


if __name__ == "__main__":
    sys.exit(main())
