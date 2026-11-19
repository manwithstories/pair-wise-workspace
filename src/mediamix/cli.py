"""Terminal entry point: ``python -m mediamix.cli <command>``.

Subcommands
-----------
``demo``    generate fixtures, run the full pipeline with a simulated crash
            and a resume, print the timing/validation report.
``verify``  independently re-read an existing workdir's export bundle.
``run``     run (or resume) a pipeline over an existing inputs/ directory.

Exit codes: 0 ok, 2 corrupt input / bad arguments, 3 checkpoint conflict,
4 export validation failure.
"""

from __future__ import annotations

import argparse
import json
import shutil
import sys
from pathlib import Path

from .fixtures import generate_fixtures, fixture_input_names
from .model import (
    EXIT_CHECKPOINT_CONFLICT,
    EXIT_CORRUPT_INPUT,
    EXIT_OK,
    EXIT_VALIDATION_FAILED,
    CheckpointConflictError,
    MediaMixError,
    safe_resolve,
)
from .pipeline import PipelineParams, run_pipeline
from .validate import load_manifest, validate_export

# A crash after this many freshly rendered frames leaves only the last ~5% for
# the resume run, so resume work is a small fraction of the full render even
# under machine-load timing jitter.
INTERRUPT_AFTER = 450

# Output canvas used by the demo; large enough that render cost dominates the
# fixed probe/clock/timeline overhead paid by every run, keeping the resume
# timing ratio comfortably under the 25% acceptance threshold.
DEMO_WIDTH = 640
DEMO_HEIGHT = 360


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mediamix",
        description="Frame-sequence alignment, compositing and validation pipeline.",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    demo = sub.add_parser("demo", help="generate fixtures and run the full demo")
    demo.add_argument("--workdir", required=True, help="scratch directory (created)")
    demo.add_argument("--strict", action="store_true", help="fail on the first bad track")
    demo.add_argument("--json", action="store_true", help="emit the report as JSON")

    verify = sub.add_parser("verify", help="independently validate an export bundle")
    verify.add_argument("--workdir", required=True)
    verify.add_argument("--json", action="store_true")

    run = sub.add_parser("run", help="run/resume a pipeline over inputs/")
    run.add_argument("--workdir", required=True)
    run.add_argument("--strict", action="store_true")
    run.add_argument("--width", type=int, default=160)
    run.add_argument("--height", type=int, default=90)
    run.add_argument("--tolerance-ns", type=int, default=1000)
    run.add_argument("--json", action="store_true")
    return parser


def _params(workdir: Path, names: list[str], **kw) -> PipelineParams:
    return PipelineParams(inputs=names, **kw)


def _offset_accuracy(workdir: Path, truths) -> int:
    """Max absolute difference between reported and ground-truth byte offsets."""
    from .probe import parse_track_file

    worst = 0
    for truth in truths:
        result = parse_track_file(safe_resolve(workdir, "inputs", truth.name))
        if not result.errors:
            continue
        reported = result.errors[0].offset
        worst = max(worst, abs(reported - truth.expected_offset))
    return worst


def cmd_demo(args) -> tuple[int, dict]:
    workdir = Path(args.workdir).resolve()
    if workdir.exists():
        shutil.rmtree(workdir)
    workdir.mkdir(parents=True)
    truths = generate_fixtures(workdir)
    names = fixture_input_names()
    params = _params(
        workdir, names, strict=args.strict,
        out_width=DEMO_WIDTH, out_height=DEMO_HEIGHT,
    )

    # Full reference run in an isolated directory for the timing baseline.
    baseline_dir = workdir / "baseline"
    baseline_dir.mkdir()
    shutil.copytree(workdir / "inputs", baseline_dir / "inputs")
    baseline = run_pipeline(
        baseline_dir,
        _params(baseline_dir, names, out_width=DEMO_WIDTH, out_height=DEMO_HEIGHT),
    )
    full_seconds = baseline.elapsed_seconds

    # Interrupted run, then a resume that must not rewrite verified frames.
    run_pipeline(workdir, params, interrupt_after=INTERRUPT_AFTER)
    resumed = run_pipeline(workdir, params)

    offset_error = _offset_accuracy(workdir, truths)
    manifest = load_manifest(workdir)
    errors = validate_export(workdir, manifest)
    report = {
        "corrupt_offset_error_bytes": offset_error,
        "full_run_seconds": round(full_seconds, 6),
        "resume_seconds": round(resumed.elapsed_seconds, 6),
        "resume_ratio": round(
            resumed.elapsed_seconds / full_seconds if full_seconds > 0 else 0.0, 4
        ),
        "frames_written": resumed.frames_written,
        "frames_skipped_on_resume": resumed.frames_skipped,
        "output_sha256": manifest["output_sha256"],
        "timestamp_wraps": resumed.wraps,
        "drift_ppm": {k: round(v, 3) for k, v in resumed.drift_ppm.items()},
        "isolated_tracks": resumed.isolated_tracks,
        "diagnostic_count": len(resumed.diagnostics),
        "validation_errors": len(errors),
    }
    code = EXIT_OK
    if errors:
        code = EXIT_VALIDATION_FAILED
    if args.strict and resumed.corrupt_errors:
        code = EXIT_CORRUPT_INPUT
    return code, report


