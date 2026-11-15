"""Command-line interface with JSON-only stdout and stable exit codes."""

from __future__ import annotations

import argparse
import json
import sys

from .detector import DetectorParams, detect
from .errors import AppError, AudioError, EXIT_AUDIO, EXIT_OK, FileSystemError, UsageError
from .exporter import export_directory
from .fixtures import generate_fixtures
from .selftest import run_selftest
from .store import Store
from .wavio import WavReader, sha256_file


class JsonArgumentParser(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        raise UsageError("invalid_arguments", message)


def _add_json(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--json", action="store_true", help="emit machine-readable JSON (default)")


def _add_detection_args(parser: argparse.ArgumentParser) -> None:
    defaults = DetectorParams()
    parser.add_argument("--frame-ms", type=float, default=defaults.frame_ms)
    parser.add_argument("--hop-ms", type=float, default=defaults.hop_ms)
    parser.add_argument("--enter-db", type=float, default=defaults.enter_db)
    parser.add_argument("--exit-db", type=float, default=defaults.exit_db)
    parser.add_argument("--min-seg-ms", type=float, default=defaults.min_seg_ms)
    parser.add_argument("--min-sil-ms", type=float, default=defaults.min_sil_ms)
    parser.add_argument("--pad-ms", type=float, default=defaults.pad_ms)
    parser.add_argument("--hysteresis-frames", type=int, default=defaults.hysteresis_frames)
    parser.add_argument(
        "--allow-multichannel",
        action="store_true",
        help="downmix stereo/multichannel input instead of rejecting it",
    )


def _params(args: argparse.Namespace) -> DetectorParams:
    return DetectorParams(
        frame_ms=args.frame_ms,
        hop_ms=args.hop_ms,
        enter_db=args.enter_db,
        exit_db=args.exit_db,
        min_seg_ms=args.min_seg_ms,
        min_sil_ms=args.min_sil_ms,
        pad_ms=args.pad_ms,
        hysteresis_frames=args.hysteresis_frames,
    )


def _check_channels(reader: WavReader, allowed: bool) -> None:
    if reader.header.channels > 1 and not allowed:
        raise AudioError(
            "stereo_not_supported",
            "stereo or multichannel input requires --allow-multichannel",
            path=str(reader.path),
        )


def _scan_payload(path: str, detection) -> dict:
    sample_rate = detection.analysis.sample_rate
    silence_samples = sum(item.end_sample - item.start_sample for item in detection.silences)
    return {
        "file": path,
        "sample_rate": sample_rate,
        "total_samples": detection.analysis.total_samples,
        "noise_floor_dbfs": round(detection.thresholds.noise_floor_dbfs, 3),
        "threshold_dbfs": round(detection.thresholds.enter_dbfs, 3),
        "segment_count": len(detection.segments),
        "segments": [
            {
                "index": index,
                "start_sample": segment.start_sample,
                "end_sample": segment.end_sample,
                "duration_ms": round(
                    (segment.end_sample - segment.start_sample) * 1000.0 / sample_rate, 3
                ),
                "peak_dbfs": round(segment.peak_dbfs, 3),
            }
            for index, segment in enumerate(detection.segments)
        ],
        "silence_total_ms": round(silence_samples * 1000.0 / sample_rate, 3),
        "silence_ratio": round(
            silence_samples / detection.analysis.total_samples, 6
        ),
        "dropped_segments": detection.dropped_segments,
    }


def _cmd_scan(args: argparse.Namespace) -> tuple[dict, int]:
    params = _params(args)
    params.validate()
    with WavReader(args.file) as reader:
        _check_channels(reader, args.allow_multichannel)
        detection = detect(reader, params)
    digest = sha256_file(args.file)
    payload = _scan_payload(str(args.file), detection)
    with Store(args.db) as store:
        batch_id = store.start_batch("scan", params.as_dict())
        try:
            file_id = store.record_scan(
                batch_id=batch_id,
                source_path=str(args.file),
                content_sha256=digest,
                params=params.as_dict(),
                detection=detection,
            )
            store.finish_batch(batch_id)
        except Exception:
            try:
                store.finish_batch(batch_id, "failed")
            except Exception:
                pass
            raise
    payload["file_id"] = file_id
    payload["content_sha256"] = digest
    return payload, EXIT_OK


def _cmd_export(args: argparse.Namespace) -> tuple[dict, int]:
    params = _params(args)
    manifest = export_directory(
        args.directory,
        args.db,
        args.out,
        args.manifest,
        params,
        allow_multichannel=args.allow_multichannel,
    )
    return {
        "manifest": str(args.manifest),
        "out_dir": str(args.out),
        "file_count": manifest["file_count"],
        "segment_count": manifest["segment_count"],
        "silence_total_ms": manifest["silence_total_ms"],
    }, EXIT_OK


def _cmd_fixtures(args: argparse.Namespace) -> tuple[dict, int]:
    if args.files < 1:
        raise UsageError("invalid_arguments", "--files must be positive")
    return generate_fixtures(args.out, seed=args.seed, files=args.files), EXIT_OK


def _cmd_report(args: argparse.Namespace) -> tuple[dict, int]:
    with Store(args.db, readonly=True) as store:
        return store.report(), EXIT_OK


def _cmd_selftest(args: argparse.Namespace) -> tuple[dict, int]:
    return run_selftest(args.fixtures, args.tol_samples)


def build_parser() -> argparse.ArgumentParser:
    parser = JsonArgumentParser(prog="audiosplit", description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    scan = subparsers.add_parser("scan", help="scan one WAV master")
    scan.add_argument("file")
    scan.add_argument("--db", required=True)
    _add_detection_args(scan)
    _add_json(scan)
    scan.set_defaults(func=_cmd_scan)

    export = subparsers.add_parser("export", help="recursively export WAV segments")
    export.add_argument("--dir", dest="directory", required=True)
    export.add_argument("--db", required=True)
    export.add_argument("--out", required=True)
    export.add_argument("--manifest", required=True)
    _add_detection_args(export)
    _add_json(export)
    export.set_defaults(func=_cmd_export)

    fixtures = subparsers.add_parser("fixtures", help="generate deterministic test masters")
    fixtures.add_argument("--out", required=True)
    fixtures.add_argument("--seed", type=int, default=7)
    fixtures.add_argument("--files", type=int, default=6)
    _add_json(fixtures)
    fixtures.set_defaults(func=_cmd_fixtures)

    report = subparsers.add_parser("report", help="report aggregates from SQLite")
    report.add_argument("--db", required=True)
    _add_json(report)
    report.set_defaults(func=_cmd_report)

    selftest = subparsers.add_parser("selftest", help="compare fixtures against known truth")
    selftest.add_argument("--fixtures", required=True)
    selftest.add_argument("--tol-samples", type=int, default=48)
    _add_json(selftest)
    selftest.set_defaults(func=_cmd_selftest)
    return parser


def _emit(payload: dict) -> None:
    json.dump(payload, sys.stdout, ensure_ascii=False, allow_nan=False)
    sys.stdout.write("\n")


def main(argv: list[str] | None = None) -> int:
    try:
        args = build_parser().parse_args(argv)
        payload, code = args.func(args)
    except AppError as exc:
        _emit({"error": exc.as_dict(), "exit_code": exc.exit_code})
        return exc.exit_code
    except OSError as exc:
        error = FileSystemError("filesystem_error", str(exc))
        _emit({"error": error.as_dict(), "exit_code": EXIT_AUDIO})
        return EXIT_AUDIO
    except Exception as exc:
        _emit(
            {
                "error": {"code": "internal_error", "message": str(exc)},
                "exit_code": EXIT_AUDIO,
            }
        )
        return EXIT_AUDIO
    _emit(payload)
    return code


if __name__ == "__main__":
    raise SystemExit(main())

