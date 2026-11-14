from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys
from typing import Any, Callable

from .convert import SUPPORTED_VERSIONS, load_timeline, save_timeline, normalize_version
from .errors import CutplanError
from .otio_bridge import export_timeline, roundtrip_check
from .render import render_timeline
from .resolve import POLICIES, ResolutionStats, build_report, resolve_timeline
from .validate import validate_timeline


def _emit(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2))


def _error(error: CutplanError, *, as_json: bool, batch: bool = False, path: str | None = None) -> int:
    if as_json or batch:
        payload = {"ok": False, "error": {"code": error.code}}
        if path is not None:
            payload["error"]["file"] = path
        if not batch:
            payload["error"]["message"] = error.message
        _emit(payload)
    else:
        print(f"ERROR {error.code}: {error.message}", file=sys.stderr)
    return error.exit_code


def _plan_files(directory: str | Path) -> list[Path]:
    root = Path(directory)
    files = sorted(
        [path for path in root.iterdir() if path.is_file() and path.suffix.lower() in {".txt", ".json"}],
        key=lambda path: path.name,
    )
    if not files:
        raise CutplanError("E_BATCH_EMPTY", "batch directory contains no .txt or .json plans", exit_code=2)
    return files


def _validate(args: argparse.Namespace) -> int:
    source = Path(args.input)
    if source.is_dir() or args.batch:
        if not source.is_dir():
            raise CutplanError("E_BATCH_INPUT", "batch input must be a directory")
        summary: dict[str, Any] = {"ok": True, "success": 0, "failed": 0, "files": []}
        for path in _plan_files(source):
            try:
                report = validate_timeline(load_timeline(path))
                summary["success"] += 1
                summary["files"].append(
                    {
                        "file": str(path),
                        "ok": report.ok,
                        "clusters": report.conflicts["clusters"],
                        "pairs": report.conflicts["pairs"],
                    }
                )
                if not report.ok:
                    summary["files"][-1]["errors"] = [error.code for error in report.errors]
            except CutplanError as error:
                summary["failed"] += 1
                summary["files"].append({"file": str(path), "ok": False, "error_code": error.code})
            except Exception:
                summary["failed"] += 1
                summary["files"].append({"file": str(path), "ok": False, "error_code": "E_INTERNAL"})
        summary["ok"] = summary["failed"] == 0 and all(item["ok"] for item in summary["files"])
        _emit(summary)
        return 0 if summary["ok"] else 3
    try:
        report = validate_timeline(load_timeline(source))
    except CutplanError as error:
        return _error(error, as_json=args.json)
    except Exception:
        return _error(CutplanError("E_INTERNAL", "unexpected failure"), as_json=args.json)
    _emit(report.as_dict())
    return 0 if report.ok else 3


