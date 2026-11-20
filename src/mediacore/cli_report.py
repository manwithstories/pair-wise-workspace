"""Report construction, output verification and terminal rendering.

Kept out of the argument parser so the verification logic can be unit-tested
without building a namespace.
"""

from __future__ import annotations

import sys
from pathlib import Path

from .audio_io import file_sha256
from .errors import (
    EXIT_GRAPH,
    EXIT_OK,
    EXIT_OUTPUT,
    EXIT_RECOVERED,
    EXIT_USAGE,
    MediaError,
)
from .pipeline_types import PipelineSettings, RunResult
from .report import Report
from .timebase import samples_for_duration
from .timeline import Timeline

def _build_report(
    *,
    command: str,
    timeline: Timeline,
    result: RunResult,
    settings: PipelineSettings,
    workdir: Path,
) -> Report:
    report = Report(
        command=command,
        workdir=str(workdir),
        settings={
            "sample_rate": settings.sample_rate,
            "layout": settings.layout,
            "target_lufs": settings.target_lufs,
            "ceiling_dbfs": settings.ceiling_dbfs,
        },
    )
    report.scans = {
        track_id: scan.to_dict() for track_id, scan in result.damage.scans.items()
    }
    report.damage = result.damage.to_dict()
    report.graph = result.plan.to_dict()
    report.audio = {
        node_id: outcome.loudness
        for node_id, outcome in sorted(result.outcomes.items())
        if outcome.loudness
    }
    report.recovery = {
        "reused_nodes": result.reused_nodes,
        "rendered_nodes": result.rendered_nodes,
        "recovery_seconds": result.recovery_seconds,
        "output_sha256": result.output_sha256,
        "output_samples": result.output_samples,
        "output_path": str(result.output_path),
        "interrupted": result.interrupted,
        "skipped": result.skipped,
        "failed": result.failed,
    }
    return report


def _verify_output(
    workdir: Path,
    result: RunResult,
    report: Report,
    timeline: Timeline,
    settings: PipelineSettings,
) -> None:
    from .audio_io import file_sha256

    problems: list[MediaError] = []
    if len(result.output_sha256) != 64:
        problems.append(
            MediaError("output hash is not 64 hex chars", code="output.bad_hash",
                       severity="fatal")  # type: ignore[arg-type]
        )
    actual = file_sha256(result.output_path)
    if actual != result.output_sha256:
        problems.append(
            MediaError(
                "output file changed after assembly",
                code="output.hash_changed",
                context={"expected": result.output_sha256, "actual": actual},
            )
        )
    if result.output_samples <= 0:
        problems.append(
            MediaError("output has no samples", code="output.empty",
                       context={"samples": result.output_samples})
        )
    # Sample count must equal the sum of the track unions.
    expected = _expected_samples(timeline, settings)
    if expected and result.output_samples != expected:
        problems.append(
            MediaError(
                "output sample count does not match timeline",
                code="output.count_mismatch",
                context={
                    "expected_samples": expected,
                    "actual_samples": result.output_samples,
                    "delta": result.output_samples - expected,
                },
            )
        )
    # Repeatability: re-assembling the same segments must reproduce the hash.
    # This proves the persisted output is a deterministic function of the
    # checkpointed nodes — the property resume relies on.
    import tempfile

    from .pipeline_assembly import assemble_output

    with tempfile.TemporaryDirectory(prefix="mc-verify-") as tmp:
        recomputed_path, recomputed_hash, recomputed_samples = assemble_output(
            timeline,
            result.outcomes,
            output_path=Path(tmp) / "output.wav",
            sample_rate=settings.sample_rate,
            layout=settings.layout,
        )
        if recomputed_hash != result.output_sha256:
            problems.append(
                MediaError(
                    "re-assembly does not reproduce the output hash",
                    code="output.nondeterministic",
                    context={
                        "expected": result.output_sha256,
                        "recomputed": recomputed_hash,
                    },
                )
            )
        if recomputed_samples != result.output_samples:
            problems.append(
                MediaError(
                    "re-assembly sample count differs",
                    code="output.count_mismatch",
                    context={
                        "expected_samples": result.output_samples,
                        "recomputed_samples": recomputed_samples,
                    },
                )
            )
    report.recovery["verified"] = not problems
    report.errors.extend(problems)
    if problems:
        report.status = "failed"
        report.exit_code = EXIT_OUTPUT


def _expected_samples(timeline: Timeline, settings: PipelineSettings) -> int:
    """Sum of per-track union durations converted to samples."""
    from .timebase import samples_for_duration

    total = 0
    track_ids = sorted({c.track for c in timeline.clips.values()})
    for track_id in track_ids:
        clips = sorted(
            (c for c in timeline.clips.values() if c.track == track_id),
            key=lambda c: (c.start_us, c.id),
        )
        union = 0
        cursor = None
        for clip in clips:
            if cursor is None or clip.start_us > cursor:
                union += clip.end_us - clip.start_us
                cursor = clip.end_us
            elif clip.end_us > cursor:
                union += clip.end_us - cursor
                cursor = clip.end_us
        total += samples_for_duration(union, settings.sample_rate)
    return total


def _final_exit(report: Report) -> int:
    if report.status == "failed":
        return report.exit_code or EXIT_OUTPUT
    if report.damage.get("damaged"):
        report.status = "recovered"
        report.exit_code = EXIT_RECOVERED
        return EXIT_RECOVERED
    report.status = "ok"
    report.exit_code = EXIT_OK
    return EXIT_OK


def _exit_for(exc: MediaError) -> int:
    code = exc.code
    if code.startswith("graph"):
        return EXIT_GRAPH
    if code.startswith("output") or code.startswith("checkpoint"):
        return EXIT_OUTPUT
    if exc.recoverable:
        return EXIT_RECOVERED
    return EXIT_USAGE


def _print_headline(report: Report, workdir: Path) -> None:
    data = report.to_dict()
    print(f"workdir: {workdir}")
    print(f"status: {data['status']}  exit_code: {data['exit_code']}")
    print(f"damage_offset_frames: {data['damage_offset_frames']}")
    print(f"recovery_seconds: {data['recovery_seconds']:.6f}")
    print(f"reused_nodes: {data['reused_nodes']}")
    print(f"output_sha256: {data['output_sha256']}")
    print(f"timeline_drift_us: {data['timeline_drift_us']}")
    if data["damage"]["kinds"]:
        fault = data["damage"].get("first_fault")
        print(f"damage_kinds: {', '.join(data['damage']['kinds'])}")
        if fault:
            ctx = fault.get("context", {})
            print(
                "first_fault: "
                f"{fault['code']} track={ctx.get('track')} "
                f"packet={ctx.get('packet_ordinal')} pts_us={ctx.get('pts_us')}"
            )
    print(f"report: {workdir / 'report.json'}")


def _print_error(exc: MediaError) -> None:
    payload = exc.to_dict()
    print(f"error[{payload['code']}]: {payload['message']}", file=sys.stderr)
    if payload.get("context"):
        print(f"  context: {payload['context']}", file=sys.stderr)

