"""tlkit command line interface.

Exit codes: 0 ok, 1 unexpected error, 2 input/version validation failure,
3 unresolved conflicts remain, 4 frame-plan multi-master or hash mismatch.
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .conflicts import detect
from .formats import v1
from .formats.registry import FormatError, load
from .model import frames_to_seconds
from .render import RenderError, frame_plan, plan_hash
from .resolve import POLICIES, resolve

EXIT_OK = 0
EXIT_UNEXPECTED = 1
EXIT_INPUT = 2
EXIT_UNRESOLVED = 3
EXIT_RENDER = 4


def _emit(obj) -> None:
    print(json.dumps(obj, indent=2, sort_keys=True))


def _seconds_str(frames: int, fps) -> str:
    sec = frames_to_seconds(frames, fps)
    return f"{sec.numerator}/{sec.denominator}"


def _intervals(tl) -> list[dict]:
    out = []
    for tr in tl.tracks:
        for c in tr.clips():
            if c.enabled:
                out.append({"track": tr.name, "id": c.id,
                            "start": c.range.start, "end": c.range.end})
    out.sort(key=lambda e: (e["track"], e["start"], e["id"]))
    return out


def _tracks_json(tl) -> list[dict]:
    return [{"name": tr.name, "kind": tr.kind, "clips": len(tr.clips())}
            for tr in tl.tracks]


def _checked_hash(tl) -> tuple[str, list[dict]]:
    plan = frame_plan(tl)
    first = plan_hash(tl, plan)
    second = plan_hash(tl, frame_plan(tl))
    if first != second:
        raise RenderError(f"frame plan hash mismatch: {first} != {second}")
    return first, plan


def cmd_gen_fixtures(args) -> int:
    from .fixtures import generate
    _emit(generate(args.out, args.seed, args.clips))
    return EXIT_OK


def cmd_validate(args) -> int:
    results, ok = [], True
    for path in args.files:
        try:
            tl, report = load(path)
            results.append({
                "file": path, "ok": True,
                "format_version": report.format_version,
                "tracks": len(tl.tracks), "clips": tl.clip_count(),
                "duration_frames": tl.duration,
                "rounding_residuals": len(report.rounding),
            })
        except FormatError as exc:
            ok = False
            results.append({"file": path, "ok": False,
                            "path": exc.path, "error": exc.message})
    _emit({"ok": ok, "files": results})
    return EXIT_OK if ok else EXIT_INPUT


def cmd_conflicts(args) -> int:
    tl, report = load(args.file)
    found = detect(tl)
    _emit({"format_version": report.format_version,
           "conflicts_detected": len(found),
           "conflicts": [c.to_json() for c in found]})
    return EXIT_OK


def cmd_resolve(args) -> int:
    tl, report = load(args.file)
    before = tl.duration
    result = resolve(tl, policy=args.policy, max_iter=args.max_iter)
    delta = result.timeline.duration - before
    plan_hash_value = None
    if result.unresolved == 0:
        plan_hash_value, _ = _checked_hash(result.timeline)
    if args.out:
        Path(args.out).parent.mkdir(parents=True, exist_ok=True)
        Path(args.out).write_text(
            json.dumps(v1.dump(result.timeline), indent=2, sort_keys=True) + "\n",
            encoding="utf-8")
    _emit({
        "format_version": report.format_version,
        "policy": args.policy,
        "tracks": _tracks_json(result.timeline),
        "intervals": _intervals(result.timeline),
        "conflicts_detected": result.detected,
        "conflicts_resolved": result.detected - result.unresolved,
        "unresolved": result.unresolved,
        "iterations": result.iterations,
        "cycle_detected": result.cycle,
        "duration_delta_frames": delta,
        "duration_delta_seconds": _seconds_str(delta, tl.fps),
        "frame_plan_hash": plan_hash_value,
        "decision_log": result.decisions,
        "resolved_file": args.out,
    })
    return EXIT_UNRESOLVED if result.unresolved else EXIT_OK


def cmd_plan(args) -> int:
    tl, report = load(args.file)
    plan_hash_value, plan = _checked_hash(tl)
    summary = {
        "format_version": report.format_version,
        "duration_frames": tl.duration,
        "duration_seconds": _seconds_str(tl.duration, tl.fps),
        "runs": len(plan),
        "frame_plan_hash": plan_hash_value,
    }
    if args.json:
        summary["plan"] = plan
        _emit(summary)
    else:
        print(f"format_version: {summary['format_version']}")
        print(f"duration: {summary['duration_frames']} frames "
              f"({summary['duration_seconds']} s)")
        print(f"runs: {summary['runs']}")
        print(f"frame_plan_hash: {summary['frame_plan_hash']}")
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="tlkit", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("gen-fixtures", help="generate paired v1/v2 fixtures")
    p.add_argument("--out", default="fixtures")
    p.add_argument("--seed", type=int, default=7)
    p.add_argument("--clips", type=int, default=400)
    p.set_defaults(func=cmd_gen_fixtures)

    p = sub.add_parser("validate", help="parse and validate exchange files")
    p.add_argument("files", nargs="+")
    p.set_defaults(func=cmd_validate)

    p = sub.add_parser("conflicts", help="list detected conflicts")
    p.add_argument("file")
    p.set_defaults(func=cmd_conflicts)

    p = sub.add_parser("resolve", help="resolve conflicts to a fixpoint")
    p.add_argument("file")
    p.add_argument("--policy", choices=POLICIES, default="track-priority")
    p.add_argument("--max-iter", type=int, default=512)
    p.add_argument("--out", default=None)
    p.set_defaults(func=cmd_resolve)

    p = sub.add_parser("plan", help="render frame plan and ownership hash")
    p.add_argument("file")
    p.add_argument("--json", action="store_true")
    p.set_defaults(func=cmd_plan)
    return parser


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except FormatError as exc:
        print(json.dumps({"path": exc.path, "error": exc.message}),
              file=sys.stderr)
        return EXIT_INPUT
    except RenderError as exc:
        print(json.dumps({"error": str(exc)}), file=sys.stderr)
        return EXIT_RENDER
    except Exception as exc:  # noqa: BLE001 - top-level guard
        print(json.dumps({"error": f"unexpected: {exc!r}"}), file=sys.stderr)
        return EXIT_UNEXPECTED


if __name__ == "__main__":
    sys.exit(main())
