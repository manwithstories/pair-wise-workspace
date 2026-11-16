"""Command-line assembly for gen, scan, rebuild, and verify."""

from __future__ import annotations

import argparse
import sys
from pathlib import Path
from typing import Sequence

from .fixtures import WRITERS, generate_fixture
from .rebuild import rebuild_sidx
from .report import (
    build_report,
    print_rebuild_summary,
    print_scan_summary,
    print_verify_summary,
    write_json,
)
from .verify import exit_code, scan_path, verify_scan


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="sidxcheck", description="fMP4 sidx checker")
    commands = parser.add_subparsers(dest="command", required=True)

    gen = commands.add_parser("gen", help="generate a self-contained fixture directory")
    gen.add_argument("--out", required=True, help="fixture output directory")
    gen.add_argument("--segments", type=int, default=12)
    gen.add_argument("--writer", choices=sorted(WRITERS), default="normal")

    scan = commands.add_parser("scan", help="scan a directory and print a summary")
    scan.add_argument("input")
    scan.add_argument("--json", help="optional JSON report path")

    rebuild = commands.add_parser("rebuild", help="rebuild sidx bytes from measured moofs")
    rebuild.add_argument("input")
    rebuild.add_argument("--out", required=True, help="rebuilt sidx output")
    rebuild.add_argument("--report", help="JSON report output")

    verify = commands.add_parser("verify", help="cross-check indexes, offsets, and samples")
    verify.add_argument("input")
    verify.add_argument("--json", help="JSON report output")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "gen":
            path = generate_fixture(args.out, segments=args.segments, writer=args.writer)
            print(f"generated={path}")
            return 0
        if args.command == "scan":
            scan = scan_path(args.input)
            verification = verify_scan(scan)
            print_scan_summary(scan, sys.stdout)
            if args.json:
                write_json(args.json, build_report(scan, verification=verification))
            return exit_code(verification)
        if args.command == "rebuild":
            scan = scan_path(args.input)
            verification = verify_scan(scan)
            rebuilt = rebuild_sidx(
                scan.moofs, sidxes=scan.sidxes, sample_tables=scan.sample_tables
            )
            output = Path(args.out)
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_bytes(rebuilt.data)
            if args.report:
                write_json(
                    args.report,
                    build_report(scan, verification=verification, rebuilt=rebuilt),
                )
            print_rebuild_summary(rebuilt, sys.stdout)
            if rebuilt.issues:
                return 1
            return 3 if scan.fatal_count else 0
        if args.command == "verify":
            scan = scan_path(args.input)
            verification = verify_scan(scan)
            if args.json:
                write_json(args.json, build_report(scan, verification=verification))
            print_verify_summary(verification, sys.stdout)
            return exit_code(verification)
    except (OSError, ValueError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
