"""Terminal entry point.

Exit codes are part of the contract:

* ``0`` - acceptance passed;
* ``1`` - acceptance differs (a threshold was missed);
* ``2`` - the command line or an input was wrong;
* ``3`` - processing failed in a way that cannot be recovered.

Every subcommand prints human-readable lines on stdout and, with ``--json``, a
single machine-readable document on stdout.  Errors go to stderr so a JSON
consumer can parse stdout unconditionally.
"""

from __future__ import annotations

import argparse
import json
import sys
import tempfile
import threading
from pathlib import Path

from .audio import inspect_audio
from .demo import run_demo, run_verify
from .demux import Demuxer
from .fixtures import FORMAT_PRESETS
from .keyframes import build_index, nearest_keyframe, validate_index
from .mapping import resolve_mapping
from .pipeline import (
    Pipeline,
    PipelineConfig,
    compare_roundtrip,
    part_path,
    read_checkpoint,
    timeline_of,
)
from .probe import probe
from .timebase import AircheckError, ExitCode, InputError, UsageError, format_ticks

DEFAULT_FORMATS = "mov,mp4,mkv"


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="aircheck",
        description=(
            "Bounded, resumable multi-track demux, mapping, lossless remux and "
            "verification for broadcast master assets."
        ),
    )
    parser.add_argument("--version", action="store_true", help="print the version and exit")
    subparsers = parser.add_subparsers(dest="command")

    demo = subparsers.add_parser(
        "demo",
        help="generate fixtures, process them and print acceptance metrics",
    )
    demo.add_argument(
        "--formats",
        default=DEFAULT_FORMATS,
        help=f"comma-separated containers to exercise (default: {DEFAULT_FORMATS})",
    )
    demo.add_argument(
        "--queue-size",
        type=int,
        default=4,
        help="maximum items held in each pipeline queue (default: 4)",
    )
    demo.add_argument(
        "--slow-consumer-ms",
        type=int,
        default=0,
        help="artificial delay per packet in the writer, to exercise backpressure",
    )
    demo.add_argument(
        "--output",
        type=Path,
        default=None,
        help="directory for fixtures and outputs (default: a temporary directory)",
    )
    demo.add_argument(
        "--target",
        choices=sorted(FORMAT_PRESETS),
        default="mkv",
        help="container for remuxed outputs (default: mkv)",
    )
    demo.add_argument("--json", action="store_true", help="emit a JSON report on stdout")

    verify = subparsers.add_parser(
        "verify",
        help="re-check an existing demo output directory",
    )
    verify.add_argument("--input", type=Path, required=True, help="demo output directory")
    verify.add_argument("--json", action="store_true", help="emit a JSON report on stdout")

    probe_cmd = subparsers.add_parser("probe", help="print container and stream metadata")
    probe_cmd.add_argument("--input", type=Path, required=True)
    probe_cmd.add_argument("--json", action="store_true")

    map_cmd = subparsers.add_parser(
        "map", help="validate a stream mapping and timeline for one asset"
    )
    map_cmd.add_argument("--input", type=Path, required=True)
    map_cmd.add_argument(
        "--map",
        dest="map_spec",
        default=None,
        help="explicit mapping, e.g. 'v:0=a:1' (default: copy every media stream)",
    )
    map_cmd.add_argument(
        "--segment-ms", type=int, default=500, help="timeline segment length (default: 500)"
    )
    map_cmd.add_argument("--transition-ms", type=int, default=0)
    map_cmd.add_argument("--json", action="store_true")

    remux_cmd = subparsers.add_parser(
        "remux", help="losslessly remux one asset and verify the round trip"
    )
    remux_cmd.add_argument("--input", type=Path, required=True)
    remux_cmd.add_argument("--output", type=Path, required=True)
    remux_cmd.add_argument(
        "--target", choices=sorted(FORMAT_PRESETS), default="mkv"
    )
    remux_cmd.add_argument("--map", dest="map_spec", default=None)
    remux_cmd.add_argument("--segment-ms", type=int, default=500)
    remux_cmd.add_argument("--queue-size", type=int, default=4)
    remux_cmd.add_argument("--slow-consumer-ms", type=int, default=0)
    remux_cmd.add_argument("--resume", action="store_true")
    remux_cmd.add_argument(
        "--cancel-after-ms",
        type=int,
        default=0,
        help="cancel the run after this delay to exercise interrupt/resume",
    )
    remux_cmd.add_argument("--json", action="store_true")

    index_cmd = subparsers.add_parser("index", help="build and validate the keyframe index")
    index_cmd.add_argument("--input", type=Path, required=True)
    index_cmd.add_argument("--json", action="store_true")

    audio_cmd = subparsers.add_parser("audio", help="sample audio channels, peak and silence")
    audio_cmd.add_argument("--input", type=Path, required=True)
    audio_cmd.add_argument("--sample-stride", type=int, default=8)
    audio_cmd.add_argument("--json", action="store_true")

    return parser


