"""Terminal-only command entry points for the offscreen timeline pipeline."""

from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import json
import os
import sys
import tempfile
import time
from fractions import Fraction
from pathlib import Path
from typing import Any, Mapping

from exchange_v1 import V1Error, write_json_atomic
from exchange_v2 import V2Error
from fixtures import generate_all
from ledger import LedgerError, aggregate, initialize as init_ledger, record_run
from model import ModelError, parse_rational, rational_json
from normalize import NormalizeError, load_exchange, normalize_v1, timeline_from_report
from otio_bridge import OtioBridgeError, export_otio
from overlap import ResolutionError, build_resolution_report, resolve_timeline
from renderer import hashes_match, render_timeline

VERSION = "1.0"


class CliError(ValueError):
    def __init__(self, code: str, message: str, *, exit_code: int = 2, **details: Any) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.exit_code = exit_code
        self.details = details


def safe_path(value: str | Path, *, must_exist: bool = False) -> Path:
    root = Path.cwd().resolve()
    candidate = Path(value)
    if not candidate.is_absolute():
        candidate = root / candidate
    resolved = candidate.resolve(strict=False)
    try:
        resolved.relative_to(root)
    except ValueError as exc:
        raise CliError("path_escape", f"path escapes working directory: {value}", exit_code=2) from exc
    if must_exist and not resolved.exists():
        raise CliError("path_not_found", f"path does not exist: {value}", exit_code=1)
    return resolved


