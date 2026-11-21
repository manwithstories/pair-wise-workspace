"""Command-line entry point.

Commands:

``demo``    generate fixtures, run the pipeline (optionally interrupting it),
            resume, and print a JSON summary with the acceptance metrics,
``verify``  run the pipeline twice in fresh directories and compare the
            timelines, manifests and per-segment checksums,
``inspect`` print the parsed timeline and its validation issues.

Exit codes: 0 success, 2 invalid material/timeline, 3 recovery/output check
failure.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import sys
import tempfile
import time
from fractions import Fraction
from typing import Any, Sequence

from .config import ConfigError, ProjectConfig
from .errors import ExitCode, Stage
from .model import format_rational
from .pipeline import Pipeline, PipelineResult
from .timeline import TimelineParseError, load_timeline, validate_timeline


def _parse_stage(text: str) -> Stage:
    try:
        return Stage[text.strip().upper()]
    except KeyError as exc:
        options = ", ".join(s.label for s in Stage)
        raise argparse.ArgumentTypeError(f"unknown stage {text!r}; choose from {options}") from exc


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="avtimeline",
        description="Deterministic multi-track media timeline segmentation",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    demo = sub.add_parser("demo", help="run the end-to-end demonstration")
    demo.add_argument("--workdir", required=True, help="output directory")
    demo.add_argument("--break-at", type=_parse_stage, default=None, help="simulate an interruption after this stage")
    demo.add_argument("--resume", action="store_true", help="resume from a checkpoint when present")
    demo.add_argument("--corrupt-sample", action="store_true", help="include the truncated camera fixture")
    demo.add_argument("--edge-sample", action="store_true", help="include the 0.2 s tail and alternate camera fixtures")
    demo.add_argument("--policy", choices=("stop", "skip"), default="skip", help="behaviour on damaged input")
    demo.add_argument("--clean", action="store_true", help="remove the workdir before running")
    demo.add_argument("--force-samples", action="store_true", help="regenerate fixtures from scratch")
    demo.add_argument("--json-out", default=None, help="also write the summary JSON here")

    verify = sub.add_parser("verify", help="check determinism across two fresh runs")
    verify.add_argument("--workdir", required=True)
    verify.add_argument("--corrupt-sample", action="store_true")
    verify.add_argument("--edge-sample", action="store_true")
    verify.add_argument("--json-out", default=None)

    inspect = sub.add_parser("inspect", help="parse and validate a timeline document")
    inspect.add_argument("path")
    return parser


def _config_from_args(args: argparse.Namespace) -> ProjectConfig:
    payload: dict[str, Any] = {"policy": getattr(args, "policy", "stop")}
    config = ProjectConfig.from_mapping(payload)
    return config


def _run_pipeline(
    workdir: str,
    config: ProjectConfig,
    *,
    resume: bool,
    break_at: Stage | None,
    corrupt_sample: bool,
    edge_sample: bool,
    force_samples: bool,
) -> PipelineResult:
    pipeline = Pipeline(
        workdir,
        config,
        resume=resume,
        break_at=break_at,
        corrupt_sample=corrupt_sample,
        edge_sample=edge_sample,
        force_samples=force_samples,
    )
    return pipeline.run()


def _summary(
    result: PipelineResult,
    *,
    full_seconds: float,
    resume_seconds: float | None,
    repeat: dict[str, Any] | None,
) -> dict[str, Any]:
    errors = result.errors
    first = errors[0] if errors else None
    plan = result.plan
    summary: dict[str, Any] = {
        "first_error_track": first.location.track if first else None,
        "first_error_pts": format_rational(first.location.pts)
        if first and first.location.pts is not None
        else None,
        "first_error_pts_seconds": _seconds_of(first.location.pts) if first else None,
        "first_error_frame": first.location.frame if first else None,
        "first_error_sample": first.location.sample_start if first else None,
        "first_error_code": first.code if first else None,
        "first_error_stage": first.stage.label if first else None,
        "resume_seconds": round(resume_seconds, 4) if resume_seconds is not None else None,
        "full_seconds": round(full_seconds, 4),
        "resume_ratio": round(resume_seconds / full_seconds, 4)
        if resume_seconds is not None and full_seconds > 0
        else None,
        "aligned_segments": result.aligned_segments,
        "sha256_match": bool(repeat and repeat.get("sha256_match")),
        "manifest_sha256": result.manifest.checksums.get("manifest") if result.manifest else None,
        "segment_sha256": _segment_sha(result),
        "boundary_error": str(plan.max_boundary_error()) if plan else None,
        "short_segments": [s.to_dict() for s in plan.short_segments] if plan else [],
        "renditions": [r.name for r in ProjectConfig().renditions],
        "color_decision": result.color,
        "skipped_tracks": result.skipped_tracks,
        "stages": [s.to_dict() for s in result.stages],
        "timings": {k: round(v, 4) for k, v in sorted(result.timings.items())},
        "repeated_run": repeat,
    }
    if result.manifest is not None:
        summary["segment_count"] = len(result.manifest.segments)
    return summary


def _seconds_of(value: Fraction) -> float:
    return round(float(value), 6)


def _segment_sha(result: PipelineResult) -> dict[str, dict[str, str]]:
    if result.manifest is None:
        return {}
    return {
        str(segment["index"]): {
            name: entry["sha256"] for name, entry in segment["renditions"].items()
        }
        for segment in result.manifest.segments
    }


def _compare_runs(first: PipelineResult, second: PipelineResult) -> dict[str, Any]:
    def render(result: PipelineResult) -> dict[str, Any]:
        return {
            "manifest": result.manifest.serialize() if result.manifest else None,
            "segment_sha": _segment_sha(result),
            "boundaries": [s["start"] for s in result.manifest.segments]
            if result.manifest
            else [],
        }

    a = render(first)
    b = render(second)
    match = a["segment_sha"] == b["segment_sha"] and a["manifest"] == b["manifest"]
    return {
        "sha256_match": match,
        "manifest_match": a["manifest"] == b["manifest"],
        "segments_match": a["segment_sha"] == b["segment_sha"],
        "boundaries_match": a["boundaries"] == b["boundaries"],
        "segments_compared": len(a["segment_sha"]),
    }


def _exit_code(result: PipelineResult, summary: dict[str, Any], threshold_ok: bool) -> int:
    if result.has_fatal:
        return int(ExitCode.INVALID_INPUT)
    if summary.get("mixed_failure"):
        return int(ExitCode.INVALID_INPUT)
    if not threshold_ok:
        return int(ExitCode.VERIFY_FAILED)
    if result.has_error and not result.skipped_tracks:
        return int(ExitCode.VERIFY_FAILED)
    if summary.get("aligned_segments", 0) < 6:
        return int(ExitCode.VERIFY_FAILED)
    if not summary.get("sha256_match", False):
        return int(ExitCode.VERIFY_FAILED)
    return int(ExitCode.OK)


def run_demo(args: argparse.Namespace) -> int:
    workdir = os.path.abspath(args.workdir)
    if args.clean and os.path.isdir(workdir):
        shutil.rmtree(workdir)
    os.makedirs(workdir, exist_ok=True)
    config = _config_from_args(args)
    started = time.perf_counter()
    result = _run_pipeline(
        workdir,
        config,
        resume=False,
        break_at=args.break_at,
        corrupt_sample=args.corrupt_sample,
        edge_sample=args.edge_sample,
        force_samples=args.force_samples,
    )
    full_seconds = time.perf_counter() - started
    resume_seconds: float | None = None
    resumed_result = result
    if args.break_at is not None and args.resume:
        resume_started = time.perf_counter()
        resumed_result = _run_pipeline(
            workdir,
            config,
            resume=True,
            break_at=None,
            corrupt_sample=args.corrupt_sample,
            edge_sample=args.edge_sample,
            force_samples=args.force_samples,
        )
        resume_seconds = time.perf_counter() - resume_started
    # Determinism: a second isolated run must reproduce every checksum.
    repeat: dict[str, Any] | None = None
    repeat_dir = tempfile.mkdtemp(prefix="avtimeline-repeat-")
    try:
        repeat_result = _run_pipeline(
            repeat_dir,
            config,
            resume=False,
            break_at=None,
            corrupt_sample=args.corrupt_sample,
            edge_sample=args.edge_sample,
            force_samples=True,
        )
        repeat = _compare_runs(resumed_result, repeat_result)
    finally:
        shutil.rmtree(repeat_dir, ignore_errors=True)
    summary = _summary(
        resumed_result,
        full_seconds=full_seconds,
        resume_seconds=resume_seconds,
        repeat=repeat,
    )
    summary["workdir"] = workdir
    summary["interrupted_at"] = result.interrupted_at.label if result.interrupted_at else None
    summary["resumed_from"] = resumed_result.resumed_from.label if resumed_result.resumed_from else None
    summary["reused_stages"] = [s.label for s in resumed_result.reused_stages]
    threshold_ok = True
    if resume_seconds is not None and full_seconds > 0:
        threshold_ok = resume_seconds < full_seconds * 0.5
    summary["resume_within_budget"] = threshold_ok
    code = _exit_code(resumed_result, summary, threshold_ok)
    summary["exit_code"] = code
    _emit(summary, args.json_out)
    return code


def run_verify(args: argparse.Namespace) -> int:
    workdir = os.path.abspath(args.workdir)
    os.makedirs(workdir, exist_ok=True)
    config = _config_from_args(args)
    first_dir = os.path.join(workdir, "run1")
    second_dir = os.path.join(workdir, "run2")
    for path in (first_dir, second_dir):
        shutil.rmtree(path, ignore_errors=True)
    first = _run_pipeline(
        first_dir,
        config,
        resume=False,
        break_at=None,
        corrupt_sample=args.corrupt_sample,
        edge_sample=args.edge_sample,
        force_samples=True,
    )
    second = _run_pipeline(
        second_dir,
        config,
        resume=False,
        break_at=None,
        corrupt_sample=args.corrupt_sample,
        edge_sample=args.edge_sample,
        force_samples=True,
    )
    repeat = _compare_runs(first, second)
    summary = {
        "sha256_match": repeat["sha256_match"],
        "segments_compared": repeat["segments_compared"],
        "aligned_segments": first.aligned_segments,
        "first_errors": [e.to_dict() for e in first.errors],
        "second_errors": [e.to_dict() for e in second.errors],
        "first_manifest": first.manifest.checksums.get("manifest") if first.manifest else None,
        "second_manifest": second.manifest.checksums.get("manifest") if second.manifest else None,
        "exit_code": int(
            ExitCode.OK
            if repeat["sha256_match"] and first.aligned_segments >= 6
            else ExitCode.VERIFY_FAILED
        ),
    }
    _emit(summary, args.json_out)
    return int(summary["exit_code"])


def run_inspect(args: argparse.Namespace) -> int:
    try:
        timeline = load_timeline(args.path)
    except TimelineParseError as exc:
        _emit({"error": str(exc)}, None)
        return int(ExitCode.INVALID_INPUT)
    issues = validate_timeline(timeline)
    payload = {
        "tracks": sorted(timeline.tracks),
        "window": {
            "start": str(timeline.declared_window().start),
            "end": str(timeline.declared_window().end),
        },
        "issues": [issue.to_media_error(Stage.TIMELINE).to_dict() for issue in issues],
        "valid": not issues,
    }
    _emit(payload, None)
    return 0 if not issues else int(ExitCode.INVALID_INPUT)


def _emit(payload: dict[str, Any], json_out: str | None) -> None:
    text = json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=False)
    print(text)
    if json_out:
        temp = f"{json_out}.part-{os.getpid()}"
        with open(temp, "w", encoding="utf-8") as handle:
            handle.write(text + "\n")
        os.replace(temp, json_out)


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "demo":
            return run_demo(args)
        if args.command == "verify":
            return run_verify(args)
        if args.command == "inspect":
            return run_inspect(args)
    except ConfigError as exc:
        print(json.dumps({"error": str(exc)}), file=sys.stderr)
        return int(ExitCode.INVALID_INPUT)
    except KeyboardInterrupt:
        return 130
    parser.error("unknown command")
    return int(ExitCode.INVALID_INPUT)


if __name__ == "__main__":  # pragma: no cover - console entry
    raise SystemExit(main())
