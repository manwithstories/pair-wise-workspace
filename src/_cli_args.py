"""Argument parsing and input resolution for the CLI."""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from . import __version__
from .ebml_schema import Strictness
from .fixture_builder import demo_spec, write_fixture


def resolve_input(args: argparse.Namespace) -> Path:
    """Decide where the source bytes come from: fixture, file, or stdin."""
    if getattr(args, "generate_fixture", False):
        target = Path(args.fixture_path)
        write_fixture(target, demo_spec())
        return target
    if getattr(args, "stdin", False):
        data = sys.stdin.buffer.read()
        if not data:
            raise CliError("input_empty", "stdin produced no bytes", EXIT_INPUT)
        target = Path(args.fixture_path)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        return target
    if not args.input:
        raise CliError(
            "input_required",
            "provide --input <file>, --stdin, or --generate-fixture",
            EXIT_INPUT,
        )
    return Path(args.input)


def strictness_from(args: argparse.Namespace) -> Strictness:
    return Strictness.LENIENT if getattr(args, "lenient", False) else Strictness.STRICT


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mkv2mp4",
        description="Inspect Matroska files and remux them to MP4 without re-encoding.",
    )
    parser.add_argument("--version", action="version", version=f"mkv2mp4 {__version__}")
    sub = parser.add_subparsers(dest="command", required=True)

    inspect_p = sub.add_parser("inspect", help="summarize a Matroska file")
    _add_input_flags(inspect_p)

    remux_p = sub.add_parser("remux", help="write an MP4 from a Matroska file")
    _add_input_flags(remux_p)
    remux_p.add_argument("--output", "-o", required=True, help="destination MP4 path")
    remux_p.add_argument("--chunk-samples", type=int, default=10, help="samples per chunk (default 10)")
    remux_p.add_argument("--no-normalize", action="store_true", help="do not shift negative timestamps")
    remux_p.add_argument(
        "--strict-timestamps",
        action="store_true",
        help="fail on duplicate timestamps instead of repairing them",
    )
    remux_p.add_argument(
        "--force-monotonic-pts",
        action="store_true",
        help="flatten B-frame reordering into a monotonic presentation order",
    )
    remux_p.add_argument(
        "--allow-unsupported-codecs",
        action="store_true",
        help="skip tracks with unmappable CodecIDs instead of failing (implied by --lenient)",
    )
    remux_p.add_argument("--drop-subtitles", action="store_true", help="exclude subtitle tracks")
    remux_p.add_argument("--verify", action="store_true", help="verify the written file before exiting")

    verify_p = sub.add_parser("verify", help="check a written MP4")
    verify_p.add_argument("--input", "-i", required=True, help="MP4 path to verify")
    verify_p.add_argument("--source", help="original Matroska file for byte comparison")
    verify_p.add_argument("--expect-sha256", help="expected digest of the MP4")
    verify_p.add_argument("--lenient", action="store_true", help="tolerate unknown EBML elements")
    verify_p.add_argument("--json", action="store_true", help="emit JSON instead of a summary")
    return parser


def _add_input_flags(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--input", "-i", help="Matroska file to read")
    parser.add_argument(
        "--generate-fixture",
        action="store_true",
        help="generate the demo fixture instead of reading a file",
    )
    parser.add_argument(
        "--fixture-path",
        default="outputs/fixture.mkv",
        help="where --generate-fixture (or --stdin) stages its bytes",
    )
    parser.add_argument(
        "--stdin",
        action="store_true",
        help="read the Matroska stream from stdin (staged at --fixture-path)",
    )
    parser.add_argument("--lenient", action="store_true", help="tolerate unknown EBML elements")
    parser.add_argument("--json", action="store_true", help="emit JSON instead of a summary")
    parser.add_argument("--json-out", help="also write the JSON payload to this path")


__all__ = ["build_parser", "resolve_input", "strictness_from"]
