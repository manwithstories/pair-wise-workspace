"""Terminal entry point for scan, batch, and verify."""

from __future__ import annotations

import argparse
import math
from pathlib import Path
import sys

from .job import BatchConfig, run_batch, scan_groups, write_report
from .kernel import KernelLimitError, QUALITY_TAPS
from .verify import verify_report


EXIT_OK = 0
EXIT_PARTIAL = 1
EXIT_ARGUMENT = 2
EXIT_VERIFY = 3
EXIT_KERNEL = 4


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="audio_pipeline")
    commands = parser.add_subparsers(dest="command", required=True)

    scan = commands.add_parser("scan", help="inspect input groups")
    scan.add_argument("--in", dest="input_dir", required=True, type=Path)
    scan.add_argument("--target-rate", type=int, default=48000)
    scan.add_argument("--quality", choices=sorted(QUALITY_TAPS), default="high")

    batch = commands.add_parser("batch", help="resample, normalize, and report")
    batch.add_argument("--in", dest="input_dir", required=True, type=Path)
    batch.add_argument("--out", dest="output_dir", required=True, type=Path)
    batch.add_argument("--target-rate", type=int, default=48000)
    batch.add_argument("--target-lufs", type=float, default=-16.0)
    batch.add_argument("--block", type=int, default=4096)
    batch.add_argument("--quality", choices=sorted(QUALITY_TAPS), default="high")
    batch.add_argument("--report", type=Path)
    batch.add_argument("--cache-dir", type=Path)
    batch.add_argument("--resume", action="store_true")

    verify = commands.add_parser("verify", help="recompute output metrics")
    verify.add_argument("--report", required=True, type=Path)
    verify.add_argument("--dir", dest="output_dir", type=Path)
    verify.add_argument("--block", type=int, default=4096)
    return parser


def _validate_common(args: argparse.Namespace) -> None:
    if getattr(args, "target_rate", 48000) <= 0:
        raise ValueError("--target-rate must be positive")
    if getattr(args, "block", 4096) <= 0:
        raise ValueError("--block must be positive")
    if hasattr(args, "target_lufs") and not math.isfinite(args.target_lufs):
        raise ValueError("--target-lufs must be finite")
    if hasattr(args, "target_lufs") and args.target_lufs >= 0.0:
        raise ValueError("--target-lufs must be negative")
    if getattr(args, "input_dir", None) is not None and not args.input_dir.is_dir():
        raise ValueError(f"input directory does not exist: {args.input_dir}")


def _print_scan(groups: list[dict], failures: list[dict]) -> int:
    for group in groups:
        print(
            f"group in_rate={group['in_rate']} target={group['target_rate']} "
            f"quality={group['quality']} kernel={group['kernel_fingerprint']}"
        )
        for relative in group["files"]:
            print(f"  {relative}")
    for record in failures:
        print(f"fail {record['input']} [{record['error_code']}] {record['error']}")
    print(f"total={len(groups)} groups failed={len(failures)}")
    return EXIT_PARTIAL if failures else EXIT_OK


def _fmt(value: object, digits: int = 2) -> str:
    if isinstance(value, float):
        return f"{value:.{digits}f}"
    return str(value)


def _print_batch(report: dict) -> int:
    for record in report["records"]:
        if record["status"] == "ok":
            print(
                f"ok {record['input']} {record['in_rate']}->{record['out_rate']} "
                f"LUFS {_fmt(record['lufs_before'])}->{_fmt(record['lufs_after'])} "
                f"TP {_fmt(record['true_peak_dbtp'])} "
                f"clips {record['clipped_before']}->{record['clipped_after']}"
            )
        else:
            print(f"fail {record['input']} [{record['error_code']}] {record['error']}")
    summary = report["summary"]
    print(f"total={summary['total']} ok={summary['ok']} failed={summary['failed']}")
    return EXIT_OK if summary["failed"] == 0 else EXIT_PARTIAL


def _print_verify(result) -> int:
    for check in result.checks:
        if check.passed:
            print(f"ok {check.name}")
        else:
            print(
                f"mismatch {check.name}: expected={check.expected!r} "
                f"actual={check.actual!r}"
            )
    print(f"checked={result.checked} skipped={result.skipped} ok={result.ok}")
    return EXIT_OK if result.ok else EXIT_VERIFY


def main(argv: list[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    try:
        _validate_common(args)
        if args.command == "scan":
            config = BatchConfig(
                args.input_dir,
                Path("."),
                target_rate=args.target_rate,
                quality=args.quality,
            )
            groups, failures = scan_groups(config)
            return _print_scan(groups, failures)

        if args.command == "batch":
            report_path = args.report or (args.output_dir / "report.json")
            config = BatchConfig(
                args.input_dir,
                args.output_dir,
                target_rate=args.target_rate,
                target_lufs=args.target_lufs,
                block_size=args.block,
                quality=args.quality,
                cache_dir=args.cache_dir,
                report_path=report_path,
                resume=args.resume,
            )
            report = run_batch(config)
            exit_code = _print_batch(report)
            try:
                write_report(report, report_path)
            except OSError as exc:
                print(f"report write failed: {exc}", file=sys.stderr)
                return EXIT_PARTIAL if exit_code == EXIT_OK else exit_code
            return exit_code

        output_dir = args.output_dir or args.report.parent
        result = verify_report(args.report, output_dir, args.block)
        return _print_verify(result)
    except KernelLimitError as exc:
        print(f"kernel limit: {exc}", file=sys.stderr)
        return EXIT_KERNEL
    except (ValueError, OSError) as exc:
        print(f"invalid input: {exc}", file=sys.stderr)
        return EXIT_ARGUMENT


if __name__ == "__main__":
    raise SystemExit(main())
