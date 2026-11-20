"""Command-line entry point: ``ingest``, ``process``, ``resume``, ``verify``.

Every subcommand prints a single JSON object to stdout so the terminal output
can be archived and diffed between runs.  Diagnostics that are not fatal are
listed under ``diagnostics`` with the shared error codes; exit codes are:

``0`` success and acceptance metrics met
``1`` input or processing error
``2`` invalid arguments
``3`` recoverable interruption, or acceptance metrics not met
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Sequence

from . import __version__, checkpoint, pipeline
from .errors import (
    Code,
    Diagnostic,
    DiagnosticCollector,
    InputError,
    Interrupted,
    MediaError,
    ParamError,
)

EXIT_OK = 0
EXIT_INPUT = 1
EXIT_PARAMS = 2
EXIT_RECOVERABLE = 3


class _Parser(argparse.ArgumentParser):
    """ArgumentParser that returns exit code 2 and JSON on usage errors."""

    def error(self, message: str) -> None:  # type: ignore[override]
        _emit(
            {
                "command": self.prog,
                "status": "error",
                "exit_code": EXIT_PARAMS,
                "error": {
                    "code": Code.UNSUPPORTED_LAYOUT.value,
                    "severity": "fatal",
                    "message": message,
                },
            }
        )
        raise SystemExit(EXIT_PARAMS)


def _emit(payload: dict[str, Any]) -> None:
    json.dump(payload, sys.stdout, ensure_ascii=False, indent=2, sort_keys=True)
    sys.stdout.write("\n")
    sys.stdout.flush()


def _fail(command: str, exc: MediaError, *, exit_code: int) -> int:
    payload = {
        "command": command,
        "status": "error",
        "exit_code": exit_code,
        "error": exc.to_json(),
    }
    _emit(payload)
    return exit_code


def build_parser() -> argparse.ArgumentParser:
    parser = _Parser(prog="region-media", description=__doc__)
    parser.add_argument("--version", action="version", version=__version__)
    sub = parser.add_subparsers(dest="command", required=True,
                                parser_class=_Parser)

    def add_common(p: argparse.ArgumentParser, *, output: bool) -> None:
        if output:
            p.add_argument("--out", required=True, type=Path,
                           help="output directory (created if missing)")

    ingest = sub.add_parser("ingest", help="inspect a capture without decoding")
    ingest.add_argument("--input", required=True, type=Path)
    add_common(ingest, output=False)
    ingest.add_argument("--json", action="store_true", help="(always on)")

    for name in ("process", "resume"):
        p = sub.add_parser(name, help=f"{name} a capture into delivery chunks")
        p.add_argument("--input", required=True, type=Path)
        add_common(p, output=True)
        p.add_argument("--rate", type=int, default=48000)
        p.add_argument("--layout", default="stereo")
        p.add_argument("--lufs", type=float, default=-16.0)
        p.add_argument("--chunk-ms", type=int, default=1000)
        p.add_argument("--target-kbps", type=float, default=256.0)
        p.add_argument("--peak-dbfs", type=float, default=-1.0)
        p.add_argument(
            "--simulate-interrupt-after",
            type=int,
            default=None,
            metavar="N",
            help="stop after N newly committed chunks (resume testing)",
        )
        if name == "resume":
            p.set_defaults(resume=True)
        else:
            p.set_defaults(resume=False)

    verify = sub.add_parser("verify", help="recompute quality and alignment")
    verify.add_argument("--out", required=True, type=Path)
    verify.add_argument("--manifest", required=True, type=Path)

    return parser


def _run_ingest(args: argparse.Namespace) -> int:
    from . import assemble, metadata

    analysis = assemble.scan_capture(args.input)
    report, video_diags = metadata.inspect_video_metadata(
        analysis.videos, where=str(args.input)
    )
    collector = DiagnosticCollector()
    collector.extend(analysis.diagnostics)
    collector.extend(video_diags)
    _emit(
        {
            "command": "ingest",
            "status": "ok",
            "exit_code": EXIT_OK,
            "input": str(args.input),
            "source": analysis.summary(),
            "video": report.to_json(),
            "errors": collector.to_json()["codes"],
            "diagnostics": collector.to_json()["samples"],
        }
    )
    return EXIT_OK


def _run_process(args: argparse.Namespace) -> int:
    options = pipeline.ProcessOptions(
        input_dir=args.input,
        out_dir=args.out,
        rate=args.rate,
        layout=args.layout,
        lufs=args.lufs,
        chunk_ms=args.chunk_ms,
        target_kbps=args.target_kbps,
        peak_dbfs=args.peak_dbfs,
        simulate_interrupt_after=args.simulate_interrupt_after,
        resume=getattr(args, "resume", False),
    )
    command = "resume" if options.resume else "process"
    try:
        result = pipeline.process(options)
    except Interrupted as exc:
        payload = {
            "command": command,
            "status": "interrupted",
            "exit_code": EXIT_RECOVERABLE,
            "input": str(args.input),
            "out": str(args.out),
            "interrupted": True,
            "resume_hint": f"rerun with: resume --input {args.input} --out {args.out}",
            "error": exc.to_json(),
            "detail": exc.detail,
        }
        _emit(payload)
        return EXIT_RECOVERABLE
    except ParamError as exc:
        return _fail(command, exc, exit_code=EXIT_PARAMS)
    except InputError as exc:
        return _fail(command, exc, exit_code=EXIT_INPUT)
    except MediaError as exc:
        return _fail(command, exc, exit_code=EXIT_INPUT)
    except Exception as exc:  # pragma: no cover - unexpected
        diag = Diagnostic(Code.ENCODE_FAILURE, f"unexpected failure: {exc}")
        return _fail(command, MediaError(Code.ENCODE_FAILURE, diag.message),
                     exit_code=EXIT_INPUT)
    payload = {
        "command": command,
        "status": "ok" if result.exit_code == EXIT_OK else "error",
        **result.to_json(),
    }
    _emit(payload)
    return result.exit_code


def _run_verify(args: argparse.Namespace) -> int:
    try:
        result = pipeline.verify(args.out, args.manifest)
    except ParamError as exc:
        return _fail("verify", exc, exit_code=EXIT_PARAMS)
    except InputError as exc:
        return _fail("verify", exc, exit_code=EXIT_INPUT)
    except MediaError as exc:
        return _fail("verify", exc, exit_code=EXIT_INPUT)
    payload = {
        "command": "verify",
        "status": "ok" if result.exit_code == EXIT_OK else "metrics_failed",
        **result.to_json(),
    }
    _emit(payload)
    return result.exit_code


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.command == "ingest":
        return _run_ingest(args)
    if args.command in ("process", "resume"):
        return _run_process(args)
    if args.command == "verify":
        return _run_verify(args)
    parser.error(f"unknown command {args.command!r}")
    return EXIT_PARAMS


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
