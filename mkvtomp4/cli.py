"""Command-line entry point with a single JSON response contract."""

from __future__ import annotations

import argparse
import json
import os
import sys
import traceback

from mkvtomp4.ebml.reader import EBMLReadError
from mkvtomp4.mkv.blocks import parse_media_index
from mkvtomp4.mkv.tracks import NoUsableTracksError, ZeroSamplesError
from mkvtomp4.mkv.tree import MatroskaTree
from mkvtomp4.mp4.export_frag import export_fragmented
from mkvtomp4.mp4.export_plain import ExportError, export_plain
from mkvtomp4.timing.normalize import TimestampNormalizeError, normalize_media
from mkvtomp4.verify import VerifyError, verify_output

EXIT_OK = 0
EXIT_EBML = 2
EXIT_NO_TRACKS = 3
EXIT_TIMING = 4
EXIT_VERIFY = 5


class JsonArgumentParser(argparse.ArgumentParser):
    def error(self, message: str) -> None:
        _emit(_error_payload(EXIT_EBML, "cli_usage", message), True)
        raise SystemExit(EXIT_EBML)


def _emit(payload: dict[str, object], pretty: bool) -> None:
    print(json.dumps(payload, ensure_ascii=False, indent=2 if pretty else None, sort_keys=True))


def _error_payload(
    code: int,
    error: str,
    message: str,
    *,
    path: str | None = None,
    detail: dict[str, object] | None = None,
) -> dict[str, object]:
    payload: dict[str, object] = {
        "ok": False,
        "exit_code": code,
        "error": {"code": error, "message": message},
    }
    if path is not None:
        payload["path"] = path
    if detail:
        payload["detail"] = detail
    return payload


def _classify(exc: BaseException, *, verify: bool = False) -> tuple[int, str]:
    if isinstance(exc, NoUsableTracksError):
        return EXIT_NO_TRACKS, "no_usable_tracks"
    if isinstance(exc, ZeroSamplesError):
        return EXIT_NO_TRACKS, "zero_samples"
    if isinstance(exc, TimestampNormalizeError):
        return EXIT_TIMING, "timestamp_not_monotonic"
    if isinstance(exc, VerifyError):
        return EXIT_VERIFY, "verify_io"
    if verify:
        return EXIT_VERIFY, "verify_io"
    if isinstance(exc, (EBMLReadError, ExportError, OSError, ValueError)):
        return EXIT_EBML, "ebml_structure"
    return EXIT_EBML, "internal_error"


def _probe_summary(path: str) -> dict[str, object]:
    tree = MatroskaTree(path)
    summary = tree.summary()
    return {
        "ok": True,
        "file": path,
        "element_summary": {
            "element_count": summary.element_count,
            "max_depth": summary.max_depth,
            "unknown_size_count": summary.unknown_size_count,
            "cue_entries": summary.cue_entries,
            "segment_count": summary.segment_count,
        },
        "id_counts": summary.id_counts,
        "first_elements": summary.first_elements,
        "warnings": tree.warnings,
    }


def _do_probe(args) -> tuple[int, dict[str, object]]:
    return EXIT_OK, _probe_summary(args.source)


def _do_remux(args) -> tuple[int, dict[str, object]]:
    media = parse_media_index(args.source)
    normalized = normalize_media(media)
    if args.path == "plain":
        output = export_plain(normalized, args.out)
    else:
        output = export_fragmented(normalized, args.out, frag_duration=args.frag_duration)
    track_map = [
        {
            "mkv_track": track.track.mkv_track,
            "mp4_track_id": track.track.mp4_track_id,
            "codec": track.track.codec,
            "timescale": track.track.timescale,
            "default_duration_ns": track.track.default_duration_ns,
            "codec_private_size": len(track.track.codec_private),
            "sample_count": track.sample_count,
            "keyframe_count": track.keyframe_count,
        }
        for track in normalized.tracks
    ]
    warnings = list(media.warnings) + list(normalized.warnings)
    return EXIT_OK, {
        "ok": True,
        "source": args.source,
        "track_map": track_map,
        "timestamps": normalized.timestamp_summary(),
        "index": {
            "source": media.cue_stats.index_source,
            "cue_entries": media.cue_stats.entries,
            "cue_valid": media.cue_stats.valid,
            "cue_rejected": media.cue_stats.rejected,
            "rebuilt_samples": media.cue_stats.rebuilt_samples,
        },
        "skipped_tracks": [
            {
                "mkv_track": skipped.mkv_track,
                "codec": skipped.codec,
                "track_type": skipped.track_type,
                "reason": skipped.reason,
            }
            for skipped in media.skipped
        ],
        "warnings": warnings,
        "output": output,
    }


def _do_verify(args) -> tuple[int, dict[str, object]]:
    media = parse_media_index(args.source)
    normalized = normalize_media(media)
    result = verify_output(args.output, normalized)
    result.update({"source": args.source, "output": args.output})
    return (EXIT_OK if result["ok"] else EXIT_VERIFY), result


def _do_selftest(args) -> tuple[int, dict[str, object]]:
    from mkvtomp4.selftest import run_selftest

    result = run_selftest(args.fixtures)
    return (EXIT_OK if result["ok"] else EXIT_VERIFY), result


def _parser() -> argparse.ArgumentParser:
    parser = JsonArgumentParser(prog="mkvtomp4")
    sub = parser.add_subparsers(dest="command", required=True)
    probe = sub.add_parser("probe", help="inspect Matroska structure")
    probe.add_argument("source")
    probe.add_argument("--json", action="store_true")
    remux = sub.add_parser("remux", help="remux Matroska to MP4")
    remux.add_argument("source")
    remux.add_argument("--path", choices=("plain", "frag"), required=True)
    remux.add_argument("--out", required=True)
    remux.add_argument("--frag-duration", type=float, default=2.0)
    remux.add_argument("--json", action="store_true")
    verify = sub.add_parser("verify", help="read MP4 back and compare with source")
    verify.add_argument("output")
    verify.add_argument("--source", required=True)
    verify.add_argument("--json", action="store_true")
    selftest = sub.add_parser("selftest", help="run built-in fixture checks")
    selftest.add_argument("--fixtures", default="fixtures")
    selftest.add_argument("--json", action="store_true")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    pretty = bool(getattr(args, "json", False))
    try:
        if args.command == "probe":
            code, payload = _do_probe(args)
        elif args.command == "remux":
            code, payload = _do_remux(args)
        elif args.command == "verify":
            code, payload = _do_verify(args)
        else:
            code, payload = _do_selftest(args)
    except BaseException as exc:
        path = getattr(args, "source", None) or getattr(args, "output", None)
        code, kind = _classify(exc, verify=args.command == "verify")
        payload = _error_payload(
            code,
            kind,
            str(exc),
            path=path,
            detail={"exception": type(exc).__name__},
        )
        if os.environ.get("MKVTOMP4_DEBUG"):
            payload["traceback"] = traceback.format_exc()
        _emit(payload, pretty)
        return code
    payload.setdefault("exit_code", code)
    _emit(payload, pretty)
    return code


if __name__ == "__main__":
    raise SystemExit(main())
