"""Command-line entry point: fixture / sweep / ladder / verify / report."""
from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

from . import __version__
from .ladder.builder import build_ladder, evaluate
from .media.yuv import digest_file
from .report.render import render_ladder_table, render_stats, render_verify_table, to_json
from .scheduler.grid import GridConflictError, detect_conflicts, expand, load_grid
from .scheduler.runner import run_sweep
from .store import db

EXIT_OK = 0
EXIT_INTERRUPTED = 130
EXIT_LADDER_INFEASIBLE = 2
EXIT_FAILED_TASKS = 3
EXIT_INPUT_ERROR = 4


def _cmd_fixture(args) -> int:
    from scripts.gen_fixture import main as fixture_main

    return fixture_main(["--out", args.out, "--frames", str(args.frames),
                         "--size", args.size, "--seed", str(args.seed)])


def _resolve_source_size(args) -> tuple[int, int]:
    if args.source_size:
        width, height = (int(part) for part in args.source_size.lower().split("x", 1))
        return width, height
    path = Path(args.source)
    if not path.is_file():
        raise FileNotFoundError(f"source not found: {path}")
    import re
    match = re.search(r"(\d{2,5})x(\d{2,5})", path.name)
    if match:
        return int(match.group(1)), int(match.group(2))
    meta_path = path.with_suffix(".meta.json")
    if meta_path.is_file():
        meta = json.loads(meta_path.read_text(encoding="utf-8"))
        return int(meta["width"]), int(meta["height"])
    raise GridConflictError("pass --source-size when no WxH name or .meta.json sidecar exists")


def _cmd_sweep(args) -> int:
    width, height = _resolve_source_size(args)
    digest = digest_file(args.source)
    grid = load_grid(args.grid)
    specs, duplicates = expand(grid, args.source, digest)
    conn = db.connect(args.db)
    try:
        db.initialize(conn)
        db.validate_schema(conn)
        detect_conflicts(db.existing_params(conn), specs)
        stats = run_sweep(conn, specs, duplicates, args.workers, args.db, width, height)
    finally:
        conn.close()
    payload = stats.as_dict()
    print(render_stats(payload))
    if args.json:
        print(to_json(payload))
    if stats.interrupted:
        # In-flight worker threads are non-daemon; exit immediately so the
        # interrupted tasks stay 'running' and are reclaimed as orphans on
        # the next invocation instead of being waited on here.
        sys.stdout.flush()
        sys.stderr.flush()
        os._exit(EXIT_INTERRUPTED)
    if stats.failed:
        return EXIT_FAILED_TASKS
    return EXIT_OK


def _cmd_ladder(args) -> int:
    conn = db.connect(args.db)
    try:
        db.validate_schema(conn)
        rows = db.completed_rows(conn)
        failed = db.counts(conn).get("failed", 0)
    finally:
        conn.close()
    if failed:
        print(f"refusing to build ladder: {failed} failed task(s)", file=sys.stderr)
        return EXIT_FAILED_TASKS
    result = build_ladder(rows, args.rungs, args.max_kbps, args.min_ratio, args.max_ratio)
    payload = result.as_dict()
    if result.feasible:
        print(render_ladder_table(result.rungs))
    else:
        print(result.reason, file=sys.stderr)
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(to_json(payload), encoding="utf-8")
    if args.json:
        print(to_json(payload))
    return EXIT_OK if result.feasible else EXIT_LADDER_INFEASIBLE


def _verify_rows(conn, ladder: list[dict]) -> list[dict]:
    rows = []
    by_key = {row["key"]: row for row in conn.execute("SELECT * FROM tasks WHERE status='done'")}
    for rung in ladder:
        row = by_key.get(rung["key"])
        if row is None:
            raise db.DatabaseError(f"ladder rung {rung['key']} missing or not done")
        rows.append(dict(row))
    return rows


