"""Argument parsing and JSON-only command-line interface."""

from __future__ import annotations

import argparse
import json
import os
import sys
import tempfile
from pathlib import Path
from typing import Any, Sequence

from .audio import check_audio
from .errors import TlcompError, UsageError
from .exporter import diff_manifests, render_project, verify_project
from .fixtures import generate_fixtures


class JsonParser(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        raise UsageError(message)


def _parser() -> JsonParser:
    parser = JsonParser(prog="tlcomp", description="Offline deterministic timeline compositor")
    subcommands = parser.add_subparsers(dest="command", required=True)
    render = subcommands.add_parser("render", help="render frame summaries")
    render.add_argument("project")
    render.add_argument("--out", required=True, help="output directory")
    render.add_argument("--workers", type=int, default=1)
    render.add_argument("--resume", action="store_true")
    verify = subcommands.add_parser("verify", help="recompute and compare a manifest")
    verify.add_argument("project")
    verify.add_argument("--manifest", required=True)
    verify.add_argument("--workers", type=int, default=1)
    diff = subcommands.add_parser("diff", help="compare two manifests")
    diff.add_argument("first")
    diff.add_argument("second")
    audio_check = subcommands.add_parser("audio-check", help="measure audio cue alignment")
    audio_check.add_argument("project")
    audio_check.add_argument("--out", required=True, help="output JSON file")
    fixtures = subcommands.add_parser("fixtures", help="generate the deterministic demo")
    fixtures.add_argument("--out", required=True)
    fixtures.add_argument("--frames", type=int, default=120)
    fixtures.add_argument("--fps", default="30000/1001")
    return parser


def _emit(value: dict[str, Any], path: str | Path | None = None) -> None:
    text = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    if path is not None:
        target = Path(path)
        target.parent.mkdir(parents=True, exist_ok=True)
        descriptor, temporary = tempfile.mkstemp(prefix=f".{target.name}.", suffix=".tmp", dir=target.parent)
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
                handle.write(text + "\n")
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, target)
        except BaseException:
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass
            raise
    sys.stdout.write(text + "\n")


def _run(arguments: argparse.Namespace) -> tuple[dict[str, Any], str | Path | None, int]:
    if arguments.command == "render":
        if arguments.workers <= 0:
            raise UsageError("--workers must be greater than zero")
        result = render_project(
            arguments.project,
            arguments.out,
            workers=arguments.workers,
            resume=arguments.resume,
        )
        return result, None, 0
    if arguments.command == "verify":
        if arguments.workers <= 0:
            raise UsageError("--workers must be greater than zero")
        result = verify_project(arguments.project, arguments.manifest, workers=arguments.workers)
        return result, None, 1 if result["mismatch"] else 0
    if arguments.command == "diff":
        result = diff_manifests(arguments.first, arguments.second)
        return result, None, 1 if result["diff_frames"] else 0
    if arguments.command == "audio-check":
        from .timeline import load_timeline

        return check_audio(load_timeline(arguments.project)), arguments.out, 0
    if arguments.command == "fixtures":
        return generate_fixtures(arguments.out, arguments.frames, arguments.fps), None, 0
    raise UsageError("unknown command")


def main(argv: Sequence[str] | None = None) -> int:
    try:
        arguments = _parser().parse_args(argv)
        outcome = _run(arguments)
        result, output, exit_code = outcome
        _emit(result, output)
        return exit_code
    except TlcompError as exc:
        sys.stdout.write(
            json.dumps(
                {"error": str(exc), "kind": exc.kind},
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            )
            + "\n"
        )
        return exc.exit_code
    except OSError as exc:
        sys.stdout.write(
            json.dumps({"error": str(exc), "kind": "io"}, ensure_ascii=False, sort_keys=True) + "\n"
        )
        return 2
    except Exception as exc:
        sys.stdout.write(
            json.dumps(
                {"error": str(exc), "kind": "internal"},
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            )
            + "\n"
        )
        return 1
