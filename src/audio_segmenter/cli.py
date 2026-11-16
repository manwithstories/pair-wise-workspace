"""Command-line interface: generate, detect, batch and verify.

Exit codes are a contract:

    0  everything passed
    2  bad arguments or unusable input format
    3  detection or database failure
    4  verification failed
"""

from __future__ import annotations

import argparse
import os
import sys
from typing import Optional, Sequence

from . import __version__
from . import reporting
from .exporter import ExportError, export_all
from .fixtures import generate_fixtures, write_float_wav, write_truncated_wav
from .model import DetectionConfig
from .pipeline import discover_wavs, execute_batch, run_batch
from .store import Store, StoreError
from .verify import verify_analyses

EXIT_OK = 0
EXIT_ARG = 2
EXIT_RUNTIME = 3
EXIT_VERIFY = 4

FORMAT_ERROR_CODES = frozenset(
    {
        "wav_not_found",
        "wav_empty",
        "wav_corrupt",
        "wav_unsupported_float",
        "wav_unsupported_codec",
        "wav_unsupported_channels",
        "wav_unsupported_bit_depth",
    }
)


class CliError(Exception):
    def __init__(self, message: str, code: int = EXIT_ARG) -> None:
        super().__init__(message)
        self.message = message
        self.code = code


def _config_from_args(args: argparse.Namespace) -> DetectionConfig:
    config = DetectionConfig(
        frame_ms=args.frame_ms,
        hop_ms=args.hop_ms,
        min_segment_ms=args.min_segment_ms,
        min_silence_ms=args.min_silence_ms,
        padding_ms=args.padding_ms,
        threshold_db=args.threshold_db,
        relative_margin_db=args.relative_margin_db,
        hysteresis_db=args.hysteresis_db,
    )
    try:
        config.validate(args.sample_rate)
    except ValueError as exc:
        raise CliError(f"invalid detection configuration: {exc}") from exc
    return config


def _resolve_inputs(raw: str) -> list[str]:
    try:
        paths = discover_wavs(raw)
    except FileNotFoundError as exc:
        raise CliError(f"input path does not exist: {raw}") from exc
    if not paths:
        raise CliError(f"no .wav files found under {raw}")
    return paths


def cmd_generate(args: argparse.Namespace) -> int:
    # Negative fixtures live in a subdirectory so a plain scan of the fixture
    # directory only sees valid audio (the demo's FILES=3 contract).
    invalid_dir = os.path.join(args.out_dir, "invalid")
    try:
        paths = generate_fixtures(args.out_dir, seed=args.seed)
        os.makedirs(invalid_dir, exist_ok=True)
        write_float_wav(os.path.join(invalid_dir, "bad_float.wav"))
        write_truncated_wav(os.path.join(invalid_dir, "bad_truncated.wav"))
    except OSError as exc:
        print(f"ERROR: cannot write fixtures: {exc}", file=sys.stderr)
        return EXIT_ARG
    for line in reporting.format_generate(paths, args.out_dir):
        print(line)
    print(f"INVALID_DIR={invalid_dir}")
    return EXIT_OK


def cmd_detect(args: argparse.Namespace) -> int:
    config = _config_from_args(args)
    paths = _resolve_inputs(args.input)
    analyses = run_batch(paths, config)
    exported: Optional[int] = None
    if args.jsonl or args.csv:
        try:
            report = export_all(analyses, jsonl_path=args.jsonl, csv_path=args.csv)
        except ExportError as exc:
            print(f"ERROR: {exc.code}: {exc.message}", file=sys.stderr)
            return EXIT_RUNTIME
        exported = report.records_written
    for line in reporting.format_detect(analyses, exported):
        print(line)
    if not any(a.status == "ok" for a in analyses):
        return EXIT_ARG
    return EXIT_OK