def _emit_json(payload: dict) -> None:
    json.dump(payload, sys.stdout, indent=2, ensure_ascii=False)
    sys.stdout.write("\n")


def _print_error(error: AircheckError) -> None:
    print(f"error[{error.category.value}]: {error.message}", file=sys.stderr)
    if error.context:
        print(
            "  context: " + json.dumps(error.context, ensure_ascii=False, sort_keys=True),
            file=sys.stderr,
        )


def _parse_formats(raw: str) -> list[str]:
    formats = [token.strip().lower() for token in raw.split(",") if token.strip()]
    if not formats:
        raise UsageError("--formats selected nothing", raw=raw)
    unknown = [name for name in formats if name not in FORMAT_PRESETS]
    if unknown:
        raise UsageError(
            "unsupported format requested",
            formats=unknown,
            supported=sorted(FORMAT_PRESETS),
        )
    return formats


def _cmd_demo(args: argparse.Namespace) -> int:
    formats = _parse_formats(args.formats)
    if args.queue_size <= 0:
        raise UsageError("--queue-size must be positive", value=args.queue_size)
    if args.slow_consumer_ms < 0:
        raise UsageError("--slow-consumer-ms must not be negative")
    if args.output is None:
        directory = Path(tempfile.mkdtemp(prefix="aircheck-demo-"))
    else:
        directory = args.output
    report = run_demo(
        directory,
        formats=formats,
        queue_size=args.queue_size,
        slow_consumer_ms=args.slow_consumer_ms,
        target_format=args.target,
    )
    if args.json:
        _emit_json(report.to_dict())
    else:
        for line in report.summary_lines():
            print(line)
        print(f"output_dir={report.output_dir}")
        for asset in report.assets:
            state = "ok" if asset.ok else "isolated"
            detail = ",".join(asset.probe_issues) if asset.probe_issues else "-"
            print(f"  {asset.kind:7} {asset.name:26} {state:9} {detail}")
        if report.notes:
            for note in report.notes:
                print(f"note: {note}")
    return int(ExitCode.OK if report.acceptable else ExitCode.ACCEPTANCE_DIFF)


def _cmd_verify(args: argparse.Namespace) -> int:
    report = run_verify(args.input)
    if args.json:
        _emit_json(report.to_dict())
    else:
        for line in report.summary_lines():
            print(line)
        for note in report.notes:
            print(f"note: {note}")
    return int(ExitCode.OK if report.acceptable else ExitCode.ACCEPTANCE_DIFF)


def _cmd_probe(args: argparse.Namespace) -> int:
    result = probe(args.input, decode_check=True)
    if args.json:
        _emit_json(result.to_dict())
    else:
        print(f"format={result.format_name} streams={len(result.streams)} "
              f"duration={format_ticks(result.duration_ticks)}")
        for stream in result.streams:
            print(
                f"  #{stream.index} {stream.kind:5} {stream.codec:12} "
                f"tb={stream.time_base} start={format_ticks(stream.start_ticks)}"
            )
        for issue in result.issues:
            print(f"  issue[{issue.kind}] stream={issue.stream_index} {issue.detail}")
    if result.unreadable:
        return int(ExitCode.INPUT)
    return int(ExitCode.OK)


def _cmd_map(args: argparse.Namespace) -> int:
    result = probe(args.input)
    mapping = resolve_mapping(result, args.map_spec)
    segment_ticks = args.segment_ms * 1_000_000
    if segment_ticks <= 0:
        raise UsageError("--segment-ms must be positive")
    packets = list(Demuxer(probe=result, segment_ticks=segment_ticks).iter_packets())
    timeline = timeline_of(
        packets,
        segment_ticks=segment_ticks,
        duration_ticks=result.duration_ticks or 0,
        transition_ms=args.transition_ms,
    )
    payload = {
        "probe": result.to_dict(),
        "mapping": mapping.to_dict(),
        "timeline": timeline.to_dict(),
        "exit_code": int(ExitCode.OK if timeline.ok else ExitCode.UNRECOVERABLE),
    }
    if args.json:
        _emit_json(payload)
    else:
        for item in mapping.assignments:
            print(f"map {item.kind}:{item.input_index} -> out {item.output_index}")
        if mapping.dropped:
            print(f"dropped streams: {mapping.dropped}")
        print(f"timeline_ok={timeline.ok} segments={timeline.segments}")
        for issue in timeline.issues:
            print(f"  issue[{issue.kind}] {issue.detail}")
    return int(ExitCode.OK if timeline.ok else ExitCode.UNRECOVERABLE)


