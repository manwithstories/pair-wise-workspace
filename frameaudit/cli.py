"""Command-line entry point for frameaudit."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Sequence

from frameaudit.errors import FrameAuditError, InputError, ProcessingError
from frameaudit.fixtures import generate_fixtures
from frameaudit.pipeline import repair_video, scan_video
from frameaudit.report import fixtures_text, repair_text, scan_text, verify_text
from frameaudit.verify import verify_outputs


EXIT_OK = 0
EXIT_INPUT = 2
EXIT_PROCESSING = 3
EXIT_VERIFY = 4


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="frameaudit",
        description="Offline frame audit, timeline repair, and CFR proxy generation",
    )
    commands = parser.add_subparsers(dest="command", required=True)

    fixtures = commands.add_parser("fixtures", help="generate deterministic synthetic footage")
    fixtures.add_argument("--out", required=True, type=Path, help="output directory")
    fixtures.add_argument("--seed", type=int, default=7)
    fixtures.add_argument("--frames", type=int, default=1440)

    scan = commands.add_parser("scan", help="decode once and report frame statistics")
    scan.add_argument("input", type=Path)
    scan.add_argument("--json", action="store_true")
    scan.add_argument("--reorder-depth", type=int, default=16)

    repair = commands.add_parser("repair", help="repair timeline and emit index plus CFR proxy")
    repair.add_argument("input", type=Path)
    repair.add_argument("--emit-index", required=True, type=Path)
    repair.add_argument("--emit-proxy", required=True, type=Path)
    repair.add_argument("--json", action="store_true")
    repair.add_argument("--reorder-depth", type=int, default=16)

    verify = commands.add_parser("verify", help="verify index against CFR proxy")
    verify.add_argument("index", type=Path)
    verify.add_argument("proxy", type=Path)
    verify.add_argument("--json", action="store_true")
    return parser


def _emit(report: dict[str, Any], text: str, as_json: bool) -> None:
    if as_json:
        print(json.dumps(report, ensure_ascii=False, indent=2))
    else:
        print(text)


def _validate_depth(depth: int) -> None:
    if depth < 1:
        raise InputError("--reorder-depth must be at least 1")


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        if args.command == "fixtures":
            if args.frames < 2:
                raise InputError("--frames must be at least 2")
            report = generate_fixtures(args.out, seed=args.seed, frames=args.frames)
            _emit(report, fixtures_text(report), False)
            return EXIT_OK

        if args.command == "scan":
            _validate_depth(args.reorder_depth)
            report = scan_video(args.input, args.reorder_depth)
            _emit(report, scan_text(report), args.json)
            return EXIT_OK

        if args.command == "repair":
            _validate_depth(args.reorder_depth)
            report = repair_video(
                args.input,
                args.emit_index,
                args.emit_proxy,
                args.reorder_depth,
            )
            _emit(report, repair_text(report), args.json)
            return EXIT_OK

        if args.command == "verify":
            report = verify_outputs(args.index, args.proxy)
            _emit(report, verify_text(report), args.json)
            return EXIT_OK if report["ok"] else EXIT_VERIFY

        raise InputError(f"unknown command: {args.command}")
    except InputError as exc:
        print(f"frameaudit: input error: {exc}", file=sys.stderr)
        return EXIT_INPUT
    except ProcessingError as exc:
        print(f"frameaudit: processing error: {exc}", file=sys.stderr)
        return EXIT_PROCESSING
    except FrameAuditError as exc:
        print(f"frameaudit: error: {exc}", file=sys.stderr)
        return EXIT_PROCESSING
    except (OSError, ValueError, RuntimeError) as exc:
        print(f"frameaudit: processing error: {exc}", file=sys.stderr)
        return EXIT_PROCESSING

