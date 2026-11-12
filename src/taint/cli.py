"""Command-line interface for scan and report operations."""

from __future__ import annotations

import argparse
import json
import os
import sys
import tempfile
from dataclasses import replace
from pathlib import Path
from typing import Any, Sequence

from .alias import build_alias_index
from .cache import AstCache, CacheError
from .callgraph import build_callgraph
from .paths import build_report, finalize_paths, normalize_diagnostics
from .policy import PolicyError, load_policy
from .propagate import propagate
from .resolver import build_module_graph
from .summary import build_summaries


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="taint", description="Local Python static taint scanner"
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    scan = subparsers.add_parser("scan", help="scan a Python source tree")
    scan.add_argument("--root", required=True, help="source tree to scan")
    scan.add_argument("--policy", required=True, help="YAML policy file")
    scan.add_argument("--report", help="write deterministic JSON report")
    scan.add_argument("--stats", action="store_true", help="print aggregate statistics")
    scan.add_argument(
        "--cache-dir", default=".cache", help="cache directory (default: .cache)"
    )
    scan.add_argument(
        "--jobs", type=int, default=0, help="parser worker count (0 = automatic)"
    )
    scan.add_argument(
        "--max-paths", type=int, default=None, help="override path budget"
    )

    report = subparsers.add_parser("report", help="render an existing JSON report")
    report.add_argument(
        "--input", "--report", dest="input", required=True, help="JSON report"
    )
    report.add_argument(
        "--stats", action="store_true", help="print aggregate statistics"
    )
    return parser


def _write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(
        prefix=path.name + ".", suffix=".tmp", dir=path.parent
    )
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, sort_keys=True, indent=2)
            handle.write("\n")
        os.replace(temp_name, path)
    finally:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass


def _run_scan(args: argparse.Namespace) -> int:
    try:
        policy = load_policy(args.policy)
    except PolicyError as exc:
        print(f"taint: {exc}", file=sys.stderr)
        return 2
    if args.max_paths is not None and args.max_paths <= 0:
        print("taint: --max-paths must be positive", file=sys.stderr)
        return 2
    if args.max_paths is not None:
        policy.budgets = replace(policy.budgets, max_paths=args.max_paths)

    root = Path(args.root).resolve()
    if not root.is_dir():
        print(f"taint: scan root not found: {args.root}", file=sys.stderr)
        return 2

    cache_dir = Path(args.cache_dir)
    try:
        cache = AstCache(root, cache_dir=cache_dir)
        graph = build_module_graph(root, policy, cache, jobs=args.jobs)
        aliases = build_alias_index(graph)
        callgraph = build_callgraph(graph, aliases)
        summaries = build_summaries(graph, aliases, callgraph, policy)
        propagation = propagate(graph, aliases, callgraph, policy, summaries)
    except (CacheError, OSError, ValueError) as exc:
        print(f"taint: {exc}", file=sys.stderr)
        return 3
    except Exception as exc:  # keep the CLI's documented fatal exit code
        print(f"taint: internal analysis error: {exc}", file=sys.stderr)
        return 3

    report_paths = finalize_paths(propagation.paths, policy.budgets.max_paths)
    truncated = (
        graph.truncated
        or propagation.truncated
        or summaries.truncated
        or len(propagation.paths) > len(report_paths)
    )
    diagnostics = normalize_diagnostics(
        policy.diagnostics
        + graph.diagnostics
        + callgraph.diagnostics
        + summaries.diagnostics
        + propagation.diagnostics
    )
    if len(graph.modules) == 0:
        print("taint: no parseable Python modules found", file=sys.stderr)
        return 3

    report = build_report(
        policy=policy,
        files=graph.files_discovered,
        parsed=graph.files_discovered,
        sources=propagation.sources,
        sinks=propagation.sinks,
        report_paths=report_paths,
        diagnostics=diagnostics,
        sanitized_candidates=propagation.sanitized_candidates,
        truncated=truncated,
    )
    if args.report:
        try:
            _write_json(Path(args.report), report)
        except OSError as exc:
            print(f"taint: cannot write report: {exc}", file=sys.stderr)
            return 3

    if args.stats:
        print(
            f"files={graph.files_discovered} parsed={cache.stats.parsed} "
            f"cache_hit={cache.stats.hit} diagnostics={len(diagnostics)}"
        )
        print(
            f"sources={propagation.sources} sinks={propagation.sinks} "
            f"paths={len(report_paths)} truncated={str(truncated).lower()}"
        )
    for report_path in report_paths:
        print(report_path.terminal_line())
    return 0


def _run_report(args: argparse.Namespace) -> int:
    try:
        payload = json.loads(Path(args.input).read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        print(f"taint: cannot read report: {exc}", file=sys.stderr)
        return 2
    if not isinstance(payload, dict) or not isinstance(payload.get("summary"), dict):
        print("taint: invalid report structure", file=sys.stderr)
        return 2
    summary = payload["summary"]
    if args.stats:
        print(
            f"files={summary.get('files', 0)} parsed={summary.get('parsed', 0)} "
            f"cache_hit=0 diagnostics={summary.get('diagnostics', 0)}"
        )
        print(
            f"sources={summary.get('sources', 0)} sinks={summary.get('sinks', 0)} "
            f"paths={summary.get('paths', 0)} "
            f"truncated={str(bool(summary.get('truncated', False))).lower()}"
        )
    for path in payload.get("paths", []):
        source = path.get("source", {})
        sink = path.get("sink", {})
        print(
            f"{path.get('id', '')} {source.get('file', '')}:{source.get('line', 0)} "
            f"{path.get('function_chain', '')} {sink.get('file', '')}:{sink.get('line', 0)} "
            f"{sink.get('name', '')}"
        )
    return 0


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    if args.command == "scan":
        return _run_scan(args)
    if args.command == "report":
        return _run_report(args)
    return 2


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
