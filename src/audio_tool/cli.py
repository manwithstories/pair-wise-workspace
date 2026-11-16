"""Command-line entry point: argument parsing, subcommands, exit codes.

Exit codes:

  0  every input processed successfully
  1  some inputs failed (bad format, unsupported layout, silent material, ...)
  2  parameters are invalid or the input set is unavailable
  3  outputs or the report could not be written

The module is importable as ``audio_tool.cli``; ``main()`` returns the exit
code so tests can call it directly without spawning a process.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from . import batch, report as report_mod
from .model import (
    AudioToolError,
    DEFAULT_BLOCK_FRAMES,
    MAX_SAMPLE_RATE,
    MAX_TRUE_PEAK_DBTP,
    MIN_SAMPLE_RATE,
    MIN_TRUE_PEAK_DBTP,
    ProcessConfig,
    ResampleConfig,
    UsageError,
    WriteError,
)

#: Default workspace root used when no explicit root is given: the current
#: directory.  Every input and output path must resolve inside it.
PROGRAM = "audio-tool"


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m audio_tool.cli",
        description="Offline WAV sample-rate and BS.1770 loudness normalizer.")
    parser.add_argument("--version", action="version", version=_version())
    sub = parser.add_subparsers(dest="command", required=True)

    batch_cmd = sub.add_parser(
        "batch", help="normalize a directory, list of files, or manifest")
    batch_cmd.add_argument("inputs", nargs="*", type=Path,
                           help="input files or directories")
    batch_cmd.add_argument("--manifest", type=Path,
                           help="file listing one input path per line")
    batch_cmd.add_argument("--input", dest="input_dirs", action="append", type=Path,
                           default=[], help="additional input directory (repeatable)")
    batch_cmd.add_argument("--output", type=Path, required=True,
                           help="output directory (created if missing)")
    batch_cmd.add_argument("--sample-rate", type=int, default=48000,
                           help=f"target sample rate, {MIN_SAMPLE_RATE}.."
                                f"{MAX_SAMPLE_RATE} Hz (default 48000)")
    batch_cmd.add_argument("--target-lufs", type=float, default=-16.0,
                           help="programme loudness target in LUFS (default -16)")
    batch_cmd.add_argument("--true-peak", type=float, default=-1.0,
                           help=f"true-peak ceiling in dBTP, "
                                f"{MIN_TRUE_PEAK_DBTP}..{MAX_TRUE_PEAK_DBTP} "
                                f"(default -1)")
    batch_cmd.add_argument("--block-frames", type=int, default=DEFAULT_BLOCK_FRAMES,
                           help="frames per processing block (default 16384)")
    batch_cmd.add_argument("--resample-passband", type=float, default=None,
                           help="resampler low-pass cutoff in Hz "
                                "(default: 92%% of the lower Nyquist)")
    batch_cmd.add_argument("--resample-taps", type=int, default=48,
                           help="one-sided resampler window length (default 48)")
    batch_cmd.add_argument("--resample-phases", type=int, default=256,
                           help="resampler polyphase resolution (default 256)")
    batch_cmd.add_argument("--subtype", default=None,
                           choices=["PCM_16", "PCM_24", "PCM_32", "FLOAT"],
                           help="output sample format (default PCM_24)")
    batch_cmd.add_argument("--jobs", type=int, default=1,
                           help="worker threads (default 1)")
    batch_cmd.add_argument("--report", type=Path,
                           help="write a JSON report to this path")
    batch_cmd.add_argument("--workspace", type=Path, default=Path.cwd(),
                           help="confinement root for inputs/outputs "
                                "(default: current directory)")
    batch_cmd.add_argument("--no-fingerprint", action="store_true",
                           help="skip SHA-256 input fingerprints (faster)")
    batch_cmd.add_argument("--quiet", action="store_true",
                           help="suppress per-file progress lines")
    batch_cmd.add_argument("--dry-run", action="store_true",
                           help="list planned inputs/outputs without writing")

    inspect_cmd = sub.add_parser(
        "inspect", help="print or verify a JSON report (offline recompute)")
    inspect_cmd.add_argument("--report", type=Path, required=True,
                             help="report JSON written by a previous batch run")
    inspect_cmd.add_argument("--verify", action="store_true",
                             help="re-derive results and flag inconsistencies")
    inspect_cmd.add_argument("--json", action="store_true",
                             help="emit the report (or verification) as JSON")

    verify_cmd = sub.add_parser(
        "verify", help="recompute a report's results from stored statistics")
    verify_cmd.add_argument("--report", type=Path, required=True)
    verify_cmd.add_argument("--output-dir", type=Path, default=None,
                            help="also check that output files still exist")
    verify_cmd.add_argument("--json", action="store_true")
    return parser


def _version() -> str:
    from . import __version__

    return f"{PROGRAM} {__version__}"


def _config_from_args(args: argparse.Namespace) -> ProcessConfig:
    resampler = ResampleConfig(passband_hz=args.resample_passband,
                               half_taps=args.resample_taps,
                               phases=args.resample_phases)
    config = ProcessConfig(
        target_rate=args.sample_rate,
        target_lufs=args.target_lufs,
        true_peak_db=args.true_peak,
        block_frames=args.block_frames,
        resampler=resampler,
        output_subtype=args.subtype,
    )
    config.validate()
    return config


def _confine(root: Path, path: Path, label: str) -> Path:
    from .io import ensure_inside

    return ensure_inside(root, path, label=label)


def _cmd_batch(args: argparse.Namespace) -> int:
    if args.quiet:
        def log(_message: str) -> None:
            return
    else:
        def log(message: str) -> None:
            print(message, file=sys.stderr)

    config = _config_from_args(args)
    workspace = Path(args.workspace).resolve()
    if not workspace.is_dir():
        raise UsageError(f"workspace is not a directory: {workspace}",
                         code="bad_workspace")

    entries: list[Path] = list(args.inputs)
    entries.extend(args.input_dirs)
    inputs = batch.collect_inputs(entries, args.manifest)
    for path in inputs:
        _confine(workspace, path, "input")
    output_root = _confine(workspace, args.output, "output directory")
    for path in inputs:
        if Path(path).resolve() == output_root:
            raise UsageError(f"input {path} is the output directory",
                             code="input_is_output")
    for path in inputs:
        try:
            path.resolve().relative_to(output_root)
        except ValueError:
            continue
        raise UsageError(
            f"input {path} lives inside the output directory {output_root}; "
            "refusing to overwrite sources", code="input_is_output")

    input_root = _common_root(inputs)
    plan = batch.plan_destinations(inputs, output_root, input_root)

    if args.dry_run:
        for source, destination, _relative in plan:
            print(f"{source} -> {destination}")
        print(f"{len(plan)} file(s) planned", file=sys.stderr)
        return 0

    if args.report is not None:
        _confine(workspace, args.report, "report path")

    def progress(outcome: batch.FileOutcome) -> None:
        if args.quiet:
            return
        if outcome.ok and outcome.result is not None:
            result = outcome.result
            lufs = "-inf" if result.after.lufs is None else f"{result.after.lufs:.2f}"
            print(f"[ok]   {outcome.relative}: {lufs} LUFS, "
                  f"gain {result.gain_db:+.2f} dB", file=sys.stderr)
        else:
            print(f"[fail] {outcome.relative}: {outcome.error_code}: "
                  f"{outcome.error_message}", file=sys.stderr)

    result = batch.run_batch(
        plan, config, jobs=args.jobs, input_root=input_root,
        output_root=output_root, fingerprint=not args.no_fingerprint,
        log=log if not args.quiet else None, progress=progress)

    document = report_mod.build_report(result, command="batch")
    print(report_mod.format_summary(document))

    if args.report is not None:
        report_mod.write_report(args.report, document)
        print(f"report: {args.report}", file=sys.stderr)
    return result.exit_code()


def _common_root(paths: list[Path]) -> Path | None:
    """Longest common directory of the inputs, if they share one."""

    if not paths:
        return None
    parents = [Path(path).resolve().parent for path in paths]
    try:
        common = Path(__import__("os").path.commonpath([str(p) for p in parents]))
    except ValueError:
        return None
    return common if common.is_dir() else None


def _cmd_inspect(args: argparse.Namespace) -> int:
    document = report_mod.load_report(args.report)
    if args.verify:
        findings = report_mod.verify_report(document)
        if args.json:
            print(json.dumps({"report": str(args.report),
                              "ok": not findings,
                              "findings": [f.to_dict() for f in findings]},
                             indent=2, sort_keys=True))
        else:
            _print_verification(args.report, findings)
        return 0 if not findings else 1
    if args.json:
        print(json.dumps(document, indent=2, sort_keys=True))
        return 0
    print(report_mod.format_summary(document))
    return 0


def _cmd_verify(args: argparse.Namespace) -> int:
    document = report_mod.load_report(args.report)
    findings = report_mod.verify_report(document, output_root=args.output_dir)
    if args.json:
        print(json.dumps({"report": str(args.report), "ok": not findings,
                          "findings": [f.to_dict() for f in findings]},
                         indent=2, sort_keys=True))
    else:
        _print_verification(args.report, findings)
    return 0 if not findings else 1


def _print_verification(path: Path, findings: list[report_mod.VerifyFinding]) -> None:
    if not findings:
        print(f"verify: OK - {path} is self-consistent")
        return
    print(f"verify: {len(findings)} finding(s) in {path}")
    for finding in findings:
        print(f"  {finding.source}: {finding.code}: {finding.detail}")


HANDLERS = {"batch": _cmd_batch, "inspect": _cmd_inspect, "verify": _cmd_verify}


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    try:
        args = parser.parse_args(argv)
        handler = HANDLERS[args.command]
        return handler(args)
    except AudioToolError as exc:
        print(f"error [{exc.code}]: {exc.message}", file=sys.stderr)
        if exc.details:
            print(f"  details: {json.dumps(exc.details, sort_keys=True)}",
                  file=sys.stderr)
        return exc.exit_code
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
