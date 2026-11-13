"""Command-line interface for offline verification and timeline reconstruction."""

from __future__ import annotations

import argparse
import hashlib
import json
import sqlite3
import sys
from pathlib import Path

from .filemeta import FileMetaError, parse_filemeta
from .imageio import ImageIO, ImageError
from .report import print_report, report_from_db, report_dict, write_json
from .sqlitecarve import (
    CarveError,
    carve_sqlite,
    load_carved_database,
)
from .timebase import TimebaseError, load_maintenance_windows, TimeBase
from .timeline import TimelineError, build_timeline
from .verify import ManifestError, VerifyResult, verify_image
from .workdb import WorkStore


COMMANDS = ("verify", "timeline", "report", "run")


def _fingerprint(image: Path, manifest: Path, bucket: int) -> str:
    digest = hashlib.sha256()
    digest.update(b"image\0")
    with image.open("rb") as handle:
        while chunk := handle.read(1024 * 1024):
            digest.update(chunk)
    digest.update(b"\0manifest\0")
    with manifest.open("rb") as handle:
        while chunk := handle.read(1024 * 1024):
            digest.update(chunk)
    digest.update(b"\0bucket\0")
    digest.update(str(bucket).encode("ascii"))
    return digest.hexdigest()


def _verify_summary(result: VerifyResult) -> str:
    lines = [
        f"blocks={result.expected_blocks} manifest_blocks={result.manifest_blocks} "
        f"checked={result.checked_blocks} hash_mismatch={len(result.mismatches)} "
        f"chain_breaks={len(result.chain_breaks)}"
    ]
    for item in result.mismatches:
        lines.append(
            f"mismatch block={item.block} offset={item.offset} asset={item.asset} "
            f"logical_offset={item.logical_offset} reason={item.reason}"
        )
    for text in result.structural_errors:
        lines.append(f"verify_issue text={text}")
    lines.append(f"exit={result.exit_code}")
    return "\n".join(lines) + "\n"


def _verify_json(result: VerifyResult) -> dict:
    return {
        "blocks": result.expected_blocks,
        "manifest_blocks": result.manifest_blocks,
        "checked": result.checked_blocks,
        "hash_mismatch": len(result.mismatches),
        "hash_mismatches": [
            {
                "block": item.block,
                "offset": item.offset,
                "asset": item.asset,
                "logical_offset": item.logical_offset,
                "expected_sha256": item.expected_sha256,
                "actual_sha256": item.actual_sha256,
                "reason": item.reason,
            }
            for item in result.mismatches
        ],
        "chain_breaks": result.chain_breaks,
        "structural_errors": result.structural_errors,
        "input_errors": result.input_errors,
        "exit": result.exit_code,
    }


def _run_pipeline(
    image_path: Path,
    manifest_path: Path,
    work_path: Path,
    bucket_seconds: int,
    *,
    emit_report: bool,
) -> int:
    fingerprint = _fingerprint(image_path, manifest_path, bucket_seconds)
    with WorkStore(work_path) as store:
        if store.stage() == "complete" and store.get_meta("fingerprint") == fingerprint:
            if emit_report:
                print_report(store.report(), sys.stdout)
            return store.report().exit_code

        store.reset_for_run(fingerprint, bucket_seconds)
        stage = store.stage()
        if stage == "new":
            result = verify_image(image_path, manifest_path)
            store.store_verify(result)
            store.set_stage("verified")
            stage = "verified"

        if store.stage() == "verified":
            with ImageIO.open(image_path) as image:
                carve_result = carve_sqlite(image)
            store.store_carved(
                carve_result.rows,
                {
                    "warnings": carve_result.warnings,
                    "freelist_pages": carve_result.freelist_pages,
                    "wal_committed_frames": carve_result.wal_committed_frames,
                    "wal_discarded_frames": carve_result.wal_discarded_frames,
                    "live_rows": carve_result.live_rows,
                    "recovered_rows": carve_result.recovered_rows,
                },
            )
            store.set_stage("carved")

        if store.stage() == "carved":
            carved_rows = load_carved_database(store.path)
            with ImageIO.open(image_path) as image:
                timebase = TimeBase.from_image(image)
                filemeta = parse_filemeta(image)
                maintenance = load_maintenance_windows(image)
            timeline = build_timeline(
                carved_rows,
                filemeta,
                timebase,
                bucket_seconds=bucket_seconds,
                maintenance_windows=maintenance,
            )
            carve_warnings = json.loads(store.get_meta("carve_warnings", "[]") or "[]")
            store.store_timeline(
                timeline.events,
                timeline.missing_ranges,
                [*carve_warnings, *timeline.warnings],
            )
            store.set_stage("timeline")

        if store.stage() == "timeline":
            store.set_stage("complete")

        report = store.report()
        if emit_report:
            print_report(report, sys.stdout)
        return report.exit_code


