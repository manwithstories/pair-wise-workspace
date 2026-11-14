"""Terminal UI: argparse subcommands and process exit codes."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from . import batch, profile as profile_mod, report as report_mod, wavio
from . import framing, noise
from .errors import (EXIT_INTERRUPTED, EXIT_OK, EXIT_PARTIAL, EXIT_PROFILE,
                     AudioCutError, OutputPathError)
from .store import Store


def _resolve_output(path, out_dir):
    p = Path(path)
    if p.is_symlink() or (p.parent.exists() and p.parent.is_symlink()):
        raise OutputPathError(f"refusing symlinked output path: {path}")
    if out_dir is not None:
        root = Path(out_dir).resolve()
        try:
            p.resolve().relative_to(root)
        except ValueError:
            raise OutputPathError(f"output {path} escapes --out-dir {out_dir}")
    p.parent.mkdir(parents=True, exist_ok=True)
    return p


def _cmd_calibrate(args) -> int:
    info = wavio.read_info(args.input)
    frame, hop = framing.frame_params(info.sample_rate, args.frame_ms, args.hop_ms)
    env = framing.envelope_dbfs(wavio.iter_blocks(path=args.input, info=info), frame, hop)
    floor = noise.estimate_noise_floor(env.env_db)
    floor_db = float(floor.mean()) if floor.size else framing.FLOOR_DB
    prof = profile_mod.create_profile(
        info.sample_rate, args.frame_ms, args.hop_ms, floor_db)
    out = _resolve_output(args.out, args.out_dir)
    profile_mod.save_profile(prof, out)
    print(json.dumps({"profile": str(out), "sample_rate": info.sample_rate,
                      "noise_floor_db": round(floor_db, 2)}))
    return EXIT_OK


def _cmd_scan(args) -> int:
    prof = profile_mod.load_profile(args.profile)
    db = _resolve_output(args.db, args.out_dir)
    store = Store(db)
    try:
        summary = batch.scan_directory(args.input, store, prof)
    finally:
        store.close()
    print(json.dumps({"ok": summary.ok, "failed": summary.failed,
                      "skipped": summary.skipped,
                      "failures": summary.failures}, ensure_ascii=False))
    if summary.profile_mismatch:
        return EXIT_PROFILE
    return EXIT_OK if summary.failed == 0 else EXIT_PARTIAL


def _cmd_report(args) -> int:
    store = Store(_resolve_existing(args.db))
    try:
        data = report_mod.build_report(store)
    finally:
        store.close()
    if args.json:
        print(json.dumps(data, ensure_ascii=False, separators=(",", ":")))
    else:
        print(f"files={data['files']} segments={data['segments']} "
              f"total_silence_ms={data['total_silence_ms']}")
    return EXIT_OK


def _cmd_export(args) -> int:
    prof = profile_mod.load_profile(args.profile)
    store = Store(_resolve_existing(args.db))
    out = _resolve_output(args.out, args.out_dir)
    try:
        count = report_mod.export_labels(store, prof, out)
    finally:
        store.close()
    print(json.dumps({"labels": count, "out": str(out)}))
    return EXIT_OK


def _resolve_existing(path):
    if not Path(path).exists():
        raise OutputPathError(f"database not found: {path}")
    return path


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="audiocut")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("calibrate", help="build a calibration profile")
    p.add_argument("--input", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--frame-ms", type=float, default=20.0)
    p.add_argument("--hop-ms", type=float, default=10.0)
    p.add_argument("--out-dir")
    p.set_defaults(func=_cmd_calibrate)

    p = sub.add_parser("scan", help="scan a directory of WAV files")
    p.add_argument("--input", required=True)
    p.add_argument("--db", required=True)
    p.add_argument("--profile", required=True)
    p.add_argument("--out-dir")
    p.set_defaults(func=_cmd_scan)

    p = sub.add_parser("report", help="summarize the index")
    p.add_argument("--db", required=True)
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=_cmd_report)

    p = sub.add_parser("export", help="export segment labels as jsonl")
    p.add_argument("--db", required=True)
    p.add_argument("--profile", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--out-dir")
    p.set_defaults(func=_cmd_export)
    return parser


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except AudioCutError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return exc.exit_code
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        return EXIT_INTERRUPTED


if __name__ == "__main__":
    sys.exit(main())
