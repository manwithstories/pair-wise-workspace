"""Terminal entry point: synth, analyze, export, query and check subcommands."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .config import Params
from .errors import QueryEmpty, TalkmapError
from .export import export_segments
from .refine import run_pipeline
from .store import Store
from .synth import synth_fixtures
from .util import expand_inputs, safe_resolve
from .wavio import WavReader


def _print(payload: dict, as_json: bool) -> None:
    if as_json:
        print(json.dumps(payload, ensure_ascii=False, indent=2))
    else:
        for key, value in payload.items():
            print(f"{key}: {value}")


def _add_param_args(parser: argparse.ArgumentParser) -> None:
    for name in ("frame-ms", "hop-ms", "block-ms", "enter-margin-db",
                 "exit-margin-db", "min-silence-ms", "min-speech-ms"):
        parser.add_argument(f"--{name}", type=float, default=argparse.SUPPRESS)


def _params_from_args(args: argparse.Namespace) -> Params:
    mapping = {
        "frame_ms": "frame_ms", "hop_ms": "hop_ms", "block_ms": "block_ms",
        "enter_margin_db": "enter_margin_db", "exit_margin_db": "exit_margin_db",
        "min_silence_ms": "min_silence_ms", "min_speech_ms": "min_speech_ms",
    }
    overrides = {field: getattr(args, field) for field in mapping
                 if hasattr(args, field)}
    params = Params(**overrides)
    params.validate()
    return params


def cmd_synth(args: argparse.Namespace) -> dict:
    result = synth_fixtures(args.out, args.seed, args.masters, args.minutes)
    return {"out": result["dir"], "files": result["files"], "truth": result["truth"]}


def cmd_analyze(args: argparse.Namespace) -> dict:
    params = _params_from_args(args)
    inputs = expand_inputs(args.inputs)
    if not inputs:
        raise TalkmapError("no input WAV files matched")
    total_segments = 0
    silence_seconds = 0.0
    by_file: dict[str, dict] = {}
    rows: list[dict] = []
    with Store(args.db) as store:
        for path in inputs:
            with WavReader(path) as reader:
                segments, _total = run_pipeline(reader, params)
                store.upsert_analysis(path, reader.info.sha256, params,
                                      reader.info, segments)
                rate = reader.info.sample_rate
                truncated = reader.info.truncated
            gap_samples = sum(
                max(0, segments[i].start_sample - segments[i - 1].end_sample)
                for i in range(1, len(segments)))
            silence_seconds += gap_samples / rate
            total_segments += len(segments)
            by_file[path.name] = {
                "segments": len(segments),
                "silence_seconds": round(gap_samples / rate, 3),
                "truncated": truncated,
            }
            for seg in segments:
                rows.append({"file": path.name,
                             "start_sample": seg.start_sample,
                             "end_sample": seg.end_sample,
                             "fade_in_ms": seg.fade_in_ms,
                             "fade_out_ms": seg.fade_out_ms})
    payload = {"files": len(inputs), "segments": total_segments,
               "silence_seconds": round(silence_seconds, 3),
               "segments_by_file": by_file}
    if args.json:
        payload["segment_rows"] = rows
    return payload


def cmd_export(args: argparse.Namespace) -> dict:
    params = _params_from_args(args)
    with Store(args.db) as store:
        return export_segments(store, args.out, params,
                               args.pre_roll_ms, args.post_roll_ms)


def cmd_query(args: argparse.Namespace) -> dict:
    with Store(args.db) as store:
        rows = store.query_segments(args.file, args.min_silence_ms,
                                    args.start_ms, args.end_ms)
    if not rows:
        raise QueryEmpty("query matched no segments")
    return {"matches": len(rows), "segments": rows}


def cmd_check(args: argparse.Namespace) -> dict:
    fixtures = safe_resolve(args.fixtures)
    truth = json.loads((fixtures / "truth.json").read_text())
    matched = total = 0
    max_boundary = 0
    max_silence_delta = 0
    with Store(args.db) as store:
        for frow in store.active_files():
            name = Path(frow["path"]).name
            if name not in truth:
                continue
            expected = truth[name]["segments"]
            actual = store.segments_for(frow["id"])
            total += len(expected)
            for exp, got in zip(expected, actual):
                err = max(abs(exp["start_sample"] - got["start_sample"]),
                          abs(exp["end_sample"] - got["end_sample"]))
                max_boundary = max(max_boundary, err)
                matched += 1
            for i in range(1, min(len(expected), len(actual))):
                exp_gap = expected[i]["start_sample"] - expected[i - 1]["end_sample"]
                got_gap = actual[i]["start_sample"] - actual[i - 1]["end_sample"]
                max_silence_delta = max(max_silence_delta, abs(exp_gap - got_gap))
    payload = {"matched": matched, "total": total,
               "max_boundary_error_samples": int(max_boundary),
               "silence_delta_samples": int(max_silence_delta)}
    if matched != total or max_boundary > 480 or max_silence_delta > 960:
        raise TalkmapError(f"check tolerance exceeded: {payload}")
    return payload


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="talkmap")
    sub = parser.add_subparsers(dest="command", required=True)

    p_synth = sub.add_parser("synth")
    p_synth.add_argument("--out", required=True)
    p_synth.add_argument("--seed", type=int, default=7)
    p_synth.add_argument("--masters", type=int, default=4)
    p_synth.add_argument("--minutes", type=int, default=3)
    p_synth.set_defaults(func=cmd_synth)

    p_analyze = sub.add_parser("analyze")
    p_analyze.add_argument("inputs", nargs="+")
    p_analyze.add_argument("--db", required=True)
    p_analyze.add_argument("--json", action="store_true")
    _add_param_args(p_analyze)
    p_analyze.set_defaults(func=cmd_analyze)

    p_export = sub.add_parser("export")
    p_export.add_argument("--db", required=True)
    p_export.add_argument("--out", required=True)
    p_export.add_argument("--pre-roll-ms", type=float, default=40.0)
    p_export.add_argument("--post-roll-ms", type=float, default=120.0)
    p_export.add_argument("--json", action="store_true")
    _add_param_args(p_export)
    p_export.set_defaults(func=cmd_export)

    p_query = sub.add_parser("query")
    p_query.add_argument("--db", required=True)
    p_query.add_argument("--file")
    p_query.add_argument("--min-silence-ms", type=float)
    p_query.add_argument("--start-ms", type=float)
    p_query.add_argument("--end-ms", type=float)
    p_query.add_argument("--json", action="store_true")
    p_query.set_defaults(func=cmd_query)

    p_check = sub.add_parser("check")
    p_check.add_argument("--db", required=True)
    p_check.add_argument("--fixtures", required=True)
    p_check.add_argument("--json", action="store_true")
    p_check.set_defaults(func=cmd_check)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        payload = args.func(args)
        _print(payload, getattr(args, "json", False) or args.command == "synth")
        return 0
    except QueryEmpty as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    except TalkmapError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return exc.exit_code


if __name__ == "__main__":
    raise SystemExit(main())