def _timeline_summary(work_path: Path) -> str:
    with WorkStore(work_path) as store:
        stats = store.report().stats
        events = int(stats.get("event_count", "0"))
        missing = int(stats.get("missing_count", "0"))
        recovered = int(stats.get("carve_recovered_rows", "0"))
        return (
            f"events={events}\n"
            f"missing_ranges={missing}\n"
            f"recovered_rows={recovered}\n"
            f"exit={store.report().exit_code}\n"
        )


def _write_or_print_json(value: dict, destination: str | None) -> None:
    text = json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False) + "\n"
    if destination is None:
        return
    if destination == "-":
        sys.stdout.write(text)
    else:
        Path(destination).write_text(text, encoding="utf-8")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="forensictl",
        description="Offline disk-image integrity and timeline reconstruction",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    verify_parser = subparsers.add_parser("verify", help="verify block hashes and chain")
    verify_parser.add_argument("--image", required=True)
    verify_parser.add_argument("--manifest", required=True)
    verify_parser.add_argument("--json", dest="json_path")

    timeline_parser = subparsers.add_parser("timeline", help="build the merged timeline")
    timeline_parser.add_argument("--image", required=True)
    timeline_parser.add_argument("--manifest", required=True)
    timeline_parser.add_argument("--work", required=True)
    timeline_parser.add_argument("--bucket", type=int, default=300)

    report_parser = subparsers.add_parser("report", help="render a completed working database")
    report_parser.add_argument("--work", required=True)
    report_parser.add_argument("--json", dest="json_path")

    run_parser = subparsers.add_parser("run", help="verify, recover, merge and report")
    run_parser.add_argument("--image", required=True)
    run_parser.add_argument("--manifest", required=True)
    run_parser.add_argument("--work", required=True)
    run_parser.add_argument("--bucket", type=int, default=300)
    run_parser.add_argument("--json", dest="json_path")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "verify":
            result = verify_image(args.image, args.manifest)
            sys.stdout.write(_verify_summary(result))
            _write_or_print_json(_verify_json(result), args.json_path)
            return result.exit_code
        if args.command == "timeline":
            if args.bucket <= 0:
                raise TimelineError("--bucket must be positive")
            code = _run_pipeline(
                Path(args.image), Path(args.manifest), Path(args.work), args.bucket, emit_report=False
            )
            sys.stdout.write(_timeline_summary(Path(args.work)))
            return code
        if args.command == "report":
            if not Path(args.work).is_file():
                raise ValueError(f"working database does not exist: {args.work}")
            report = report_from_db(args.work)
            if args.json_path:
                text = json.dumps(report_dict(report), indent=2, sort_keys=True, ensure_ascii=False) + "\n"
                if args.json_path == "-":
                    sys.stdout.write(text)
                else:
                    Path(args.json_path).write_text(text, encoding="utf-8")
            else:
                print_report(report, sys.stdout)
            return report.exit_code
        if args.command == "run":
            if args.bucket <= 0:
                raise TimelineError("--bucket must be positive")
            code = _run_pipeline(
                Path(args.image), Path(args.manifest), Path(args.work), args.bucket, emit_report=True
            )
            if args.json_path:
                report = report_from_db(args.work)
                write_json(report, args.json_path)
            return code
    except (
        OSError,
        ManifestError,
        ImageError,
        CarveError,
        FileMetaError,
        TimebaseError,
        TimelineError,
        ValueError,
        sqlite3.Error,
    ) as exc:
        sys.stderr.write(f"error: {exc}\n")
        return 2
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