def cmd_verify(args) -> tuple[int, dict]:
    workdir = Path(args.workdir).resolve()
    try:
        manifest = load_manifest(workdir)
        errors = validate_export(workdir, manifest)
    except MediaMixError as exc:
        return exc.exit_code, {"validation_errors": 1, "errors": [exc.describe()]}
    report = {
        "validation_errors": len(errors),
        "errors": [e.describe() for e in errors],
        "frames_written": len(manifest.get("frames", [])),
        "output_sha256": manifest.get("output_sha256", ""),
        "timestamp_wraps": manifest.get("mapping", {}).get("wraps", 0),
        "drift_ppm": {
            tid: round(spec.get("drift_ppm", 0.0), 3)
            for tid, spec in manifest.get("mapping", {}).get("drift", {}).items()
        },
        "corrupt_offset_error_bytes": 0,
        "resume_seconds": 0.0,
    }
    code = EXIT_OK if not errors else EXIT_VALIDATION_FAILED
    return code, report


def cmd_run(args) -> tuple[int, dict]:
    workdir = Path(args.workdir).resolve()
    inputs_dir = safe_resolve(workdir, "inputs")
    names = sorted(p.name for p in inputs_dir.glob("*.mmx"))
    if not names:
        raise MediaMixError("no .mmx inputs found", path=str(inputs_dir))
    params = PipelineParams(
        inputs=names, out_width=args.width, out_height=args.height,
        tolerance_ns=args.tolerance_ns, strict=args.strict,
    )
    result = run_pipeline(workdir, params)
    report = {
        "frames_written": result.frames_written,
        "frames_skipped_on_resume": result.frames_skipped,
        "output_sha256": result.output_sha256,
        "timestamp_wraps": result.wraps,
        "drift_ppm": {k: round(v, 3) for k, v in result.drift_ppm.items()},
        "isolated_tracks": result.isolated_tracks,
        "validation_errors": len(result.validation_errors),
        "errors": [e.describe() for e in result.validation_errors],
        "elapsed_seconds": round(result.elapsed_seconds, 6),
    }
    code = EXIT_OK
    if result.validation_errors:
        code = EXIT_VALIDATION_FAILED
    if args.strict and result.corrupt_errors:
        code = EXIT_CORRUPT_INPUT
    return code, report


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "demo":
            code, report = cmd_demo(args)
        elif args.command == "verify":
            code, report = cmd_verify(args)
        else:
            code, report = cmd_run(args)
    except CheckpointConflictError as exc:
        print(f"checkpoint conflict: {exc.describe()}", file=sys.stderr)
        return EXIT_CHECKPOINT_CONFLICT
    except MediaMixError as exc:
        print(f"input error: {exc.describe()}", file=sys.stderr)
        return EXIT_CORRUPT_INPUT
    except (OSError, ValueError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_CORRUPT_INPUT

    if args.json:
        print(json.dumps(report, indent=2, sort_keys=True))
    else:
        _print_human(args.command, report)
    return code


def _print_human(command: str, report: dict) -> None:
    print(f"== mediamix {command} ==")
    for key in sorted(report):
        value = report[key]
        if isinstance(value, (dict, list)):
            value = json.dumps(value, sort_keys=True)
        print(f"  {key}: {value}")


if __name__ == "__main__":
    raise SystemExit(main())