def _resolve(args: argparse.Namespace) -> int:
    source = Path(args.input)
    if source.is_dir():
        return _resolve_batch(args, source)
    if not args.out:
        raise CutplanError("E_OUTPUT_REQUIRED", "--out is required for a single input")
    try:
        result = resolve_timeline(load_timeline(source), args.policy, cross_track=args.cross_track)
        target = save_timeline(result.timeline, args.out, args.format)
        if args.report:
            Path(args.report).parent.mkdir(parents=True, exist_ok=True)
            Path(args.report).write_text(
                json.dumps(result.report, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
                encoding="utf-8",
            )
        output = dict(result.report)
        output["out"] = str(args.out)
        output["format"] = target
        _emit(output)
        return _resolve_status(result.report)
    except CutplanError as error:
        return _error(error, as_json=args.json)
    except Exception:
        return _error(CutplanError("E_INTERNAL", "unexpected failure", exit_code=4), as_json=args.json)


def _resolve_batch(args: argparse.Namespace, source: Path) -> int:
    if not args.out_dir:
        raise CutplanError("E_OUTPUT_REQUIRED", "--out-dir is required for batch resolution")
    destination = Path(args.out_dir)
    destination.mkdir(parents=True, exist_ok=True)
    summary: dict[str, Any] = {"ok": True, "success": 0, "failed": 0, "files": []}
    for path in _plan_files(source):
        try:
            result = resolve_timeline(load_timeline(path), args.policy, cross_track=args.cross_track)
            suffix = ".json" if normalize_version(args.format) == "v2" else ".txt"
            output_path = destination / f"{path.stem}.resolved{suffix}"
            save_timeline(result.timeline, output_path, args.format)
            report_path = destination / f"{path.stem}.report.json"
            report_path.write_text(
                json.dumps(result.report, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
                encoding="utf-8",
            )
            status = _resolve_status(result.report)
            if status == 0:
                summary["success"] += 1
                summary["files"].append({"file": str(path), "ok": True})
            else:
                summary["failed"] += 1
                code = "E_RESOLVE_CONFLICT" if status == 4 else "E_SEMANTIC_INVALID"
                summary["files"].append({"file": str(path), "ok": False, "error_code": code})
        except CutplanError as error:
            summary["failed"] += 1
            summary["files"].append({"file": str(path), "ok": False, "error_code": error.code})
        except Exception:
            summary["failed"] += 1
            summary["files"].append({"file": str(path), "ok": False, "error_code": "E_INTERNAL"})
    summary["ok"] = summary["failed"] == 0 and all(item["ok"] for item in summary["files"])
    _emit(summary)
    return 0 if summary["ok"] else 3


def _resolve_status(report: dict[str, Any]) -> int:
    if report.get("clusters_after", 0):
        return 4
    if report.get("validation_errors"):
        return 3
    return 0


def _convert(args: argparse.Namespace) -> int:
    source = Path(args.input)
    if source.is_dir():
        if not args.out_dir:
            raise CutplanError("E_OUTPUT_REQUIRED", "--out-dir is required for batch conversion")
        destination = Path(args.out_dir)
        destination.mkdir(parents=True, exist_ok=True)
        summary: dict[str, Any] = {"ok": True, "success": 0, "failed": 0, "files": []}
        for path in _plan_files(source):
            try:
                suffix = ".json" if normalize_version(args.to) == "v2" else ".txt"
                output_path = destination / f"{path.stem}.{normalize_version(args.to)}{suffix}"
                timeline = load_timeline(path)
                save_timeline(timeline, output_path, args.to)
                summary["success"] += 1
                summary["files"].append({"file": str(path), "ok": True, "out": str(output_path)})
            except CutplanError as error:
                summary["failed"] += 1
                summary["files"].append({"file": str(path), "ok": False, "error_code": error.code})
            except Exception:
                summary["failed"] += 1
                summary["files"].append({"file": str(path), "ok": False, "error_code": "E_INTERNAL"})
        summary["ok"] = summary["failed"] == 0
        _emit(summary)
        return 0 if summary["ok"] else 3
    if not args.out:
        raise CutplanError("E_OUTPUT_REQUIRED", "--out is required for a single input")
    try:
        timeline = load_timeline(source)
        target = save_timeline(timeline, args.out, args.to)
        _emit(
            {
                "ok": True,
                "out": str(args.out),
                "version": target,
                "tracks": len(timeline.tracks),
                "clips": timeline.clip_count,
            }
        )
        return 0
    except CutplanError as error:
        return _error(error, as_json=args.json)
    except Exception:
        return _error(CutplanError("E_INTERNAL", "unexpected failure"), as_json=args.json)


def _otio(args: argparse.Namespace) -> int:
    if args.otio_command == "export":
        try:
            timeline = load_timeline(args.input)
            validation = validate_timeline(timeline, include_overlap_errors=False)
            if validation.conflicts["clusters"]:
                raise CutplanError("E_UNRESOLVED_CONFLICT", "resolve conflicts before OTIO export", exit_code=4)
            export_timeline(timeline, args.out)
            _emit(
                {
                    "ok": True,
                    "out": str(args.out),
                    "tracks": len(timeline.tracks),
                    "clips": timeline.clip_count,
                    "duration_frames": timeline.duration_frames,
                }
            )
            return 0
        except CutplanError as error:
            return _error(error, as_json=args.json)
        except Exception:
            return _error(CutplanError("E_INTERNAL", "unexpected failure", exit_code=5), as_json=args.json)
    try:
        result = roundtrip_check(args.input)
        _emit(result)
        return 0 if result["ok"] else 5
    except CutplanError as error:
        return _error(error, as_json=args.json)
    except Exception:
        return _error(CutplanError("E_INTERNAL", "unexpected failure", exit_code=5), as_json=args.json)


def _render(args: argparse.Namespace) -> int:
    source = Path(args.input)
    if source.is_dir():
        if not args.out_dir:
            raise CutplanError("E_OUTPUT_REQUIRED", "--out-dir is required")
        summary: dict[str, Any] = {"ok": True, "success": 0, "failed": 0, "files": []}
        for path in _plan_files(source):
            try:
                timeline = load_timeline(path)
                validation = validate_timeline(timeline, include_overlap_errors=False)
                if validation.conflicts["clusters"]:
                    raise CutplanError("E_UNRESOLVED_CONFLICT", "unresolved conflicts", exit_code=4)
                result = render_timeline(timeline, Path(args.out_dir) / path.stem, width=args.width, height=args.height)
                summary["success"] += 1
                summary["files"].append({"file": str(path), "ok": True, "summary": result})
            except CutplanError as error:
                summary["failed"] += 1
                summary["files"].append({"file": str(path), "ok": False, "error_code": error.code})
            except Exception:
                summary["failed"] += 1
                summary["files"].append({"file": str(path), "ok": False, "error_code": "E_INTERNAL"})
        summary["ok"] = summary["failed"] == 0
        _emit(summary)
        return 0 if summary["ok"] else 5
    try:
        timeline = load_timeline(source)
        validation = validate_timeline(timeline, include_overlap_errors=False)
        if validation.conflicts["clusters"]:
            raise CutplanError("E_UNRESOLVED_CONFLICT", "resolve conflicts before rendering", exit_code=4)
        result = render_timeline(timeline, args.out_dir, width=args.width, height=args.height)
        if args.summary:
            Path(args.summary).parent.mkdir(parents=True, exist_ok=True)
            Path(args.summary).write_text(json.dumps(result, ensure_ascii=False, sort_keys=True, indent=2) + "\n", encoding="utf-8")
        _emit(result)
        return 0
    except CutplanError as error:
        return _error(error, as_json=args.json)
    except Exception:
        return _error(CutplanError("E_RENDER_SELF_CHECK", "unexpected render failure", exit_code=5), as_json=args.json)


def _report(args: argparse.Namespace) -> int:
    try:
        timeline = load_timeline(args.input)
        stats = ResolutionStats(policy=str(timeline.metadata.get("resolution_policy", "")))
        report = build_report(timeline, stats)
        _emit(report)
        return _resolve_status(report)
    except CutplanError as error:
        return _error(error, as_json=args.json)
    except Exception:
        return _error(CutplanError("E_INTERNAL", "unexpected failure"), as_json=args.json)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="cutplan", description="Frame-accurate cut plan tool")
    subparsers = parser.add_subparsers(dest="command", required=True)
    validate_parser = subparsers.add_parser("validate")
    validate_parser.add_argument("input")
    validate_parser.add_argument("--json", action="store_true")
    validate_parser.add_argument("--batch", action="store_true")
    validate_parser.set_defaults(func=_validate)
    resolve_parser = subparsers.add_parser("resolve")
    resolve_parser.add_argument("input")
    resolve_parser.add_argument("--policy", choices=POLICIES, default="trim_head")
    resolve_parser.add_argument("--out")
    resolve_parser.add_argument("--out-dir")
    resolve_parser.add_argument("--report")
    resolve_parser.add_argument("--format", choices=SUPPORTED_VERSIONS, default="v2")
    resolve_parser.add_argument("--cross-track", choices=("truncate", "orphan"))
    resolve_parser.add_argument("--json", action="store_true")
    resolve_parser.set_defaults(func=_resolve)
    convert_parser = subparsers.add_parser("convert")
    convert_parser.add_argument("input")
    convert_parser.add_argument("--to", required=True, choices=SUPPORTED_VERSIONS)
    convert_parser.add_argument("--out")
    convert_parser.add_argument("--out-dir")
    convert_parser.add_argument("--json", action="store_true")
    convert_parser.set_defaults(func=_convert)
    otio_parser = subparsers.add_parser("otio")
    otio_sub = otio_parser.add_subparsers(dest="otio_command", required=True)
    export_parser = otio_sub.add_parser("export")
    export_parser.add_argument("input")
    export_parser.add_argument("--out", required=True)
    export_parser.add_argument("--json", action="store_true")
    export_parser.set_defaults(func=_otio)
    roundtrip_parser = otio_sub.add_parser("roundtrip")
    roundtrip_parser.add_argument("input")
    roundtrip_parser.add_argument("--json", action="store_true")
    roundtrip_parser.set_defaults(func=_otio)
    render_parser = subparsers.add_parser("render")
    render_parser.add_argument("input")
    render_parser.add_argument("--out-dir", required=True)
    render_parser.add_argument("--width", type=int, default=64)
    render_parser.add_argument("--height", type=int, default=36)
    render_parser.add_argument("--summary")
    render_parser.add_argument("--json", action="store_true")
    render_parser.set_defaults(func=_render)
    report_parser = subparsers.add_parser("report")
    report_parser.add_argument("input")
    report_parser.add_argument("--json", action="store_true")
    report_parser.set_defaults(func=_report)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    try:
        args = parser.parse_args(argv)
        return int(args.func(args))
    except CutplanError as error:
        return _error(error, as_json=bool(getattr(args, "json", False)))
    except Exception:
        print("ERROR E_INTERNAL", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
