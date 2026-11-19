"""Command line interface for relaydiag.

Subcommands
-----------
``generate``  build the deterministic fixture under ``--out``
``run``       diagnose a fixture, with optional ``--resume`` from a checkpoint
``verify``    re-validate a result document against its input and checksums

Exit codes
----------
0 ok · 2 input/parameter error · 3 damage found · 4 resume state mismatch ·
5 output verification failure
"""

from __future__ import annotations

import argparse
import json
import os
import sys

from . import (EXIT_DAMAGE, EXIT_OK, EXIT_STATE_MISMATCH, EXIT_USAGE,
               EXIT_VERIFY_FAILED)
from .checkpoint import StateMismatch
from .engine import DiagnosisEngine, RunConfig
from .generator import GenerationConfig, generate_fixture
from .manifest import verify_document


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="relaydiag",
        description="Relay stream damage diagnosis and checkpointed recovery.")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="create deterministic test media")
    gen.add_argument("--out", required=True, help="output fixture directory")
    gen.add_argument("--frames", type=int, default=90)

    run = sub.add_parser("run", help="diagnose a fixture stream")
    run.add_argument("--input", required=True, help="fixture directory")
    run.add_argument("--checkpoint", required=True, help="checkpoint JSON path")
    run.add_argument("--json", dest="json_path", help="result manifest path")
    run.add_argument("--resume", action="store_true", help="resume a prior run")
    run.add_argument("--queue", type=int, default=8, help="bounded queue size")
    run.add_argument("--max-bytes", type=int, default=None,
                     help="stop after this many input bytes (truncation test)")
    run.add_argument("--interrupt-at", type=int, default=None,
                     help="inject an interrupt after N packets (recovery test)")

    verify = sub.add_parser("verify", help="verify a result manifest")
    verify.add_argument("--input", required=True, help="fixture directory")
    verify.add_argument("--result", required=True, help="result manifest path")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            return _cmd_generate(args)
        if args.command == "run":
            return _cmd_run(args)
        if args.command == "verify":
            return _cmd_verify(args)
    except InterruptedError as exc:
        # Recoverable stop: the checkpoint is durable; the operator resumes.
        print(f"interrupted: {exc}")
        print(f"checkpoint saved: {args.checkpoint}")
        print("rerun with --resume to continue")
        return EXIT_OK
    except StateMismatch as exc:
        print(f"resume refused: {exc}", file=sys.stderr)
        return EXIT_STATE_MISMATCH
    except (FileNotFoundError, ValueError, NotADirectoryError) as exc:
        print(f"input/parameter error: {exc}", file=sys.stderr)
        return EXIT_USAGE
    parser.error(f"unknown command {args.command}")
    return EXIT_USAGE


def _cmd_generate(args) -> int:
    if os.path.exists(args.out) and not os.path.isdir(args.out):
        raise NotADirectoryError(f"--out exists and is not a directory: {args.out}")
    cfg = GenerationConfig(frame_count=args.frames)
    info = generate_fixture(args.out, cfg)
    print(f"generated fixture: {info['stream_path']}")
    print(f"packets={info['packet_count']} "
          f"damage_plan={len(info['header']['damage_plan'])}")
    return EXIT_OK


def _cmd_run(args) -> int:
    if not os.path.isdir(args.input):
        raise FileNotFoundError(f"input directory not found: {args.input}")
    if args.queue < 1:
        raise ValueError("--queue must be >= 1")
    config = RunConfig(
        input_dir=args.input,
        checkpoint_path=args.checkpoint,
        result_path=args.json_path,
        resume=args.resume,
        queue_capacity=args.queue,
        max_bytes=args.max_bytes,
        inject_interrupt_at=args.interrupt_at,
    )
    document = DiagnosisEngine(config).run()
    metrics = document["metrics"]
    print("relaydiag run complete")
    print(f"damage_offset_error_bytes={metrics['damage_offset_error_bytes']}")
    print(f"damage_time_error_ms={metrics['damage_time_error_ms']}")
    print(f"resume_seconds={metrics.get('resume_seconds', 0.0)}")
    print(f"output_sha256={document['output_sha256']}")
    if args.json_path:
        print(f"manifest={args.json_path}")
    return EXIT_DAMAGE if document["errors"] else EXIT_OK


def _cmd_verify(args) -> int:
    result_path = args.result
    if not os.path.isfile(result_path):
        raise FileNotFoundError(f"result not found: {result_path}")
    with open(result_path, "r", encoding="utf-8") as fh:
        document = json.load(fh)
    ok, problems = verify_document(document)
    # Re-run the analysis on the named input and compare damage fingerprints.
    input_doc = _rerun_for_comparison(args.input, document)
    same_errors = document["segment_checksums"].get("__all_errors__") == \
        input_doc["segment_checksums"].get("__all_errors__")
    same_fingerprint = document["input"]["sha256"] == input_doc["input"]["sha256"]
    verified = ok and same_errors and same_fingerprint
    print(f"output_sha256={document['output_sha256']}")
    print(f"verified={'true' if verified else 'false'}")
    if not verified:
        for problem in problems:
            print(f"verify failure: {problem}", file=sys.stderr)
        if not same_errors:
            print("verify failure: error checksums differ from fresh run",
                  file=sys.stderr)
        if not same_fingerprint:
            print("verify failure: input fingerprint differs", file=sys.stderr)
        return EXIT_VERIFY_FAILED
    return EXIT_OK


def _rerun_for_comparison(input_dir: str, reference: dict) -> dict:
    """Fresh in-memory analysis whose checksums must match the stored one."""
    import tempfile
    state_fd, state_path = tempfile.mkstemp(prefix=".relaydiag-verify-",
                                            suffix=".json")
    os.close(state_fd)
    try:
        config = RunConfig(
            input_dir=input_dir,
            checkpoint_path=state_path,
            result_path=None,
            queue_capacity=reference["run_params"].get("queue_capacity", 8),
        )
        return DiagnosisEngine(config).run()
    finally:
        if os.path.exists(state_path):
            os.unlink(state_path)


if __name__ == "__main__":
    raise SystemExit(main())
