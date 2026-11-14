"""Command-line interface for plan, scan, ladder and verify."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

from .ladder import LadderError, build_ladder, verify_ladder, write_ladder
from .plan import PlanError, generate_demo, load_plan, normalize_plan, write_json_atomic
from .report import format_ladder, format_scan, json_text, scan_summary
from .scheduler import Scheduler
from .store import Store, StoreError


EXIT_OK = 0
EXIT_INVALID = 2
EXIT_INFEASIBLE = 3
EXIT_INTERRUPTED = 4
EXIT_RETRY_EXHAUSTED = 5


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="python -m abrlab", description="Local ABR scan and ladder lab")
    parser.add_argument("--version", action="version", version="abrlab 0.1.0")
    sub = parser.add_subparsers(dest="command", required=True)

    plan = sub.add_parser("plan", help="generate or normalize a scan plan")
    plan.add_argument("--demo", action="store_true", help="generate the deterministic demo grid")
    plan.add_argument("--in", dest="input_path", help="normalize an existing plan file")
    plan.add_argument("--inject-duplicates", type=int, default=0, metavar="N")
    plan.add_argument("--out", required=True)
    plan.add_argument("--json", action="store_true")

    scan = sub.add_parser("scan", help="run or resume a scan")
    scan.add_argument("--plan")
    scan.add_argument("--resume", action="store_true")
    scan.add_argument("--db", required=True)
    scan.add_argument("--stop-after", type=int, metavar="N")
    scan.add_argument("--json", action="store_true")

    ladder = sub.add_parser("ladder", help="build a measured-point ladder")
    ladder.add_argument("--db", required=True)
    ladder.add_argument("--out", required=True)
    ladder.add_argument("--max-rungs", type=int, default=4)
    ladder.add_argument("--max-bitrate", type=float, default=1800.0)
    ladder.add_argument("--min-quality", type=float, default=0.86)
    ladder.add_argument("--json", action="store_true")

    verify = sub.add_parser("verify", help="recompute and verify a ladder")
    verify.add_argument("--db", required=True)
    verify.add_argument("--ladder", required=True)
    verify.add_argument("--json", action="store_true")
    return parser


def _error(message: str, code: int, *, as_json: bool) -> int:
    if as_json:
        print(json_text({"error": message, "exit_code": code}, compact=True), file=sys.stderr)
    else:
        print(f"error: {message}", file=sys.stderr)
    return code


def _cmd_plan(args: argparse.Namespace) -> int:
    if bool(args.demo) == bool(args.input_path):
        return _error("plan requires exactly one of --demo or --in", EXIT_INVALID, as_json=args.json)
    if args.input_path and args.inject_duplicates:
        return _error("--inject-duplicates is only valid with --demo", EXIT_INVALID, as_json=args.json)
    if args.input_path:
        plan, _ = load_plan(args.input_path)
        raw_count = len(plan["entries"])
    else:
        plan = generate_demo(args.inject_duplicates)
        raw_count = int(plan["stats"]["entries"])
    unique_count = len(plan["entries"])
    write_json_atomic(args.out, plan)
    print(json_text({"entries": raw_count, "unique": unique_count}, compact=True))
    return EXIT_OK


def _cmd_scan(args: argparse.Namespace) -> int:
    if bool(args.plan) == bool(args.resume):
        return _error("scan requires exactly one of --plan or --resume", EXIT_INVALID, as_json=args.json)
    if args.stop_after is not None and args.stop_after < 0:
        return _error("--stop-after must not be negative", EXIT_INVALID, as_json=args.json)
    store = Store(args.db)
    try:
        if args.plan:
            plan, _ = load_plan(args.plan)
            store.add_plan_entries(plan["entries"])
        stats = Scheduler(store).run(stop_after=args.stop_after).as_dict()
    finally:
        store.close()
    if args.json:
        print(json_text(scan_summary(stats), compact=True))
    else:
        print(format_scan(stats))
    if stats["interrupted"]:
        return EXIT_INTERRUPTED
    if int(stats["failed"]) > 0:
        return EXIT_RETRY_EXHAUSTED
    return EXIT_OK


def _cmd_ladder(args: argparse.Namespace) -> int:
    with Store(args.db) as store:
        ladder = build_ladder(
            store.result_rows(),
            max_rungs=args.max_rungs,
            max_bitrate=args.max_bitrate,
            min_quality=args.min_quality,
        )
    write_ladder(args.out, ladder)
    if args.json:
        print(json_text(ladder, compact=True))
    else:
        print(format_ladder(ladder))
    return EXIT_OK


def _load_ladder(path: str) -> dict[str, Any]:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            value = json.load(handle)
    except OSError as exc:
        raise LadderError(f"cannot read ladder: {exc}") from exc
    except json.JSONDecodeError as exc:
        raise LadderError(f"invalid ladder JSON: {exc}") from exc
    if not isinstance(value, dict):
        raise LadderError("ladder must be a JSON object")
    return value


def _cmd_verify(args: argparse.Namespace) -> int:
    ladder = _load_ladder(args.ladder)
    with Store(args.db) as store:
        result = verify_ladder(store, ladder)
    print(json_text(result, compact=True))
    return EXIT_OK if result["pass"] else EXIT_INFEASIBLE


def main(argv: list[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    as_json = bool(getattr(args, "json", False))
    try:
        if args.command == "plan":
            return _cmd_plan(args)
        if args.command == "scan":
            return _cmd_scan(args)
        if args.command == "ladder":
            return _cmd_ladder(args)
        if args.command == "verify":
            return _cmd_verify(args)
        return _error("unknown command", EXIT_INVALID, as_json=as_json)
    except PlanError as exc:
        return _error(str(exc), EXIT_INVALID, as_json=as_json)
    except LadderError as exc:
        return _error(str(exc), EXIT_INFEASIBLE, as_json=as_json)
    except StoreError as exc:
        return _error(str(exc), EXIT_INVALID, as_json=as_json)
    except KeyboardInterrupt:
        return _error("interrupted; rerun scan with --resume", EXIT_INTERRUPTED, as_json=as_json)


if __name__ == "__main__":
    raise SystemExit(main())
