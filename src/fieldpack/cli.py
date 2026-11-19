"""Command-line interface: demo / convert / validate / probe.

Exit codes: 0 = all acceptance assertions passed; 1 = acceptance metrics
not met; 2 = argument or dependency error; 3 = unrecoverable I/O failure.
All output is terminal text (JSON with --json); no frame payload is ever
logged — only offsets, digests and counts.
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .model import FieldPackError

EXIT_OK, EXIT_METRICS, EXIT_USAGE, EXIT_IO = 0, 1, 2, 3


def safe_resolve(base: Path, *parts: str) -> Path:
    """Resolve parts under base, rejecting path escapes."""
    target = (base / Path(*parts)).resolve()
    root = base.resolve()
    if target != root and root not in target.parents:
        raise FieldPackError(
            code="path-escape",
            message=f"path {target} escapes working directory {root}",
            recoverable=False,
        )
    return target


def _check_dependencies() -> str | None:
    missing = []
    for mod in ("av", "numpy", "soundfile"):
        try:
            __import__(mod)
        except ImportError:
            missing.append(mod)
    if missing:
        return f"missing dependencies: {', '.join(missing)} — run: .venv/bin/python -m pip install -e '.[test]'"
    return None


def _run_one(source: Path, out: Path, audio: Path | None, args) -> dict:
    from .mapping import build_plan
    from .mux import Muxer
    from .pipeline import Pipeline
    from .source import probe_source
    from .validate import validate_output

    tracks = probe_source(source)
    specs = [(f"cam{dev}", dev) for dev in sorted(tracks)]
    plan = build_plan(specs, audio, tracks)
    muxer = Muxer(out, plan, tracks)
    pipeline = Pipeline(queue_size=args.queue_size,
                        slow_consumer_ms=args.slow_consumer_ms,
                        chunk_records=args.chunk_records)
    stats = pipeline.run(source, plan, muxer)
    muxer.close()
    report = validate_output(out, muxer.index_path)
    return {
        "source": source.name,
        "output": out.name,
        "mapping": plan.summary(tracks),
        "frames_written": stats.frames_written,
        "records_read": stats.records_read,
        "corrupt_records": stats.corrupt_records,
        "recovered_records": stats.recovered_records,
        "quarantined": stats.quarantined,
        "queue_peak": stats.queue_peak_items,
        "queue_peak_bytes": stats.queue_peak_bytes,
        "producer_wait_ms": round(stats.producer_wait_ns / 1e6, 3),
        "consumer_wait_ms": round(stats.consumer_wait_ns / 1e6, 3),
        "cancelled": stats.cancelled,
        "roundtrip_consistency": round(report.consistency, 6),
        "keyframe_hit_rate": round(report.keyframes.hit_rate, 6) if report.keyframes else 1.0,
        "keyframe_index_errors": report.keyframes.errors if report.keyframes else [],
        "segment_hash_match": round(report.segment_hash_rate, 6),
        "audio_ok": report.audio_ok,
        "first_error_offset": stats.first_error_offset,
        "errors": [e.as_dict() for e in stats.errors[:20]],
    }


def _cmd_demo(args) -> int:
    from . import fixtures

    work = Path(args.work_dir)
    work.mkdir(parents=True, exist_ok=True)
    if args.generate_fixtures:
        paths = fixtures.generate_all(work)
    else:
        paths = {k: work / n for k, n in
                 (("good", "good.fpk"), ("corrupt", "corrupt.fpk"), ("audio", "companion.wav"))}
        missing = [str(p) for p in paths.values() if not p.is_file()]
        if missing:
            print(f"fixtures missing: {missing}; pass --generate-fixtures", file=sys.stderr)
            return EXIT_USAGE

    good = _run_one(paths["good"], safe_resolve(work, "good_out.nut"), paths["audio"], args)
    corrupt = _run_one(paths["corrupt"], safe_resolve(work, "corrupt_out.nut"), None, args)

    assertions = {
        "good.roundtrip_consistency == 1.0": good["roundtrip_consistency"] == 1.0,
        "good.keyframe_hit_rate == 1.0": good["keyframe_hit_rate"] == 1.0,
        "good.segment_hash_match == 1.0": good["segment_hash_match"] == 1.0,
        "good.audio_ok": good["audio_ok"] is True,
        "corrupt.corrupt_records >= 3": corrupt["corrupt_records"] >= 3,
        "corrupt.recovered_records > 0": corrupt["recovered_records"] > 0,
        "corrupt.first_error_offset is not null": corrupt["first_error_offset"] is not None,
    }
    passed = all(assertions.values())
    result = {
        "assertions_passed": passed,
        "assertions": assertions,
        "good": good,
        "corrupt": corrupt,
    }
    if args.json:
        print(json.dumps(result, indent=1))
    else:
        for name, run in (("good", good), ("corrupt", corrupt)):
            print(f"[{name}] consistency={run['roundtrip_consistency']:.3f} "
                  f"keyframe_hit={run['keyframe_hit_rate']:.3f} "
                  f"segment_hash={run['segment_hash_match']:.3f} "
                  f"queue_peak={run['queue_peak']} "
                  f"corrupt={run['corrupt_records']} recovered={run['recovered_records']} "
                  f"first_error_offset={run['first_error_offset']}")
        for text, ok in assertions.items():
            print(f"  {'PASS' if ok else 'FAIL'}  {text}")
        print("RESULT:", "OK" if passed else "ACCEPTANCE METRICS NOT MET")
    return EXIT_OK if passed else EXIT_METRICS


def _parse_map_spec(spec: str) -> tuple[str, int]:
    try:
        name, dev = spec.split("=device:", 1)
        return name, int(dev)
    except ValueError:
        raise argparse.ArgumentTypeError(
            f"invalid --map spec {spec!r}; expected NAME=device:N")


def _cmd_convert(args) -> int:
    from .mapping import build_plan
    from .mux import Muxer
    from .pipeline import Pipeline
    from .source import probe_source

    source = Path(args.input)
    if not source.is_file():
        print(f"input not found: {source}", file=sys.stderr)
        return EXIT_IO
    out = safe_resolve(Path.cwd(), args.output)
    tracks = probe_source(source)
    specs = [_parse_map_spec(s) for s in args.map] or [(f"cam{d}", d) for d in sorted(tracks)]
    plan = build_plan(specs, args.audio, tracks)
    muxer = Muxer(out, plan, tracks)
    stats = Pipeline(queue_size=args.queue_size,
                     slow_consumer_ms=args.slow_consumer_ms,
                     chunk_records=args.chunk_records).run(source, plan, muxer)
    muxer.close()
    summary = {"output": str(out), "frames_written": stats.frames_written,
               "corrupt_records": stats.corrupt_records,
               "recovered_records": stats.recovered_records,
               "quarantined": stats.quarantined,
               "first_error_offset": stats.first_error_offset,
               "mapping": plan.summary(tracks)}
    print(json.dumps(summary, indent=1) if args.json else
          f"wrote {out} frames={stats.frames_written} corrupt={stats.corrupt_records}")
    return EXIT_OK


def _cmd_validate(args) -> int:
    from .validate import validate_output

    out = Path(args.container)
    index = out.with_suffix(out.suffix + ".index.json")
    if not out.is_file() or not index.is_file():
        print(f"container or index missing: {out}", file=sys.stderr)
        return EXIT_IO
    report = validate_output(out, index)
    result = {
        "roundtrip_consistency": round(report.consistency, 6),
        "keyframe_hit_rate": round(report.keyframes.hit_rate, 6) if report.keyframes else 1.0,
        "segment_hash_match": round(report.segment_hash_rate, 6),
        "audio_ok": report.audio_ok,
        "errors": [e.as_dict() for e in report.errors[:20]],
    }
    print(json.dumps(result, indent=1) if args.json else
          f"consistency={report.consistency:.3f} segment_hash={report.segment_hash_rate:.3f}")
    ok = report.consistency == 1.0 and report.segment_hash_rate == 1.0
    return EXIT_OK if ok else EXIT_METRICS


def _cmd_probe(args) -> int:
    from .source import iter_records, probe_source

    tracks = probe_source(args.input)
    errors = [e for e in iter_records(args.input) if isinstance(e, FieldPackError)]
    result = {
        "tracks": {str(d): vars(t) for d, t in tracks.items()},
        "error_count": len(errors),
        "errors": [e.as_dict() for e in errors[:20]],
    }
    print(json.dumps(result, indent=1))
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="fieldpack", description=__doc__)
    sub = parser.add_subparsers(dest="cmd", required=True)

    def common(p):
        p.add_argument("--queue-size", type=int, default=8)
        p.add_argument("--slow-consumer-ms", type=int, default=0)
        p.add_argument("--chunk-records", type=int, default=64)
        p.add_argument("--json", action="store_true")

    demo = sub.add_parser("demo", help="generate fixtures and run acceptance")
    demo.add_argument("--generate-fixtures", action="store_true")
    demo.add_argument("--work-dir", default="demo_out")
    common(demo)

    conv = sub.add_parser("convert", help="remux a frame-record file")
    conv.add_argument("input")
    conv.add_argument("-o", "--output", required=True)
    conv.add_argument("--map", action="append", default=[],
                      metavar="NAME=device:N")
    conv.add_argument("--audio", default=None)
    common(conv)

    val = sub.add_parser("validate", help="re-scan a delivered container")
    val.add_argument("container")
    val.add_argument("--json", action="store_true")

    probe = sub.add_parser("probe", help="scan a source file and report tracks/errors")
    probe.add_argument("input")
    return parser


def main(argv: list[str] | None = None) -> int:
    dep_error = _check_dependencies()
    if dep_error:
        print(dep_error, file=sys.stderr)
        return EXIT_USAGE
    parser = build_parser()
    args = parser.parse_args(argv)  # argparse itself exits 2 on bad args
    try:
        if args.cmd == "demo":
            return _cmd_demo(args)
        if args.cmd == "convert":
            return _cmd_convert(args)
        if args.cmd == "validate":
            return _cmd_validate(args)
        if args.cmd == "probe":
            return _cmd_probe(args)
    except FieldPackError as exc:
        print(str(exc), file=sys.stderr)
        return EXIT_IO if not exc.recoverable else EXIT_METRICS
    except OSError as exc:
        print(f"I/O failure: {exc}", file=sys.stderr)
        return EXIT_IO
    return EXIT_USAGE


if __name__ == "__main__":
    raise SystemExit(main())