def _cmd_remux(args: argparse.Namespace) -> int:
    result = probe(args.input, decode_check=True)
    if result.unreadable:
        raise InputError(
            "input could not be opened",
            path=str(args.input),
            open_error=result.open_error,
        )
    mapping = resolve_mapping(result, args.map_spec)
    segment_ticks = args.segment_ms * 1_000_000
    if segment_ticks <= 0:
        raise UsageError("--segment-ms must be positive")
    config = PipelineConfig(
        segment_ticks=segment_ticks,
        target_format=args.target,
        queue_size=args.queue_size,
        slow_consumer_ms=args.slow_consumer_ms,
        resume=args.resume,
    )
    cancel = threading.Event()
    if args.cancel_after_ms:
        threading.Timer(args.cancel_after_ms / 1000.0, cancel.set).start()
    pipeline = Pipeline(result, mapping, args.output, config, cancel=cancel)
    stage = pipeline.run()

    summary: dict[str, object] = {"stage": stage.to_dict()}
    exit_code = ExitCode.OK
    if stage.ok:
        output_probe = probe(stage.output)
        source_packets = list(Demuxer(probe=result, segment_ticks=segment_ticks).iter_packets())
        output_packets = list(
            Demuxer(probe=output_probe, segment_ticks=segment_ticks).iter_packets()
        )
        comparison, drift, _plan = compare_roundtrip(source_packets, output_packets)
        summary["hash_comparison"] = comparison.to_dict()
        summary["timeline_drift"] = drift
        if not comparison.all_match:
            exit_code = ExitCode.ACCEPTANCE_DIFF
    elif stage.cancelled:
        exit_code = ExitCode.UNRECOVERABLE
        checkpoint = read_checkpoint(args.output)
        summary["checkpoint"] = checkpoint.to_dict() if checkpoint else None
        summary["resume_part"] = str(part_path(args.output, (checkpoint.part_index + 1) if checkpoint else 1))
    else:
        exit_code = ExitCode.UNRECOVERABLE
    summary["exit_code"] = int(exit_code)

    if args.json:
        _emit_json(summary)
    else:
        print(f"ok={stage.ok} output={stage.output} packets={stage.packets_out} "
              f"segments={stage.segments}")
        if "hash_comparison" in summary:
            comparison = summary["hash_comparison"]
            print(f"segment_hash_match={comparison['match_ratio']:.3f} "
                  f"all_match={comparison['all_match']}")
        for note in stage.notes:
            print(f"note: {note}")
        for error in stage.errors:
            print(f"error[{error.get('category')}]: {error.get('message')}", file=sys.stderr)
    return int(exit_code)


def _cmd_index(args: argparse.Namespace) -> int:
    result = probe(args.input)
    if result.unreadable:
        raise InputError("input could not be opened", path=str(args.input))
    indexes = build_index(args.input, result)
    report = validate_index(args.input, indexes, probe_result=result)
    payload = report.to_dict()
    payload["exit_code"] = int(ExitCode.OK if report.ok else ExitCode.ACCEPTANCE_DIFF)
    if args.json:
        _emit_json(payload)
    else:
        print(f"keyframes={report.keyframe_count} hit_rate={report.hit_rate:.3f} "
              f"ok={report.ok}")
        for problem in report.problems:
            print(f"  issue[{problem.kind}] {problem.detail}")
        first = indexes[0] if indexes else None
        if first and first.entries:
            entry = nearest_keyframe(first, first.entries[-1].pts_ticks)
            if entry:
                print(f"  last keyframe stream={entry.stream_index} "
                      f"pts={format_ticks(entry.pts_ticks)} pos={entry.pos}")
    return int(ExitCode.OK if report.ok else ExitCode.ACCEPTANCE_DIFF)


def _cmd_audio(args: argparse.Namespace) -> int:
    result = probe(args.input, scan_packets=False)
    if result.unreadable:
        raise InputError("input could not be opened", path=str(args.input))
    report = inspect_audio(args.input, result, sample_stride=args.sample_stride)
    payload = report.to_dict()
    payload["exit_code"] = int(ExitCode.OK if report.ok else ExitCode.ACCEPTANCE_DIFF)
    if args.json:
        _emit_json(payload)
    else:
        for stream in report.streams:
            print(
                f"stream#{stream.stream_index} channels={stream.channels} "
                f"rate={stream.sample_rate} peak={stream.peak:.4f} "
                f"silence={len(stream.silence)} failures={stream.decode_failures}"
            )
            for anomaly in stream.anomalies:
                print(f"  anomaly[{anomaly.kind}] {anomaly.detail}")
    return int(ExitCode.OK if report.ok else ExitCode.ACCEPTANCE_DIFF)


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    if getattr(args, "version", False):
        from . import __version__

        print(__version__)
        return int(ExitCode.OK)
    if args.command is None:
        parser.print_help()
        return int(ExitCode.USAGE)
    handlers = {
        "demo": _cmd_demo,
        "verify": _cmd_verify,
        "probe": _cmd_probe,
        "map": _cmd_map,
        "remux": _cmd_remux,
        "index": _cmd_index,
        "audio": _cmd_audio,
    }
    try:
        return handlers[args.command](args)
    except AircheckError as error:
        _print_error(error)
        return int(error.exit_code)
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        return int(ExitCode.UNRECOVERABLE)
    except OSError as error:
        print(f"error[input]: {error}", file=sys.stderr)
        return int(ExitCode.INPUT)


if __name__ == "__main__":  # pragma: no cover - module entry point
    raise SystemExit(main())
