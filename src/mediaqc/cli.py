"""Command line interface for probing, batch QC, querying and packaging."""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import sqlite3
import sys
from dataclasses import replace
from pathlib import Path
from typing import Any

from .bmff import BoxReader
from .model import SEVERITY_RANK, Finding, ProbeResult, finding_counts
from .packager import PackError, pack_file
from .riff import probe_riff
from .rules import evaluate_rules
from .sniff import SniffError, sniff_file
from .store import Database, StoreError
from .tracks import parse_mp4_tracks


def _unsupported(path: str, file_size: int) -> ProbeResult:
    result = ProbeResult(path, "unsupported", file_size, status="unsupported",
                         error="unsupported_container")
    result.add_finding(Finding("UNSUPPORTED_CONTAINER", "error",
                               "container is not mp4/mov/avi/wav",
                               "magic and top-level boxes are unrecognized", offset=0))
    return result


def _io_error(path: str, exc: Exception) -> ProbeResult:
    result = ProbeResult(path, "unknown", 0, status="io_error", error=str(exc))
    result.add_finding(Finding("IO_ERROR", "error", "file could not be read", str(exc), offset=0))
    return result


def probe_path(path: str | Path, compact: bool = False) -> ProbeResult:
    display = str(path)
    try:
        file_size = Path(path).stat().st_size
    except OSError as exc:
        return _io_error(display, exc)
    try:
        sniff = sniff_file(path)
    except SniffError as exc:
        if exc.code == "unsupported_container":
            return _unsupported(display, file_size)
        return _io_error(display, exc)
    try:
        if sniff.container in {"mp4", "mov"}:
            scan = BoxReader(path).scan()
            result = ProbeResult(display, sniff.container, file_size,
                                 findings=list(scan.findings),
                                 mdat_ranges=list(scan.mdat_ranges))
            result.metadata.update({
                "brand": sniff.brand,
                "sniff_reason": sniff.reason,
                "unsupported_layout": scan.unsupported_layout,
                "trailing_bytes": scan.trailing_bytes,
            })
            with Path(path).open("rb") as handle:
                tracks, parse_findings, movie_ts = parse_mp4_tracks(handle, scan)
            result.tracks = tracks
            result.findings.extend(parse_findings)
            result.movie_timescale = movie_ts
            if scan.has_moof:
                result.status = "unsupported_layout"
        else:
            result = probe_riff(path, sniff.container)
            result.metadata["sniff_reason"] = sniff.reason
        evaluate_rules(result)
        _sort_findings(result)
        return _compact(result) if compact else result
    except Exception as exc:
        result = ProbeResult(display, sniff.container, file_size, status="parse_error",
                             error=str(exc))
        result.add_finding(Finding("FILE_PARSE_ERROR", "error", "file parsing failed",
                                   f"{type(exc).__name__}: {exc}", offset=0))
        return result


def _sort_findings(result: ProbeResult) -> None:
    result.findings.sort(key=lambda item: (
        SEVERITY_RANK[item.severity], item.rule_id, item.track_index if item.track_index is not None else -1,
        item.offset if item.offset is not None else -1, item.evidence,
    ))


def _compact(result: ProbeResult) -> ProbeResult:
    tracks = [replace(track, samples=[], chunk_ranges=[], sync_samples=[])
              for track in result.tracks]
    return replace(result, tracks=tracks)


def _batch_worker(path: str) -> ProbeResult:
    try:
        return probe_path(path, compact=True)
    except Exception as exc:
        return _io_error(path, exc)


def _number(value: float | None) -> str:
    if value is None:
        return "null"
    rounded = round(value, 3)
    return str(int(rounded)) if rounded == int(rounded) else f"{rounded:.3f}".rstrip("0").rstrip(".")


def _print_probe(result: ProbeResult) -> None:
    print(f"path={result.path}")
    print(f"container={result.container}")
    print(f"tracks={len(result.tracks)}")
    print(f"video_duration_ms={_number(result.track_duration('video'))}")
    print(f"audio_duration_ms={_number(result.track_duration('audio'))}")
    print(f"drift_ms={_number(result.drift_ms)}")
    print(f"findings={len(result.findings)}")
    for track in result.tracks:
        print(f"track[{track.index}]={track.kind} codec={track.codec} "
              f"duration_ms={_number(track.presentation_duration_ms)} "
              f"samples={track.sample_count}")
    for finding in result.findings:
        print(f"finding={finding.rule_id} severity={finding.severity} "
              f"track={finding.track_index if finding.track_index is not None else '-'} "
              f"offset={finding.offset if finding.offset is not None else '-'} "
              f"evidence={finding.evidence}")
    if result.error:
        print(f"error={result.error}")


