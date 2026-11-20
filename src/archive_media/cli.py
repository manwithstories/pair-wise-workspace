"""Terminal entry point for archive recovery and verification.

Commands
--------

``verify``
    Parse a manifest, run the two-pass bounded-queue recovery, build and
    verify the keyframe index, and print the acceptance metrics as both
    human-readable ``key=value`` lines and a JSON report.  Exit codes:

    * 0 -- success, including recovered-but-damaged segments,
    * 2 -- fatal damage under the active policy (``--strict``),
    * 3 -- invalid parameters or manifest syntax.
"""

from __future__ import annotations

import argparse
import json
import sys
from dataclasses import dataclass
from pathlib import Path

from .audio import AudioConfig
from .errors import (
    Code,
    DiagnosticCollector,
    FatalDamageError,
    ManifestSyntaxError,
    RecoveryError,
)
from .index import (
    INDEX_NAME,
    KeyframeIndex,
    build_index_from_results,
    verify_hit_rate,
)
from .manifest import parse_manifest
from .pipeline import Policy
from .pipeline_runner import RecoveryRunner
from .timeline import to_float_seconds
from .verify import (
    input_fingerprint,
    roundtrip_consistency,
    timeline_preservation,
    verify_segment_hashes,
)


@dataclass
class CliOptions:
    input_path: Path
    output_dir: Path
    rate: int
    layout: str
    target_lufs: float
    peak_ceiling_dbfs: float
    queue_size: int
    index_tolerance_ms: float
    strict: bool


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="archive-media",
        description="Recover cold-storage HLS/DASH audio into a verified archive",
    )
    sub = parser.add_subparsers(dest="command", required=True)
    verify = sub.add_parser("verify", help="run recovery and print verification metrics")
    verify.add_argument("--input", required=True, help="HLS .m3u8 or DASH .mpd manifest")
    verify.add_argument("--output", required=True, help="output archive directory")
    verify.add_argument("--rate", type=int, default=48000, help="target sample rate")
    verify.add_argument(
        "--layout", default="stereo",
        choices=("mono", "stereo", "5.1", "7.1"), help="target channel layout",
    )
    verify.add_argument("--target-lufs", type=float, default=-18.0)
    verify.add_argument("--peak-ceiling-dbfs", type=float, default=-1.0)
    verify.add_argument("--queue-size", type=int, default=4)
    verify.add_argument("--index-tolerance-ms", type=float, default=40.0)
    verify.add_argument(
        "--strict", action="store_true",
        help="stop (exit 2) on any damaged segment instead of repairing it",
    )
    verify.add_argument(
        "--policy", choices=tuple(p.value for p in Policy), default=None,
        help="explicit damage policy; overrides --strict when given",
    )
    return parser


def _options(args: argparse.Namespace) -> CliOptions:
    if args.rate <= 0:
        raise ManifestSyntaxError(
            Code.RESAMPLE_FAILURE, "--rate must be a positive integer",
            detail={"rate": args.rate},
        )
    if args.queue_size < 1:
        raise ManifestSyntaxError(
            Code.MANIFEST_SYNTAX, "--queue-size must be >= 1",
            detail={"queue_size": args.queue_size},
        )
    if not (-120.0 <= args.target_lufs <= 0.0):
        raise ManifestSyntaxError(
            Code.MANIFEST_SYNTAX,
            "--target-lufs must lie between -120 and 0",
            detail={"target_lufs": args.target_lufs},
        )
    return CliOptions(
        input_path=Path(args.input),
        output_dir=Path(args.output),
        rate=args.rate,
        layout=args.layout,
        target_lufs=args.target_lufs,
        peak_ceiling_dbfs=args.peak_ceiling_dbfs,
        queue_size=args.queue_size,
        index_tolerance_ms=args.index_tolerance_ms,
        strict=args.strict,
    )


def _resolve_policy(options: CliOptions, explicit: str | None) -> Policy:
    if explicit:
        return Policy(explicit)
    return Policy.STOP if options.strict else Policy.GAP


