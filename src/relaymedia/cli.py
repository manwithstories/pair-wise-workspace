"""Terminal adapter: ``demo`` and ``verify`` commands.

``demo`` generates the deterministic fixture set, runs the bounded pipeline
twice (a fresh run and an immediate resume), archives every artifact and
prints the quantitative summary required for acceptance. ``verify`` reopens an
archived run directory, re-hashes its outputs, re-checks damage localisation
against the recorded truth and validates the checkpoint chain, returning the
documented exit codes:

* 0 — every assertion passed;
* 2 — an input/usage error;
* 3 — verification failed;
* 4 — checkpoint no longer matches the input or parameters.
"""

from __future__ import annotations

import argparse
import dataclasses
import hashlib
import json
import sys
import time
from fractions import Fraction
from pathlib import Path

from .checkpoint import CheckpointMismatch, digest_file, digest_json, load_checkpoint
from .errors import (
    EXIT_INPUT_ERROR,
    EXIT_OK,
    EXIT_VERIFY_FAILED,
    InputError,
    RelayError,
    VerifyFailure,
)
from .pipeline import PipelineConfig, run_pipeline

_SUMMARY_NAME = "summary.json"
_TRUTH_NAME = "truth.json"
_REQUIRED_FIELDS = (
    "bad_record_count",
    "first_error_offset_bytes",
    "offset_error_bytes",
    "resume_seconds",
    "segment_count",
    "duplicate_count",
    "output_sha256",
    "peak_buffer_bytes",
)


def config_digest(config: PipelineConfig) -> str:
    data = dataclasses.asdict(config)
    normalised = {
        key: (str(value) if isinstance(value, Fraction) else value)
        for key, value in data.items()
    }
    return digest_json(normalised)


def hash_outputs(work_dir: str | Path) -> tuple[str, dict[str, str]]:
    """Hash every archived artifact in sorted order for a stable aggregate."""
    work = Path(work_dir)
    per_file: dict[str, str] = {}
    aggregate = hashlib.sha256()
    for path in sorted(p for p in work.rglob("*") if p.is_file()
                       and p.name not in {_SUMMARY_NAME, "checkpoint.json"}):
        digest = digest_file(path)
        per_file[str(path.relative_to(work))] = digest
        aggregate.update(path.relative_to(work).as_posix().encode("utf-8"))
        aggregate.update(digest.encode("ascii"))
    return aggregate.hexdigest(), per_file


def _localization_error(truth: dict, summary: dict) -> tuple[int, int]:
    """Worst damage-localisation error in bytes and lines across all records."""
    byte_errors: list[int] = []
    line_errors: list[int] = []
    reported = summary.get("damage_locations", [])
    by_file = {item["file"]: item for item in reported}
    for key in ("media_damage", "subtitle_damage", "timecode_damage"):
        spec = truth.get(key)
        if not spec:
            continue
        actual = by_file.get(spec["file"])
        if actual is None:
            continue
        if "offset_bytes" in actual and actual["offset_bytes"] >= 0:
            byte_errors.append(abs(actual["offset_bytes"] - spec["true_offset_bytes"]))
        if "line" in actual and actual["line"] >= 0:
            line_errors.append(abs(actual["line"] - spec["true_line"]))
    return (max(byte_errors) if byte_errors else 0,
            max(line_errors) if line_errors else 0)


def _collect_damage_locations(result) -> list[dict]:
    locations: list[dict] = []
    for error in result.errors:
        if error.offset_bytes < 0:
            continue
        locations.append({
            "file": Path(error.source).name,
            "offset_bytes": error.offset_bytes,
            "line": error.line,
            "stage": error.stage,
            "media_time": error.media_time,
        })
    return locations


def run_demo(args: argparse.Namespace) -> int:
    # Fixtures live in the project's tests/ directory; add it explicitly so
    # the import works regardless of the current working directory.
    tests_dir = Path(__file__).resolve().parents[2] / "tests"
    sys.path.insert(0, str(tests_dir))
    from generate_fixtures import generate_fixtures

    out = Path(args.out)
    fixtures = out / "fixtures"
    work = out / "run"
    out.mkdir(parents=True, exist_ok=True)
    truth = generate_fixtures(fixtures, seed=args.seed)

    config = PipelineConfig(queue_bytes=args.queue_bytes, queue_items=args.queue_items)
    params = config_digest(config)
    sources = [
        ("node-a", str(fixtures / "audio/node-a.wav")),
        ("node-b", str(fixtures / "audio/node-b.wav")),
        ("node-c", str(fixtures / "audio/node-c.wav")),
        ("node-d-damaged", str(fixtures / "audio/node-damaged.aac")),
    ]
    input_digest = digest_json(
        {name: digest_file(path) for name, path in sorted(sources)}
    )
    videos = sorted(str(p) for p in (fixtures / "video").glob("*.mp4"))
    common = dict(
        sources=sources,
        subtitle_path=str(fixtures / "subtitles/damaged.srt"),
        video_paths=videos,
        config=config,
        input_digest=input_digest,
        params_digest=params,
        timecode_path=str(fixtures / "timecodes.txt"),
    )

    started = time.perf_counter()
    result = run_pipeline(work_dir=str(work), **common)
    first_elapsed = time.perf_counter() - started

    # Second run must resume from the checkpoint and finish inside 2 seconds.
    resume_start = time.perf_counter()
    resumed = run_pipeline(work_dir=str(work), **common)
    resume_seconds = time.perf_counter() - resume_start

    output_sha, per_file = hash_outputs(work)
    result.output_sha256 = output_sha
    damage_locations = _collect_damage_locations(result)
    offset_error_bytes, offset_error_lines = _localization_error(
        truth, {"damage_locations": damage_locations}
    )
    summary = {
        "seed": args.seed,
        "input_digest": input_digest,
        "params_digest": params,
        "bad_record_count": result.bad_record_count,
        "first_error_offset_bytes": result.first_error_offset_bytes,
        "offset_error_bytes": offset_error_bytes,
        "offset_error_lines": offset_error_lines,
        "segment_count": result.segment_count,
        "duplicate_count": result.duplicate_count,
        "peak_buffer_bytes": result.peak_buffer_bytes,
        "buffer_limit_bytes": args.queue_bytes,
        "output_sha256": output_sha,
        "resume_seconds": round(resume_seconds, 6),
        "first_run_seconds": round(first_elapsed, 6),
        "resumed": resumed.resumed,
        "damage_locations": damage_locations,
        "output_files": sorted(per_file),
        "channel_maps": [dataclasses.asdict(m) for m in result.channel_maps],
        "color_conflicts": [r.source for r in result.color_reports if r.has_conflict],
        "silence_run_count": len(result.silence_intervals),
    }
    (out / _TRUTH_NAME).write_text(json.dumps(truth, indent=2, sort_keys=True))
    (out / _SUMMARY_NAME).write_text(json.dumps(summary, indent=2, sort_keys=True))
    _print_summary(summary)
    return EXIT_OK