def _write_json(path: str | Path, payload: dict[str, Any]) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temporary = target.parent / f".{target.name}.tmp"
    temporary.write_text(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
                         encoding="utf-8")
    os.replace(temporary, target)


def _exit_for_result(result: ProbeResult) -> int:
    if result.status == "unsupported" or result.container == "unsupported":
        return 3
    if result.status == "io_error":
        return 2
    return 1 if any(item.severity == "error" for item in result.findings) else 0


def command_probe(args) -> int:
    result = probe_path(args.path)
    _print_probe(result)
    if args.json_out:
        try:
            _write_json(args.json_out, result.public_dict())
        except OSError as exc:
            print(f"error=json_out_failed: {exc}", file=sys.stderr)
            return 2
    return _exit_for_result(result)


def command_batch(args) -> int:
    root = Path(args.directory)
    if not root.is_dir():
        print(f"error=not_a_directory: {root}", file=sys.stderr)
        return 2
    paths = sorted((item for item in root.rglob("*") if item.is_file()),
                   key=lambda item: item.as_posix())
    jobs = max(1, args.jobs)
    results: list[ProbeResult | None] = [None] * len(paths)
    try:
        with concurrent.futures.ProcessPoolExecutor(max_workers=jobs) as executor:
            futures = {executor.submit(_batch_worker, str(path)): index
                       for index, path in enumerate(paths)}
            for future in concurrent.futures.as_completed(futures):
                index = futures[future]
                try:
                    results[index] = future.result()
                except Exception as exc:
                    results[index] = _io_error(str(paths[index]), exc)
    except (OSError, RuntimeError) as exc:
        print(f"error=batch_worker_failed: {exc}", file=sys.stderr)
        return 2
    completed = [item for item in results if item is not None]
    try:
        database = Database(args.db)
        with database.writer() as writer:
            for result in completed:
                writer.write(result)
    except (StoreError, sqlite3.Error, OSError) as exc:
        print(f"error=database_write_failed: {exc}", file=sys.stderr)
        return 2
    severity_counts = {severity: 0 for severity in ("error", "warning", "info")}
    total_tracks = 0
    for result in completed:
        total_tracks += len(result.tracks)
        for severity, count in finding_counts(result.findings).items():
            severity_counts[severity] += count
    report = {
        "directory": str(root),
        "files": len(completed),
        "tracks_total": total_tracks,
        "findings_total": sum(severity_counts.values()),
        "findings_by_severity": severity_counts,
        "jobs": jobs,
        "results": [{
            "path": result.path, "container": result.container, "status": result.status,
            "tracks": len(result.tracks), "video_duration_ms": result.track_duration("video"),
            "audio_duration_ms": result.track_duration("audio"), "drift_ms": result.drift_ms,
            "findings_by_severity": finding_counts(result.findings), "error": result.error,
        } for result in completed],
    }
    print(f"files={report['files']}")
    print(f"tracks_total={report['tracks_total']}")
    print(f"findings_total={report['findings_total']}")
    print("findings_by_severity=" + ",".join(
        f"{severity}:{severity_counts[severity]}" for severity in ("error", "warning", "info")))
    if args.json_out:
        try:
            _write_json(args.json_out, report)
        except OSError as exc:
            print(f"error=json_out_failed: {exc}", file=sys.stderr)
            return 2
    return 1 if severity_counts["error"] else 0


def _open_database(path: str) -> Database | None:
    if not Path(path).exists():
        print(f"error=database_not_found: {path}", file=sys.stderr)
        return None
    try:
        return Database(path)
    except (sqlite3.Error, OSError) as exc:
        print(f"error=database_open_failed: {exc}", file=sys.stderr)
        return None