def _index_metrics(
    runner: RecoveryRunner, options: CliOptions
) -> tuple[KeyframeIndex, float, list[dict[str, object]]]:
    index, index_diags = build_index_from_results(
        [runner.decode_cache[s.sequence] for s in runner.plan.segments]
    )
    runner.collector.extend(index_diags)
    index.write(options.output_dir / INDEX_NAME)
    expected: list = []
    for result in runner.decode_cache.values():
        for chunk in result.chunks:
            # Index entries use segment-relative PTS; compare on that same
            # basis and identify the segment via the miss locator instead.
            if chunk.pts_seconds is not None:
                expected.append(chunk.pts_seconds)
    hit_rate, miss_diags = verify_hit_rate(
        index, expected, options.index_tolerance_ms
    )
    runner.collector.extend(miss_diags)
    return index, hit_rate, [d.to_json() for d in miss_diags]


def run_verify(options: CliOptions, policy: Policy) -> tuple[int, dict[str, object]]:
    plan = parse_manifest(options.input_path)
    config = AudioConfig(
        options.rate,
        options.layout,
        target_lufs=options.target_lufs,
        peak_ceiling_dbfs=options.peak_ceiling_dbfs,
    )
    runner = RecoveryRunner(
        plan, config, options.output_dir,
        policy=policy, queue_size=options.queue_size,
    )
    runner.run()

    index, hit_rate, index_misses = _index_metrics(runner, options)
    roundtrip_score, roundtrip_diags = roundtrip_consistency(plan)
    runner.collector.extend(roundtrip_diags)
    hash_report = verify_segment_hashes(runner.outputs)
    coverage, coverage_diags = timeline_preservation(
        plan, runner.outputs, options.rate
    )
    runner.collector.extend(coverage_diags)

    report: dict[str, object] = {
        "input": str(options.input_path),
        "output": str(options.output_dir),
        "policy": policy.value,
        "fingerprint": input_fingerprint(plan),
        "roundtrip_consistency": round(roundtrip_score, 4),
        "keyframe_hit_rate": round(hit_rate, 4),
        "keyframe_index_entries": len(index.entries),
        "keyframe_index_misses": len(index_misses),
        "segment_sha256_count": hash_report.count,
        "segment_sha256_mismatches": hash_report.mismatches,
        "timeline_coverage": round(coverage, 4),
        "queue_peak_items": runner.metrics.queue_peak_items,
        "queue_capacity": options.queue_size,
        "backpressure_seconds": runner.metrics.backpressure_seconds,
        "recovered_segments": runner.metrics.recovered_segments,
        "skipped_segments": runner.metrics.skipped_segments,
        "gap_segments": runner.metrics.gap_segments,
        "damaged_segments": runner.collector.damaged_segments,
        "planned_segments": len(plan.segments),
        "gain_db": round(runner.gain_db, 4),
        "peak_limited": runner.peak_limited,
        "segments": runner.outputs,
        "diagnostics": runner.collector.to_json(),
    }
    (options.output_dir / "recovery_report.json").write_text(
        json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return 0, report


def _print_metrics(report: dict[str, object]) -> None:
    ordered_keys = (
        "roundtrip_consistency",
        "keyframe_hit_rate",
        "segment_sha256_count",
        "segment_sha256_mismatches",
        "queue_peak_items",
        "recovered_segments",
        "damaged_segments",
    )
    for key in ordered_keys:
        print(f"{key}={report[key]}")


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    try:
        options = _options(args)
        if args.command != "verify":  # pragma: no cover - argparse enforces this
            parser.error("unknown command")
        policy = _resolve_policy(options, args.policy)
        exit_code, report = run_verify(options, policy)
    except ManifestSyntaxError as exc:
        print(f"parameter/manifest error {exc.code.value}: {exc.message}",
              file=sys.stderr)
        if exc.locator.manifest_path:
            print(f"  at {exc.locator.to_json()}", file=sys.stderr)
        return 3
    except FatalDamageError as exc:
        diag = exc.diagnostic()
        print(f"fatal {exc.code.value}: {exc.message}", file=sys.stderr)
        print(f"  classification: {exc.code.value}", file=sys.stderr)
        print(f"  locator: {json.dumps(diag.locator.to_json())}", file=sys.stderr)
        return 2
    except (RecoveryError, ValueError, FileNotFoundError) as exc:
        message = getattr(exc, "message", str(exc))
        code = getattr(exc, "code", Code.MANIFEST_SYNTAX)
        print(f"error {getattr(code, 'value', code)}: {message}", file=sys.stderr)
        return 3
    _print_metrics(report)
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
