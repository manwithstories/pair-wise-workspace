"""Command line entry point.

Commands
--------
``scan``    parse a raw stream and print access-unit / keyframe geometry
``index``   validate a keyframe index against the stream
``run``     execute a timeline end to end and write the report
``verify``  re-hash every frame recorded in a manifest
``demo``    generate fixtures and run the full acceptance scenario

Every command prints a single JSON document to stdout (human-readable lines go
to stderr), so the output is machine-checkable.  Exit codes follow the
contract: ``0`` pass, ``2`` invalid input/timeline, ``3`` unrecoverable media
error, ``4`` resource or output error.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from . import EXIT_INPUT_INVALID, EXIT_OK, EXIT_RESOURCE, __version__
from .errors import ErrorCategory, QcError, make_error

__all__ = ["main", "build_parser"]


def _emit(payload: dict) -> None:
    sys.stdout.write(json.dumps(payload, indent=2, sort_keys=False) + "\n")
    sys.stdout.flush()


def _fail(exc: QcError) -> int:
    sys.stdout.write(json.dumps({"error": exc.to_dict()}, indent=2) + "\n")
    sys.stderr.write(f"bcastqc: {exc}\n")
    return exc.exit_code


def _cmd_scan(args: argparse.Namespace) -> int:
    from .bitstream import Codec, KeyframeIndex, StreamScanner, compute_index, detect_codec, validate_index

    path = Path(args.stream)
    if not path.is_file():
        raise make_error(ErrorCategory.PATH_ESCAPE, "stream not found", path=str(path))
    size = path.stat().st_size
    with path.open("rb") as handle:
        head = handle.read(4096)
    codec = Codec.parse(args.codec) if args.codec else detect_codec(head)
    scanner = StreamScanner(codec, max_buffer=args.max_buffer)
    units = []
    with path.open("rb") as handle:
        while chunk := handle.read(1 << 16):
            units.extend(scanner.feed(chunk))
        units.extend(scanner.finish())
    index = compute_index(units)
    payload = {
        "stream": str(path),
        "codec": codec.value,
        "bytes": size,
        "access_units": len(units),
        "keyframes": len(index.entries),
        "keyframe_index": index.to_json(),
        "access_unit_offsets": [unit.to_json() for unit in units] if args.verbose else None,
    }
    if args.index:
        submitted = KeyframeIndex.from_file(Path(args.index))
        validation = validate_index(submitted, size, keyframes=units, policy=args.policy)
        payload["index_check"] = validation.to_json()
        if not validation.accepted:
            payload["exit_code"] = EXIT_INPUT_INVALID
            _emit(payload)
            return EXIT_INPUT_INVALID
    payload["exit_code"] = EXIT_OK
    _emit(payload)
    return EXIT_OK


def _cmd_index(args: argparse.Namespace) -> int:
    from .bitstream import Codec, KeyframeIndex, detect_codec, StreamScanner, validate_index

    stream = Path(args.stream)
    index = KeyframeIndex.from_file(Path(args.index))
    size = stream.stat().st_size
    with stream.open("rb") as handle:
        head = handle.read(4096)
    codec = Codec.parse(args.codec) if args.codec else detect_codec(head)
    if not args.no_scan:
        scanner = StreamScanner(codec)
        units = []
        with stream.open("rb") as handle:
            while chunk := handle.read(1 << 16):
                units.extend(scanner.feed(chunk))
            units.extend(scanner.finish())
    else:
        units = None
    validation = validate_index(index, size, keyframes=units, policy=args.policy)
    payload = {
        "stream": str(stream),
        "codec": codec.value,
        "stream_bytes": size,
        "submitted_entries": len(index.entries),
        "validation": validation.to_json(),
        "exit_code": EXIT_OK if validation.accepted else EXIT_INPUT_INVALID,
    }
    if validation.recovered_index is not None and args.write_recovered:
        Path(args.write_recovered).write_text(
            json.dumps({"keyframes": validation.recovered_index.to_json()}, indent=2) + "\n",
            encoding="utf-8",
        )
        payload["recovered_index"] = args.write_recovered
    _emit(payload)
    return payload["exit_code"]


def _cmd_run(args: argparse.Namespace) -> int:
    from .pipeline import RunParams, run_pipeline
    from .timeline import load_timeline

    timeline = load_timeline(Path(args.timeline), strict=args.strict)
    params = RunParams(
        seed=args.seed,
        drop_rate=args.drop_rate,
        hard_loss_rate=args.hard_loss_rate,
        block_frames=args.block_frames,
        queue_blocks=args.queue_blocks,
        slow_consumer_timeout=args.consumer_timeout,
    )
    result = run_pipeline(
        timeline,
        Path(args.out),
        params,
        resume=args.resume,
    )
    payload = result.to_json()
    payload["timeline"] = str(args.timeline)
    payload["excluded_tracks"] = sorted(timeline.excluded_tracks)
    _emit(payload)
    return result.report.exit_code


def _cmd_verify(args: argparse.Namespace) -> int:
    from .pipeline import verify_manifest

    result = verify_manifest(Path(args.manifest))
    _emit(result)
    return result["exit_code"]


def _cmd_demo(args: argparse.Namespace) -> int:
    from .demo import run_demo

    result = run_demo(
        seed=args.seed,
        drop_rate=args.drop_rate,
        queue_frames=args.queue_frames,
        consumer_delay_ms=args.consumer_delay_ms,
        out=Path(args.out),
        frames=args.frames,
        keep_existing=args.keep_existing,
    )
    _emit(result)
    return int(result["exit_code"])


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="bcastqc", description="Broadcast raw-bitstream QC")
    parser.add_argument("--version", action="version", version=f"bcastqc {__version__}")
    sub = parser.add_subparsers(dest="command", required=True)

    scan = sub.add_parser("scan", help="parse a raw stream and list access units")
    scan.add_argument("--stream", required=True)
    scan.add_argument("--codec", choices=["h264", "h265", "av1"])
    scan.add_argument("--index", help="keyframe index JSON to validate against the stream")
    scan.add_argument("--policy", choices=["scan", "reject"], default="scan")
    scan.add_argument("--max-buffer", type=int, default=8 * 1024 * 1024)
    scan.add_argument("--verbose", action="store_true")
    scan.set_defaults(func=_cmd_scan)

    index = sub.add_parser("index", help="validate a keyframe index")
    index.add_argument("--stream", required=True)
    index.add_argument("--index", required=True)
    index.add_argument("--codec", choices=["h264", "h265", "av1"])
    index.add_argument("--policy", choices=["scan", "reject"], default="scan")
    index.add_argument("--no-scan", action="store_true", help="validate offsets only")
    index.add_argument("--write-recovered", help="write the scanned index here")
    index.set_defaults(func=_cmd_index)

    run = sub.add_parser("run", help="run a timeline end to end")
    run.add_argument("--timeline", required=True)
    run.add_argument("--out", required=True)
    run.add_argument("--seed", type=int, default=41)
    run.add_argument("--drop-rate", type=float, default=0.0)
    run.add_argument("--hard-loss-rate", type=float, default=0.0)
    run.add_argument("--block-frames", type=int, default=8)
    run.add_argument("--queue-blocks", type=int, default=1)
    run.add_argument("--consumer-timeout", type=float, default=30.0)
    run.add_argument("--resume", action="store_true")
    run.add_argument("--strict", action="store_true", help="reject unreferenced tracks")
    run.set_defaults(func=_cmd_run)

    verify = sub.add_parser("verify", help="re-hash frames recorded in a manifest")
    verify.add_argument("--manifest", required=True)
    verify.set_defaults(func=_cmd_verify)

    demo = sub.add_parser("demo", help="generate fixtures and run the acceptance scenario")
    demo.add_argument("--seed", type=int, default=41)
    demo.add_argument("--drop-rate", type=float, default=0.02)
    demo.add_argument("--queue-frames", type=int, default=8)
    demo.add_argument("--consumer-delay-ms", type=float, default=15.0)
    demo.add_argument("--frames", type=int, default=75)
    demo.add_argument("--out", default="/tmp/bcastqc-demo")
    demo.add_argument("--keep-existing", action="store_true")
    demo.set_defaults(func=_cmd_demo)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except QcError as exc:
        return _fail(exc)
    except BrokenPipeError:  # pragma: no cover - terminal closed
        return EXIT_RESOURCE


if __name__ == "__main__":  # pragma: no cover - module entry point
    raise SystemExit(main())