def _cmd_verify(args) -> int:
    payload = json.loads(Path(args.ladder).read_text(encoding="utf-8"))
    constraints = payload.get("constraints") or {}
    min_ratio = args.min_ratio if args.min_ratio is not None else constraints.get("min_ratio", 1.3)
    max_ratio = args.max_ratio if args.max_ratio is not None else constraints.get("max_ratio", 2.2)
    max_kbps = args.max_kbps if args.max_kbps is not None else constraints.get("max_kbps", 1e9)
    conn = db.connect(args.db)
    try:
        db.validate_schema(conn)
        failed = db.counts(conn).get("failed", 0)
        if failed:
            print(f"{failed} failed task(s) in store", file=sys.stderr)
            return EXIT_FAILED_TASKS
        if not payload.get("feasible"):
            print(payload.get("reason", "ladder infeasible"), file=sys.stderr)
            return EXIT_LADDER_INFEASIBLE
        rungs = payload["rungs"]
        rows = _verify_rows(conn, rungs)
        checks = evaluate(rows, min_ratio, max_ratio, max_kbps)
        for row in rows:
            row["monotonic"] = {
                "bitrate_strictly_increasing": checks["bitrate_strictly_increasing"],
                "quality_nondecreasing": checks["quality_nondecreasing"],
                "ratio_in_range": checks["ratio_in_range"],
            }
        output = {"feasible": all(checks.values()), "checks": checks, "rungs": rows}
    finally:
        conn.close()
    print(to_json(output) if args.json else render_verify_table(rows))
    return EXIT_OK if all(checks.values()) else EXIT_LADDER_INFEASIBLE


def _cmd_report(args) -> int:
    conn = db.connect(args.db)
    try:
        db.validate_schema(conn)
        rows = db.completed_rows(conn)
        stats = db.counts(conn)
    finally:
        conn.close()
    payload = {"stats": stats, "tasks": rows}
    if args.json:
        print(to_json(payload))
    else:
        print(render_stats(stats))
        print(render_ladder_table(rows))
    return EXIT_OK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="abrl", description="ABR sweep and ladder toolkit")
    parser.add_argument("--version", action="version", version=__version__)
    sub = parser.add_subparsers(dest="command", required=True)

    fixture = sub.add_parser("fixture", help="generate synthetic YUV material")
    fixture.add_argument("--out", required=True)
    fixture.add_argument("--frames", type=int, default=96)
    fixture.add_argument("--size", default="640x360")
    fixture.add_argument("--seed", type=int, default=7)
    fixture.set_defaults(func=_cmd_fixture)

    sweep = sub.add_parser("sweep", help="run an encoding parameter sweep")
    sweep.add_argument("--source", required=True)
    sweep.add_argument("--grid", required=True)
    sweep.add_argument("--db", required=True)
    sweep.add_argument("--workers", type=int, default=2)
    sweep.add_argument("--source-size", default=None)
    sweep.add_argument("--json", action="store_true")
    sweep.set_defaults(func=_cmd_sweep)

    ladder = sub.add_parser("ladder", help="build an ABR ladder from sweep results")
    ladder.add_argument("--db", required=True)
    ladder.add_argument("--rungs", type=int, default=4)
    ladder.add_argument("--max-kbps", type=float, required=True)
    ladder.add_argument("--min-ratio", type=float, default=1.3)
    ladder.add_argument("--max-ratio", type=float, default=2.2)
    ladder.add_argument("--out", required=True)
    ladder.add_argument("--json", action="store_true")
    ladder.set_defaults(func=_cmd_ladder)

    verify = sub.add_parser("verify", help="verify a generated ladder against the store")
    verify.add_argument("--db", required=True)
    verify.add_argument("--ladder", required=True)
    verify.add_argument("--max-kbps", type=float, default=None)
    verify.add_argument("--min-ratio", type=float, default=None)
    verify.add_argument("--max-ratio", type=float, default=None)
    verify.add_argument("--json", action="store_true")
    verify.set_defaults(func=_cmd_verify)

    report = sub.add_parser("report", help="dump sweep results")
    report.add_argument("--db", required=True)
    report.add_argument("--json", action="store_true")
    report.set_defaults(func=_cmd_report)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except (GridConflictError, db.DatabaseError, FileNotFoundError, ValueError) as exc:
        print(f"input error: {exc}", file=sys.stderr)
        return EXIT_INPUT_ERROR


if __name__ == "__main__":
    raise SystemExit(main())
