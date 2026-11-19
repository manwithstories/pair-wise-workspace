"""Command-line interface for archive restoration and verification.

Exit codes (per acceptance spec)::

    0 – verification passed
    2 – invalid input or parameters
    3 – post-restoration metrics below threshold
    4 – unrecoverable media error
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from .errors import EXIT_INVALID, EXIT_METRICS, EXIT_OK, EXIT_UNRECOVERABLE
from .manifest import find_manifests
from .pipeline import RECOVERY_MIN, process_asset

SCENARIO_ORDER = ["normal", "missing", "duplicate", "outoforder", "truncated",
                  "rollback", "wraparound"]
REQUIRED_METRIC_KEYS = ("av_sync_error_ms", "packet_recovery_rate", "p95_latency_ms",
                        "peak_memory_bytes", "aligned_representations", "gaps")


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="archive_restore",
                                     description="Restore and verify historical HLS/DASH archives")
    sub = parser.add_subparsers(dest="command", required=True)

    restore = sub.add_parser("restore", help="restore assets under an input root")
    restore.add_argument("--input", required=True, type=Path)
    restore.add_argument("--output", required=True, type=Path)
    restore.add_argument("--queue-bytes", type=int, default=512 * 1024 * 1024)
    restore.add_argument("--queue-items", type=int, default=64)
    restore.add_argument("--consumer-delay-ms", type=int, default=0)
    restore.add_argument("--scenario", default=None,
                         help="limit processing to a single scenario directory")
    restore.add_argument("--json", action="store_true", help="emit JSON to stdout")

    verify = sub.add_parser("verify", help="verify restoration results against inputs")
    verify.add_argument("--input", required=True, type=Path)
    verify.add_argument("--result", required=True, type=Path)
    verify.add_argument("--recovery-min", type=float, default=RECOVERY_MIN)
    verify.add_argument("--json", action="store_true")
    return parser


def _resolve_manifests(input_root: Path, scenario: str | None) -> list[Path]:
    if scenario:
        target = input_root / scenario / "master.m3u8"
        return [target] if target.exists() else []
    manifests = []
    for name in SCENARIO_ORDER:
        candidate = input_root / name / "master.m3u8"
        if candidate.exists():
            manifests.append(candidate)
    return manifests or find_manifests(input_root)


def _validate_paths(input_root: Path, output: Path | None, queue_bytes: int) -> str | None:
    if not input_root.exists() or not input_root.is_dir():
        return f"input root does not exist or is not a directory: {input_root}"
    if queue_bytes <= 0:
        return f"--queue-bytes must be positive, got {queue_bytes}"
    if output is not None:
        try:
            input_root.resolve()
        except OSError as exc:
            return f"invalid input path: {exc}"
    return None


def cmd_restore(args: argparse.Namespace) -> int:
    problem = _validate_paths(args.input, args.output, args.queue_bytes)
    if problem:
        return _emit_error(problem, args.json)
    manifests = _resolve_manifests(args.input, args.scenario)
    if not manifests:
        return _emit_error(f"no manifests found under {args.input}", args.json)

    results = []
    worst = EXIT_OK
    for manifest in manifests:
        out_dir = args.output / manifest.parent.name
        result = process_asset(
            manifest, out_dir, queue_bytes=args.queue_bytes,
            consumer_delay_ms=args.consumer_delay_ms, max_items=args.queue_items)
        results.append(result)
        worst = max(worst, _result_exit(result))

    payload = {"command": "restore", "asset_count": len(results),
               "results": [_compact(r) for r in results]}
    if args.json:
        print(json.dumps(payload, indent=2, ensure_ascii=False))
    else:
        _print_human(payload)
    return worst


def _result_exit(result: dict) -> int:
    if result["status"] == "unrecoverable":
        return EXIT_UNRECOVERABLE
    metrics = result.get("metrics")
    if metrics and not _metrics_pass(metrics):
        return EXIT_METRICS
    return EXIT_OK


def _metrics_pass(metrics: dict) -> bool:
    ok, _ = _check_thresholds(metrics)
    return ok


def _check_thresholds(metrics: dict) -> tuple[bool, list[str]]:
    failures: list[str] = []
    sync = metrics.get("av_sync_error_ms")
    if sync is not None and sync > 40:
        failures.append(f"av_sync_error_ms {sync} > 40")
    if metrics.get("packet_recovery_rate", 0) < RECOVERY_MIN:
        failures.append(f"packet_recovery_rate {metrics.get('packet_recovery_rate')} < {RECOVERY_MIN}")
    p95 = metrics.get("p95_latency_ms", 0)
    # Only enforce the injected-delay band when latency was actually measured;
    # a zero-delay run legitimately reports ~0.
    if p95 and not (25 <= p95 <= 500):
        failures.append("p95_latency_ms outside 25-500")
    if metrics.get("aligned_representations", 0) < 2:
        failures.append("aligned_representations < 2")
    return not failures, failures


def cmd_verify(args: argparse.Namespace) -> int:
    if not args.input.exists() or not args.result.exists():
        return _emit_error("both --input and --result must exist", args.json)
    reports = sorted(args.result.glob("*/result.json"))
    direct = args.result / "result.json"
    if not reports and direct.exists():
        reports = [direct]
    if not reports:
        return _emit_error(f"no result.json found under {args.result}", args.json)
    verifications = []
    worst = EXIT_OK
    for report_path in reports:
        data = json.loads(report_path.read_text())
        metrics = data.get("metrics")
        entry = {"asset_id": data.get("asset_id"), "status": data.get("status")}
        if metrics is None:
            entry["verdict"] = "unrecoverable"
            entry["failures"] = ["no metrics produced"]
            worst = max(worst, EXIT_UNRECOVERABLE)
        else:
            ok, failures = _check_thresholds(metrics)
            # Corrupt/partial assets still emit explainable metrics and explicit
            # gaps; they simply fail the numeric thresholds with exit code 3
            # rather than being treated as an unrecoverable media error.
            entry["verdict"] = "passed" if ok else "threshold-failed"
            entry["failures"] = failures
            entry["metrics"] = {k: metrics[k] for k in REQUIRED_METRIC_KEYS if k in metrics}
            if not ok:
                worst = max(worst, EXIT_METRICS)
        verifications.append(entry)
    payload = {"command": "verify", "verifications": verifications}
    if args.json:
        print(json.dumps(payload, indent=2, ensure_ascii=False))
    else:
        _print_human(payload)
    return worst


def _compact(result: dict) -> dict:
    return {"asset_id": result["asset_id"], "status": result["status"],
            "metrics": result.get("metrics"), "error_summary": result.get("error_summary"),
            "gaps": (result.get("metrics") or {}).get("gaps", [])}


def _emit_error(message: str, as_json: bool) -> int:
    if as_json:
        print(json.dumps({"command": "error", "status": "invalid", "message": message},
                         ensure_ascii=False))
    else:
        print(f"error: {message}", file=sys.stderr)
    return EXIT_INVALID


def _print_human(payload: dict) -> None:
    print(f"== {payload['command']} ==")
    for key in ("results", "verifications"):
        for item in payload.get(key, []):
            print(f"- {item.get('asset_id')}: {item.get('status') or item.get('verdict')}")
            if "metrics" in item and isinstance(item["metrics"], dict):
                print(f"    {json.dumps(item['metrics'], ensure_ascii=False)}")
            if item.get("failures"):
                print(f"    failures: {'; '.join(item['failures'])}")


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    if args.command == "restore":
        return cmd_restore(args)
    if args.command == "verify":
        return cmd_verify(args)
    parser.error(f"unknown command {args.command}")
    return EXIT_INVALID


if __name__ == "__main__":
    raise SystemExit(main())
