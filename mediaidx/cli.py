"""Command-line interface for indexing, inspection and database statistics."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
from typing import Any

import numpy as np

from mediaidx.fingerprint import fingerprint_loaded
from mediaidx.indexer import Indexer, terminal_summary, write_report
from mediaidx.loaders import REGISTRY, DecodeFailure
from mediaidx.store import Store


def _jsonable(value: Any) -> Any:
    if isinstance(value, np.ndarray):
        if value.ndim == 0:
            return value.item()
        flat = value.reshape(-1)
        return {
            "dtype": str(value.dtype),
            "shape": list(value.shape),
            "head": [float(item) for item in flat[:16]],
        }
    if isinstance(value, np.generic):
        return value.item()
    if isinstance(value, dict):
        return {str(key): _jsonable(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_jsonable(item) for item in value]
    return value


def _ensure_outputs_outside_corpus(root: Path, paths: list[Path | None]) -> None:
    resolved_root = root.resolve(strict=True)
    for path in paths:
        if path is None:
            continue
        try:
            resolved = path.expanduser().resolve()
        except OSError:
            resolved = path.absolute()
        if resolved == resolved_root or resolved_root in resolved.parents:
            raise SystemExit(f"输出路径不得位于只读语料目录内: {path}")


def command_index(args: argparse.Namespace) -> int:
    root = Path(args.root).expanduser()
    db_path = Path(args.db).expanduser()
    report_path = Path(args.report).expanduser() if args.report else None
    _ensure_outputs_outside_corpus(root, [db_path, report_path])
    indexer = Indexer(root, db_path, jobs=args.jobs, labels_path=args.labels)
    report = indexer.run()
    if report_path is not None:
        write_report(report, report_path)
    print(terminal_summary(report))
    if args.verbose:
        print(
            "解码调用="
            f"{report['index']['decode_calls']} "
            "解码缓存命中="
            f"{report['index']['decode_cache_hits']} "
            "未变复用="
            f"{report['index']['unchanged_files']} "
            f"耗时={report['elapsed_seconds']:.3f}s"
        )
    return 0


def command_inspect(args: argparse.Namespace) -> int:
    loaded = REGISTRY.decode_path(args.path)
    if isinstance(loaded, DecodeFailure):
        print(json.dumps(loaded.as_dict(), ensure_ascii=False, indent=2))
        return 2
    fingerprint = fingerprint_loaded(loaded)
    output = {
        "family": loaded.kind.family.value,
        "format": loaded.kind.format,
        "loader": loaded.loader_name,
        "loader_version": loaded.loader_version,
        "metadata": _jsonable(loaded.metadata),
        "candidate_hashes": fingerprint.get("candidate_hashes", []),
        "fingerprint_version": fingerprint.get("version"),
    }
    for key in (
        "phash",
        "dhash",
        "profile_hash",
        "temporal_hash",
        "sequence_hash",
        "duration",
        "anchor_indices",
    ):
        if key in fingerprint:
            output[key] = _jsonable(fingerprint[key])
    print(json.dumps(output, ensure_ascii=False, indent=2))
    return 0


def command_stats(args: argparse.Namespace) -> int:
    with Store(args.db) as store:
        counts = store.counts()
        candidates = store.candidate_pairs()
        verified = store.verified_pairs()
        clusters = store.clusters()
        duplicate = sum(1 for cluster in clusters if int(cluster["member_count"]) > 1)
        print(
            f"素材总数={counts['total']} "
            f"解码成功={counts['success']} "
            f"解码失败={counts['failed']} "
            f"重复簇数={duplicate} "
            f"去重后独立素材={len(clusters)} "
            f"候选对={len(candidates)} "
            f"精验通过={len(verified)} "
            f"否决={len(candidates) - len(verified)}"
        )
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m mediaidx.cli",
        description="Local content-perceptual media deduplicator",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    index_parser = subparsers.add_parser("index", help="scan a corpus and build clusters")
    index_parser.add_argument("--root", required=True, help="read-only corpus directory")
    index_parser.add_argument("--db", required=True, help="SQLite database path (outside corpus)")
    index_parser.add_argument("--report", help="JSON report path (outside corpus)")
    index_parser.add_argument(
        "--jobs", type=int, default=max(1, min(4, os.cpu_count() or 1)), help="decode worker count"
    )
    index_parser.add_argument("--labels", help="optional ground-truth manifest for conflict reporting")
    index_parser.add_argument("--verbose", action="store_true", help="print decode/cache counters")
    index_parser.set_defaults(func=command_index)

    inspect_parser = subparsers.add_parser("inspect", help="decode one media file and print fingerprints")
    inspect_parser.add_argument("path")
    inspect_parser.set_defaults(func=command_inspect)

    stats_parser = subparsers.add_parser("stats", help="print counters from an existing database")
    stats_parser.add_argument("--db", required=True)
    stats_parser.set_defaults(func=command_stats)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if getattr(args, "jobs", 1) < 1:
        parser.error("--jobs must be >= 1")
    try:
        return int(args.func(args))
    except (KeyboardInterrupt, BrokenPipeError):
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