def cmd_batch(args: argparse.Namespace) -> int:
    config = _config_from_args(args)
    paths = _resolve_inputs(args.input)
    store: Optional[Store] = None
    if args.db:
        try:
            store = Store(args.db)
        except StoreError as exc:
            print(f"ERROR: {exc.code}: {exc.message}", file=sys.stderr)
            return EXIT_RUNTIME
    try:
        report = execute_batch(args.input, config, store=store, workers=args.workers)
    finally:
        if store is not None:
            store.close()

    if args.jsonl or args.csv or args.sqlite_out:
        try:
            export_all(
                report.ok,
                jsonl_path=args.jsonl,
                csv_path=args.csv,
                sqlite_path=args.sqlite_out,
            )
        except ExportError as exc:
            print(f"ERROR: {exc.code}: {exc.message}", file=sys.stderr)
            return EXIT_RUNTIME

    outputs = {
        "JSONL": args.jsonl,
        "CSV": args.csv,
        "SQLITE": args.sqlite_out,
    }
    for line in reporting.format_batch(report, outputs):
        print(line)
    if report.db_error:
        print(f"ERROR: database: {report.db_error}", file=sys.stderr)
        return EXIT_RUNTIME
    if not report.ok:
        return EXIT_ARG
    return EXIT_OK


def cmd_verify(args: argparse.Namespace) -> int:
    config = _config_from_args(args)
    if args.db:
        try:
            count, problems = reporting.verify_db(args.db)
        except Exception as exc:  # sqlite3 errors are runtime failures
            print(f"ERROR: cannot verify database: {exc}", file=sys.stderr)
            return EXIT_RUNTIME
        print(f"VERIFIED_FILES={count}")
        for line in problems:
            print(line)
        ok = not problems and count > 0
        print(f"VERIFY={'PASS' if ok else 'FAIL'}")
        return EXIT_OK if ok else EXIT_VERIFY

    if not args.input:
        print("ERROR: verify needs --input or --db", file=sys.stderr)
        return EXIT_ARG
    paths = _resolve_inputs(args.input)
    analyses = run_batch(paths, config)
    report = verify_analyses(analyses)
    print(f"VERIFIED_FILES={report.checked_files}")
    for analysis in analyses:
        if analysis.status != "ok":
            print(
                f"SKIPPED file={os.path.basename(analysis.path)}"
                f" code={analysis.error_code}"
            )
    for issue in report.issues:
        print(
            f"VERIFY_FAIL file={os.path.basename(issue.path)}"
            f" reason={issue.reason}"
        )
    ok = report.passed and report.checked_files > 0
    print(f"VERIFY={'PASS' if ok else 'FAIL'}")
    return EXIT_OK if ok else EXIT_VERIFY


def _add_common_detection_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--sample-rate", type=int, default=48000)
    parser.add_argument("--frame-ms", type=float, default=20.0)
    parser.add_argument("--hop-ms", type=float, default=10.0)
    parser.add_argument("--min-segment-ms", type=float, default=80.0)
    parser.add_argument("--min-silence-ms", type=float, default=120.0)
    parser.add_argument("--padding-ms", type=float, default=40.0)
    parser.add_argument("--threshold-db", type=float, default=-42.0)
    parser.add_argument("--relative-margin-db", type=float, default=6.0)
    parser.add_argument("--hysteresis-db", type=float, default=6.0)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="audio_segmenter",
        description="Detect audio segments and silence in local PCM WAV files.",
    )
    parser.add_argument("--version", action="version", version=__version__)
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="write the demo fixture set")
    gen.add_argument("--out-dir", required=True)
    gen.add_argument("--seed", type=int, default=20240617)
    gen.set_defaults(func=cmd_generate)

    det = sub.add_parser("detect", help="detect segments in a file or directory")
    det.add_argument("--input", required=True)
    det.add_argument("--jsonl")
    det.add_argument("--csv")
    _add_common_detection_args(det)
    det.set_defaults(func=cmd_detect)

    bat = sub.add_parser("batch", help="detect many files, persist and export")
    bat.add_argument("--input", required=True)
    bat.add_argument("--db")
    bat.add_argument("--jsonl")
    bat.add_argument("--csv")
    bat.add_argument("--sqlite-out", dest="sqlite_out")
    bat.add_argument("--workers", type=int, default=None)
    _add_common_detection_args(bat)
    bat.set_defaults(func=cmd_batch)

    ver = sub.add_parser("verify", help="check detection invariants")
    ver.add_argument("--input")
    ver.add_argument("--db")
    _add_common_detection_args(ver)
    ver.set_defaults(func=cmd_verify)
    return parser


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except CliError as exc:
        print(f"ERROR: {exc.message}", file=sys.stderr)
        return exc.code
    except KeyboardInterrupt:  # pragma: no cover
        print("interrupted", file=sys.stderr)
        return EXIT_RUNTIME


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