def _jsonable(value: Any) -> Any:
    if isinstance(value, Fraction):
        return rational_json(value)
    if isinstance(value, Path):
        return value.name
    if isinstance(value, Mapping):
        return {str(key): _jsonable(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_jsonable(item) for item in value]
    return value


def _compact(payload: dict[str, Any]) -> str:
    return json.dumps(_jsonable(payload), ensure_ascii=False, separators=(",", ":"), sort_keys=True)


def emit(payload: dict[str, Any], out: str | Path | None = None, *, ndjson: bool = False) -> None:
    text = _compact(payload)
    if out is not None:
        write_json_atomic(Path(out), _jsonable(payload))
    print(text)


def _safe_out(value: str | None) -> Path | None:
    return safe_path(value) if value else None


def _failure_payload(path: Path, exc: Exception) -> dict[str, Any]:
    code = getattr(exc, "code", exc.__class__.__name__.lower())
    message = getattr(exc, "message", str(exc))
    details = getattr(exc, "details", {})
    return {
        "status": "failed", "project": path.name.removesuffix(".v2.json").removesuffix(".v1.json"),
        "code": code, "reason": code, "message": message, "details": _jsonable(details),
        "quarantine": [{"project": path.name, "code": code, "reason": code}],
        "exit_code": int(getattr(exc, "exit_code", 1)),
    }


def _sibling_v1(path: Path) -> Path | None:
    if path.name.endswith(".v2.json"):
        sibling = path.with_name(path.name[:-8] + ".v1.json")
        if sibling.exists():
            return safe_path(sibling, must_exist=True)
    return None


def resolve_project(path: str | Path, *, rate: Fraction | None = None,
                    master_frames: int | None = None, policy: str | None = None,
                    compare_v1: bool = True) -> tuple[dict[str, Any], Any, dict[str, Any]]:
    path = Path(path)
    timeline = load_exchange(path, master_rate=rate, master_frames=master_frames, policy=policy)
    selected_policy = str(policy or timeline.policy)
    resolved, scan, actions = resolve_timeline(timeline, selected_policy)
    rendered = render_timeline(resolved)
    format_hashes = {"v2": rendered["canonical_frame_sha256"]}
    cross_checked = False
    cross_converged = True
    comparison_payload: dict[str, Any] = {}
    sibling = _sibling_v1(path) if compare_v1 and path.name.endswith(".v2.json") else None
    if sibling is not None:
        cross_checked = True
        try:
            first = normalize_v1(sibling, master_rate=timeline.rate,
                                 master_frames=timeline.master_frames, policy=selected_policy)
            first_resolved, _, _ = resolve_timeline(first, selected_policy)
            first_render = render_timeline(first_resolved)
            cross_converged = hashes_match(rendered, first_render)
            format_hashes["v1"] = first_render["canonical_frame_sha256"]
            comparison_payload = {"v1_resolved_interval_count": len(first_resolved.content_clips()),
                                  "v1_action_count": len(first_resolved.all_clips())}
        except (V1Error, V2Error, NormalizeError, ResolutionError, ModelError) as exc:
            cross_converged = False
            comparison_payload = {"v1_error": getattr(exc, "code", "v1_compare_failed")}
    losses = [dict(item) for item in timeline.precision_losses]
    report = build_resolution_report(
        timeline, resolved, scan, actions, policy=selected_policy,
        cross_format_hash_converged=cross_converged, cross_format_checked=cross_checked,
        format_frame_sha256=format_hashes, precision_losses=losses,
    )
    report.update({
        "pipeline_version": VERSION,
        "resolved_frame_sha256": rendered["canonical_frame_sha256"],
        "resolved_per_track_frame_sha256": rendered["per_track_frame_sha256"],
        "source_coverage": rendered["source_coverage"],
        "uncovered_frames": rendered["uncovered_frames"],
        **comparison_payload,
    })
    report["exit_code"] = 3 if report["over_limit"] or not cross_converged else 0
    report["status"] = "resolved" if report["exit_code"] == 0 else "deviation"
    return report, resolved, rendered


def cmd_resolve(args: argparse.Namespace) -> int:
    out = _safe_out(args.out)
    path = safe_path(args.v2, must_exist=True)
    try:
        try:
            rate = parse_rational(args.rate, field_name="rate") if args.rate else None
        except ModelError as exc:
            raise CliError("invalid_rate", exc.message, exit_code=2) from exc
        if args.master_frames is not None and args.master_frames < 0:
            raise CliError("invalid_master_frames", "master frames must be non-negative")
        report, _, _ = resolve_project(path, rate=rate, master_frames=args.master_frames,
                                       policy=args.policy)
        emit(report, out, ndjson=args.ndjson)
        return int(report["exit_code"])
    except (CliError, V1Error, V2Error, NormalizeError, ResolutionError, ModelError, LedgerError) as exc:
        payload = _failure_payload(path, exc)
        emit(payload, out, ndjson=args.ndjson)
        return int(payload["exit_code"])


def _write_hashes(path: Path, rendered: dict[str, Any]) -> None:
    lines = [f"{digest}  {track}" for track, digest in sorted(rendered["per_track_frame_sha256"].items())]
    lines.append(f"{rendered['canonical_frame_sha256']}  canonical")
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write("\n".join(lines) + "\n")
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def cmd_render(args: argparse.Namespace) -> int:
    report_path = safe_path(args.report, must_exist=True)
    hash_out = safe_path(args.hash_out) if args.hash_out else None
    try:
        report = json.loads(report_path.read_text(encoding="utf-8"))
        timeline = timeline_from_report(report)
        rendered = render_timeline(timeline)
        expected_canonical = report.get("resolved_frame_sha256")
        expected_tracks = report.get("resolved_per_track_frame_sha256")
        match = (expected_canonical in {None, rendered["canonical_frame_sha256"]} and
                 (expected_tracks is None or expected_tracks == rendered["per_track_frame_sha256"]))
        converged = bool(report.get("cross_format_hash_converged", False) and match)
        payload = {
            "status": "rendered" if converged else "hash_mismatch",
            **rendered, "cross_format_hash_converged": converged,
            "expected_hash_match": match,
            "exit_code": 0 if converged else 3,
        }
        if hash_out is not None:
            _write_hashes(hash_out, rendered)
        emit(payload, ndjson=args.ndjson)
        return int(payload["exit_code"])
    except (OSError, UnicodeError, json.JSONDecodeError, NormalizeError, ModelError) as exc:
        payload = _failure_payload(report_path, exc)
        emit(payload, ndjson=args.ndjson)
        return int(payload["exit_code"])


def cmd_selftest(args: argparse.Namespace) -> int:
    directory = safe_path(args.out)
    try:
        if args.clips < 1 or args.jobs < 1:
            raise CliError("invalid_arguments", "clips and jobs must be positive")
        summary = generate_all(directory, seed=args.seed, clips=args.clips)
        summary["pipeline_version"] = VERSION
        summary["jobs"] = args.jobs
        emit(summary)
        return 0
    except (CliError, OSError, ValueError) as exc:
        emit(_failure_payload(directory, exc))
        return int(getattr(exc, "exit_code", 1))


def cmd_otio_export(args: argparse.Namespace) -> int:
    report_path = safe_path(args.report, must_exist=True)
    out = safe_path(args.out)
    try:
        report = json.loads(report_path.read_text(encoding="utf-8"))
        timeline = timeline_from_report(report)
        summary = export_otio(timeline, out, verify=args.verify)
        summary["report"] = report_path.name
        emit(summary)
        return int(summary["exit_code"])
    except (OSError, UnicodeError, json.JSONDecodeError, NormalizeError, ModelError,
            OtioBridgeError) as exc:
        payload = _failure_payload(report_path, exc)
        emit(payload)
        return int(payload["exit_code"])


def _content_hash(path: Path) -> str:
    digest = hashlib.sha256(path.read_bytes())
    sibling = _sibling_v1(path)
    if sibling is not None:
        digest.update(sibling.read_bytes())
    return digest.hexdigest()


def _batch_worker(task: tuple[str, str]) -> dict[str, Any]:
    path_text, db_text = task
    path, db_path = Path(path_text), Path(db_text)
    project = path.name.removesuffix(".v2.json")
    try:
        content_hash = _content_hash(path)
        report, _, _ = resolve_project(path)
        reason = None
        if report["over_limit"]:
            reason = "duration_limit_exceeded"
        elif not report["cross_format_hash_converged"]:
            reason = "cross_format_hash_not_converged"
        status = "success" if report["exit_code"] == 0 and reason is None else "failed"
        result = {
            "project": project, "status": status, "exit_code": report["exit_code"],
            "reason": reason, "conflict_count": report["conflict_count"],
            "resolved_interval_count": report["resolved_interval_count"],
            "resolved_frames": report["resolved_frames"],
        }
        record_run(db_path, content_hash, project, status, report["exit_code"],
                   _compact(result), reason)
        return result
    except Exception as exc:  # Every project failure is isolated and ledgered.
        code = getattr(exc, "code", exc.__class__.__name__.lower())
        result = {
            "project": project, "status": "failed",
            "exit_code": int(getattr(exc, "exit_code", 1)), "reason": code,
            "error": getattr(exc, "message", str(exc)),
        }
        try:
            record_run(db_path, _content_hash(path), project, "failed",
                       int(getattr(exc, "exit_code", 1)), _compact(result), code)
        except Exception:
            result["ledger_write_failed"] = True
        return result


def cmd_batch(args: argparse.Namespace) -> int:
    directory = safe_path(args.dir, must_exist=True)
    out = _safe_out(args.out)
    if not directory.is_dir():
        emit(_failure_payload(directory, CliError("not_a_directory", "batch path is not a directory")))
        return 2
    if args.jobs < 1:
        emit(_failure_payload(directory, CliError("invalid_arguments", "jobs must be positive")))
        return 2
    paths: list[Path] = []
    rejected: list[dict[str, Any]] = []
    for candidate in sorted(directory.glob("*.v2.json")):
        try:
            paths.append(safe_path(candidate, must_exist=True))
        except CliError as exc:
            rejected.append({
                "project": candidate.name.removesuffix(".v2.json"), "status": "failed",
                "exit_code": exc.exit_code, "reason": exc.code,
            })
    try:
        db_path = safe_path(directory / "ledger.sqlite3")
        init_ledger(db_path)
    except (LedgerError, CliError) as exc:
        emit(_failure_payload(directory, exc))
        return int(getattr(exc, "exit_code", 1))
    tasks = [(str(path), str(db_path)) for path in paths]
    started = time.perf_counter()
    results: list[dict[str, Any]] = list(rejected)
    if args.jobs == 1:
        results = [_batch_worker(task) for task in tasks]
    else:
        with concurrent.futures.ProcessPoolExecutor(max_workers=args.jobs) as pool:
            for result in pool.map(_batch_worker, tasks, chunksize=1):
                results.append(result)
                if args.ndjson:
                    print(_compact(result))
    success = sum(result["status"] == "success" for result in results)
    failed = len(results) - success
    reasons: dict[str, int] = {}
    quarantine = []
    for result in results:
        if result["status"] == "success":
            continue
        reason = str(result.get("reason") or "unknown_failure")
        reasons[reason] = reasons.get(reason, 0) + 1
        quarantine.append({"project": result["project"], "reason": reason,
                           "exit_code": result["exit_code"]})
    try:
        ledger_summary = aggregate(db_path)
    except LedgerError as exc:
        ledger_summary = {"error": exc.code}
    payload = {
        "status": "completed" if success else "failed", "project_count": len(paths),
        "success": success, "failed": failed, "quarantine_reasons": dict(sorted(reasons.items())),
        "quarantine": quarantine, "ledger": ledger_summary,
        "elapsed_seconds": round(time.perf_counter() - started, 6), "jobs": args.jobs,
        "exit_code": 0 if success else 1,
    }
    emit(payload, out, ndjson=args.ndjson)
    return int(payload["exit_code"])


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="cli.py", description="Offscreen timeline reconciliation")
    subparsers = parser.add_subparsers(dest="command", required=True)
    selftest = subparsers.add_parser("selftest", help="generate paired v1/v2 fixtures")
    selftest.add_argument("--out", required=True)
    selftest.add_argument("--seed", type=int, default=20261115)
    selftest.add_argument("--clips", type=int, default=240)
    selftest.add_argument("--jobs", type=int, default=4)
    selftest.set_defaults(func=cmd_selftest)
    resolve = subparsers.add_parser("resolve", help="resolve a v2 project")
    resolve.add_argument("--v2", required=True)
    resolve.add_argument("--rate")
    resolve.add_argument("--master-frames", type=int)
    resolve.add_argument("--policy", choices=["trim-left", "trim-right", "split", "upper-covers",
                                              "protect-locked", "reject"])
    resolve.add_argument("--out")
    resolve.add_argument("--ndjson", action="store_true")
    resolve.set_defaults(func=cmd_resolve)
    render = subparsers.add_parser("render", help="render and hash a resolved report")
    render.add_argument("--report", required=True)
    render.add_argument("--hash-out")
    render.add_argument("--ndjson", action="store_true")
    render.set_defaults(func=cmd_render)
    otio_export = subparsers.add_parser("otio-export", help="export and verify OTIO")
    otio_export.add_argument("--report", required=True)
    otio_export.add_argument("--out", required=True)
    otio_export.add_argument("--verify", action="store_true")
    otio_export.set_defaults(func=cmd_otio_export)
    batch = subparsers.add_parser("batch", help="process all v2 projects in a directory")
    batch.add_argument("--dir", required=True)
    batch.add_argument("--jobs", type=int, default=4)
    batch.add_argument("--out")
    batch.add_argument("--ndjson", action="store_true")
    batch.set_defaults(func=cmd_batch)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except CliError as exc:
        emit({"status": "failed", "code": exc.code, "reason": exc.code,
              "message": exc.message, "exit_code": exc.exit_code})
        return exc.exit_code
    except Exception as exc:
        emit({"status": "failed", "code": exc.__class__.__name__.lower(),
              "reason": getattr(exc, "code", "internal_error"), "message": str(exc), "exit_code": 1})
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