def _print_summary(summary: dict) -> None:
    print("relaymedia demo summary")
    for key in _REQUIRED_FIELDS:
        print(f"{key}={summary[key]}")
    print(f"buffer_within_limit={summary['peak_buffer_bytes'] <= summary['buffer_limit_bytes']}")
    print(f"color_conflict_sources={len(summary['color_conflicts'])}")
    print(f"silence_run_count={summary['silence_run_count']}")


def run_verify(args: argparse.Namespace) -> int:
    directory = Path(args.dir)
    summary_path = directory / _SUMMARY_NAME
    truth_path = directory / _TRUTH_NAME
    if not summary_path.exists() or not truth_path.exists():
        print(f"input error: missing {_SUMMARY_NAME} or {_TRUTH_NAME} in {directory}",
              file=sys.stderr)
        return EXIT_INPUT_ERROR
    summary = json.loads(summary_path.read_text())
    truth = json.loads(truth_path.read_text())

    failures: list[str] = []
    current_hash, _ = hash_outputs(directory / "run")
    if current_hash != summary["output_sha256"]:
        failures.append(
            f"output_sha256 drift: {current_hash} != {summary['output_sha256']}"
        )
    byte_error, line_error = _localization_error(truth, summary)
    if byte_error > truth["media_damage"]["tolerance_bytes"]:
        failures.append(f"damage byte error {byte_error} exceeds tolerance")
    if line_error > 1:
        failures.append(f"damage line error {line_error} exceeds one line")
    if summary["peak_buffer_bytes"] > summary["buffer_limit_bytes"]:
        failures.append("peak buffer exceeded configured limit")
    if not summary.get("resumed"):
        failures.append("resume run did not reuse the checkpoint")

    checkpoint = load_checkpoint(directory / "run")
    if checkpoint is None:
        failures.append("checkpoint missing")
    else:
        if checkpoint.cursor < 1:
            failures.append("checkpoint recorded no completed stages")
        # The checkpoint must still be bound to the recorded inputs/params.
        if checkpoint.input_digest != summary.get("input_digest"):
            failures.append("checkpoint input digest no longer matches inputs")
        if checkpoint.params_digest != summary.get("params_digest"):
            failures.append("checkpoint parameter digest no longer matches params")
        expected_stages = {"subtitle", "timeline", "fingerprint", "write"}
        missing_stages = expected_stages - set(checkpoint.stages)
        if missing_stages:
            failures.append(f"checkpoint missing stages: {sorted(missing_stages)}")

    if failures:
        for failure in failures:
            print(f"verify failure: {failure}", file=sys.stderr)
        # A broken checkpoint binding is the dedicated resume-mismatch class.
        if any("digest" in f for f in failures):
            from .errors import EXIT_CHECKPOINT_MISMATCH
            return EXIT_CHECKPOINT_MISMATCH
        return EXIT_VERIFY_FAILED
    print("verify ok")
    print(f"offset_error_bytes={byte_error}")
    print(f"offset_error_lines={line_error}")
    print(f"output_sha256={current_hash}")
    print(f"resume_seconds={summary['resume_seconds']}")
    print(f"peak_buffer_bytes={summary['peak_buffer_bytes']}")
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="relaymedia")
    sub = parser.add_subparsers(dest="command", required=True)

    demo = sub.add_parser("demo", help="generate fixtures and run the pipeline")
    demo.add_argument("--out", required=True)
    demo.add_argument("--seed", type=int, default=42)
    demo.add_argument("--queue-bytes", type=int, default=1 << 20)
    demo.add_argument("--queue-items", type=int, default=8)
    demo.set_defaults(func=run_demo)

    verify = sub.add_parser("verify", help="re-verify an archived demo directory")
    verify.add_argument("--dir", required=True)
    verify.set_defaults(func=run_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except CheckpointMismatch as exc:
        print(f"checkpoint mismatch: {exc.message}", file=sys.stderr)
        return exc.exit_code
    except VerifyFailure as exc:
        print(f"verify failure: {exc.message}", file=sys.stderr)
        return EXIT_VERIFY_FAILED
    except (InputError, RelayError) as exc:
        print(f"input error: {exc.message}", file=sys.stderr)
        return EXIT_INPUT_ERROR


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
