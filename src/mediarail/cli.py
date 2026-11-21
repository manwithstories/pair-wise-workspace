"""Terminal entry point: run / resume / verify.

Exit codes: 0 = completed and index verified; 2 = unrepairable material
present; 3 = input changed or checkpoint untrusted; 4 = write or
verification failure.
"""

from __future__ import annotations

import argparse
import json
import sys
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from .checkpoint import CheckpointStore, sha256_file
from .errors import CheckpointError, Severity, WriteFailure
from .index import read_index, verify_index
from .pipeline import (
    CancelToken,
    FileResult,
    MemoryBudget,
    process_file,
)

MEDIA_SUFFIXES = {".mp4", ".mov"}


def _print_result(result: FileResult) -> None:
    first = next(
        (e for e in result.errors if e.severity in (Severity.UNREPAIRABLE, Severity.FATAL)),
        None,
    )
    parts = [
        f"file={result.path.name}",
        f"ok={int(result.ok)}",
        f"repaired_timestamps={result.repaired_timestamps}",
        f"index_entries={result.index_entries}",
        f"peak_queue_packets={result.peak_queue_packets}",
        f"backpressure_seconds={result.backpressure_seconds:.3f}",
    ]
    if result.output_sha256:
        parts.append(f"sha256={result.output_sha256}")
    if first is not None:
        parts.append(f"first_error_packet={first.packet_index}")
        parts.append(f"byte_offset={first.byte_offset}")
        parts.append(f"error={first.code}")
    print(" ".join(parts))


def _result_dict(result: FileResult) -> dict:
    return {
        "file": str(result.path),
        "ok": result.ok,
        "unrepairable": result.unrepairable,
        "packets_scanned": result.packets_scanned,
        "repaired_timestamps": result.repaired_timestamps,
        "index_entries": result.index_entries,
        "peak_queue_packets": result.peak_queue_packets,
        "backpressure_seconds": round(result.backpressure_seconds, 6),
        "resumed_from_packet": result.resumed_from_packet,
        "resume_seconds": round(result.resume_seconds, 6),
        "output_sha256": result.output_sha256,
        "tracks": [
            {
                "track": s.track,
                "packets": s.packets,
                "repaired_timestamps": s.repaired_timestamps,
                "duplicate_timestamps": s.duplicate_timestamps,
                "negative_durations": s.negative_durations,
                "wraps": s.wraps,
                "drift_segments": [
                    {
                        "start": d.start_index,
                        "end": d.end_index,
                        "drift_ppm": round(d.drift_ppm, 2),
                    }
                    for d in s.drift_segments
                ],
            }
            for s in result.track_stats
        ],
        "errors": [e.to_dict() for e in result.errors[:50]],
    }


def cmd_run(args: argparse.Namespace) -> int:
    inputs = sorted(
        p for p in Path(args.input).iterdir() if p.suffix.lower() in MEDIA_SUFFIXES
    )
    output_dir = Path(args.output)
    output_dir.mkdir(parents=True, exist_ok=True)
    store = CheckpointStore(args.checkpoint)
    budget = MemoryBudget(args.budget)
    cancel = CancelToken()
    results: list[FileResult] = []
    try:
        with ThreadPoolExecutor(max_workers=args.workers) as pool:
            futures = [
                pool.submit(
                    process_file,
                    path,
                    output_dir / f"{path.stem}.mp4",
                    store,
                    args.queue_size,
                    budget,
                    cancel,
                )
                for path in inputs
            ]
            for future in futures:
                results.append(future.result())
    except CheckpointError as exc:
        print(f"checkpoint_error={exc}", file=sys.stderr)
        return 3
    except WriteFailure as exc:
        print(f"write_failure={exc}", file=sys.stderr)
        return 4
    ok = sum(1 for r in results if r.ok)
    unrepairable = sum(1 for r in results if r.unrepairable)
    for r in results:
        _print_result(r)
    print(f"files={len(results)} ok={ok} unrepairable={unrepairable}")
    if args.report_json:
        print(
            json.dumps(
                {
                    "files": len(results),
                    "ok": ok,
                    "unrepairable": unrepairable,
                    "results": [_result_dict(r) for r in results],
                },
                indent=1,
            )
        )
    return 2 if unrepairable else 0


def cmd_resume(args: argparse.Namespace) -> int:
    input_path = Path(args.input)
    output_path = Path(args.output)
    store = CheckpointStore(args.checkpoint)
    budget = MemoryBudget()
    cancel = CancelToken()
    try:
        result = process_file(
            input_path,
            output_path,
            store,
            args.queue_size,
            budget,
            cancel,
            key=input_path.stem,
        )
    except CheckpointError as exc:
        print(f"checkpoint_error={exc}", file=sys.stderr)
        return 3
    except WriteFailure as exc:
        print(f"write_failure={exc}", file=sys.stderr)
        return 4
    print(
        f"resumed_from_packet={result.resumed_from_packet} "
        f"resume_seconds={result.resume_seconds:.3f} "
        f"output_sha256={result.output_sha256}"
    )
    _print_result(result)
    if result.unrepairable:
        return 2
    return 0 if result.ok else 4


def cmd_verify(args: argparse.Namespace) -> int:
    media = Path(args.input)
    index = read_index(args.index)
    errors = verify_index(media, index)
    if args.sha256:
        print(f"sha256={sha256_file(media)}")
    print(f"index_entries={len(index.entries)} verify_errors={len(errors)}")
    for err in errors[:10]:
        print(str(err), file=sys.stderr)
    return 4 if errors else 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mediarail")
    sub = parser.add_subparsers(dest="command", required=True)

    run = sub.add_parser("run", help="batch process a directory of MP4/MOV")
    run.add_argument("--input", required=True)
    run.add_argument("--output", required=True)
    run.add_argument("--checkpoint", required=True)
    run.add_argument("--queue-size", type=int, default=8)
    run.add_argument("--budget", type=int, default=256)
    run.add_argument("--workers", type=int, default=2)
    run.add_argument("--report-json", action="store_true")
    run.set_defaults(func=cmd_run)

    resume = sub.add_parser("resume", help="resume one interrupted input")
    resume.add_argument("--input", required=True)
    resume.add_argument("--output", required=True)
    resume.add_argument("--checkpoint", required=True)
    resume.add_argument("--queue-size", type=int, default=8)
    resume.set_defaults(func=cmd_resume)

    verify = sub.add_parser("verify", help="verify an output against its index")
    verify.add_argument("--input", required=True)
    verify.add_argument("--index", required=True)
    verify.add_argument("--sha256", action="store_true")
    verify.set_defaults(func=cmd_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