def command_check(args) -> int:
    database = _open_database(args.db)
    if database is None:
        return 2
    try:
        report = database.findings(args.min_severity)
    except (StoreError, sqlite3.Error) as exc:
        print(f"error=database_query_failed: {exc}", file=sys.stderr)
        return 2
    print(f"findings={report['total']}")
    print("findings_by_severity=" + ",".join(
        f"{severity}:{report['by_severity'][severity]}"
        for severity in ("error", "warning", "info")))
    print(f"rules={len(report['by_rule'])}")
    for rule_id, count in report["by_rule"].items():
        print(f"{rule_id}={count}")
    if args.json_out:
        try:
            _write_json(args.json_out, report)
        except OSError as exc:
            print(f"error=json_out_failed: {exc}", file=sys.stderr)
            return 2
    return 1 if report["by_severity"].get("error", 0) else 0


def command_report(args) -> int:
    database = _open_database(args.db)
    if database is None:
        return 2
    try:
        summary = database.summary()
        report = database.findings(args.min_severity)
    except (StoreError, sqlite3.Error) as exc:
        print(f"error=database_query_failed: {exc}", file=sys.stderr)
        return 2
    print(f"files={summary['files']}")
    print(f"tracks_total={summary['tracks_total']}")
    print(f"findings_total={report['total']}")
    print("findings_by_severity=" + ",".join(
        f"{severity}:{report['by_severity'][severity]}"
        for severity in ("error", "warning", "info")))
    for rule_id, count in report["by_rule"].items():
        print(f"{rule_id}={count}")
    payload = {"summary": summary, "findings": report}
    if args.json_out:
        try:
            _write_json(args.json_out, payload)
        except OSError as exc:
            print(f"error=json_out_failed: {exc}", file=sys.stderr)
            return 2
    return 1 if report["by_severity"].get("error", 0) else 0


def command_pack(args) -> int:
    try:
        manifest = pack_file(args.path, args.target_ms, args.out_dir)
    except PackError as exc:
        print(f"error={exc}")
        for item in exc.details:
            print(f"out_of_range=track:{item['track_index']} sample:{item['sample_index']} "
                  f"offset:{item['offset']} end:{item['end']} reason:{item['reason']}")
        if args.json_out:
            try:
                _write_json(args.json_out, {"error": str(exc), "kind": exc.kind,
                                             "out_of_range": exc.details})
            except OSError as json_exc:
                print(f"error=json_out_failed: {json_exc}", file=sys.stderr)
                return 2
        if exc.kind == "unsupported":
            return 3
        if exc.kind == "argument":
            return 2
        return 1
    except OSError as exc:
        print(f"error={exc}", file=sys.stderr)
        return 2
    print(f"segments={manifest['segments']}")
    print(f"max_duration_deviation_ms={_number(manifest['max_duration_deviation_ms'])}")
    print(f"manifest={Path(args.out_dir) / 'manifest.json'}")
    if args.json_out:
        try:
            _write_json(args.json_out, manifest)
        except OSError as exc:
            print(f"error=json_out_failed: {exc}", file=sys.stderr)
            return 2
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mediaqc", description="Byte-level media QC")
    commands = parser.add_subparsers(dest="command", required=True)
    probe = commands.add_parser("probe", help="inspect one media file")
    probe.add_argument("path")
    probe.add_argument("--json-out")
    probe.set_defaults(handler=command_probe)
    batch = commands.add_parser("batch", help="recursively inspect a directory")
    batch.add_argument("directory")
    batch.add_argument("--db", required=True)
    batch.add_argument("--jobs", type=int, default=4)
    batch.add_argument("--json-out")
    batch.set_defaults(handler=command_batch)
    check = commands.add_parser("check", help="query findings from the database")
    check.add_argument("--db", required=True)
    check.add_argument("--min-severity", choices=("error", "warning", "info"), default="warning")
    check.add_argument("--json-out")
    check.set_defaults(handler=command_check)
    report = commands.add_parser("report", help="show database and rule aggregates")
    report.add_argument("--db", required=True)
    report.add_argument("--min-severity", choices=("error", "warning", "info"), default="info")
    report.add_argument("--json-out")
    report.set_defaults(handler=command_report)
    pack = commands.add_parser("pack", help="build sync-sample segment manifests")
    pack.add_argument("path")
    pack.add_argument("--target-ms", required=True, type=int)
    pack.add_argument("--out-dir", required=True)
    pack.add_argument("--json-out")
    pack.set_defaults(handler=command_pack)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.handler(args))
    except BrokenPipeError:
        return 0
    except KeyboardInterrupt:
        print("error=interrupted", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
