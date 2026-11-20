"""Command-line entry point.

Three subcommands, all terminal-only:

``archive``  parse a master, normalise audio, carry colour metadata, isolate
             damaged segments and write a manifest.
``verify``   re-check a manifest (hashes, keyframe index, round-trip) and
             optionally exercise the repair path against the sample fixtures.
``probe``    print what the parser sees, without transforming anything.

Exit codes are part of the contract:

======  ====================================================
``0``   every required check passed
``1``   an input or processing failure occurred
``2``   the command line itself was wrong
======  ====================================================
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import traceback
from pathlib import Path
from typing import Sequence

from . import __version__
import av

from .errors import MediaError
from .manifest import Manifest
from .pipeline import Limits, archive
from .probe import probe
from .util import quiet_libav
from .verify import _index_checks, integrity_scan, recover, verify_manifest

EXIT_OK = 0
EXIT_FAILURE = 1
EXIT_USAGE = 2


class _UsageError(Exception):
    """Raised for command-line problems; maps to exit code 2."""


def _positive_int(text: str) -> int:
    try:
        value = int(text)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"{text!r} is not an integer") from exc
    if value <= 0:
        raise argparse.ArgumentTypeError("value must be positive")
    return value


def _positive_float(text: str) -> float:
    try:
        value = float(text)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"{text!r} is not a number") from exc
    if value <= 0:
        raise argparse.ArgumentTypeError("value must be positive")
    return value


def _loudness(text: str) -> float:
    try:
        value = float(text)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"{text!r} is not LUFS") from exc
    if not -70.0 <= value <= 0.0:
        raise argparse.ArgumentTypeError("target loudness must be within [-70, 0] LUFS")
    return value


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mediarc",
        description="Local, verifiable archiving for multi-track production masters.",
    )
    parser.add_argument("--version", action="version", version=f"mediarc {__version__}")
    sub = parser.add_subparsers(dest="command", required=True)

    arc = sub.add_parser("archive", help="parse, normalise and archive one master")
    arc.add_argument("--input", required=True, help="source MOV/MP4/MKV file")
    arc.add_argument("--manifest", required=True, help="path of the manifest to write")
    arc.add_argument("--rate", type=_positive_int, default=48000,
                     choices=(44100, 48000, 96000), help="target sample rate")
    arc.add_argument("--layout", default="stereo", choices=("mono", "stereo", "5.1"),
                     help="target channel layout")
    arc.add_argument("--lufs", type=_loudness, default=None,
                     help="integrated loudness ceiling in LUFS")
    arc.add_argument("--map", dest="mapping", action="append", default=[],
                     metavar="ROLE:INDEX",
                     help="explicit stream binding, e.g. --map video:0 --map audio:1")
    arc.add_argument("--work-dir", default=None,
                     help="stage/segment scratch directory (default: beside manifest)")
    arc.add_argument("--resume", action="store_true",
                     help="reuse recorded stages whose input digest still matches")
    arc.add_argument("--jobs", type=_positive_int, default=2,
                     help="parallel segment workers (1-8)")
    arc.add_argument("--max-file-mb", type=_positive_int, default=512,
                     help="refuse source files larger than this")
    arc.add_argument("--max-segment-seconds", type=_positive_float, default=120.0,
                     help="hard ceiling on a single archived segment")
    arc.add_argument("--cut-every", type=_positive_float, default=None,
                     help="force an extra cut at least this often (seconds)")
    arc.add_argument("--transition", dest="transitions", action="append", default=[],
                     metavar="FROM:TO:START_MS:END_MS",
                     help="declare a cross-segment overlap; only declared overlaps "
                          "are legal")

    ver = sub.add_parser("verify", help="verify a manifest and optionally self-test")
    ver.add_argument("--manifest", required=True)
    ver.add_argument("--source", default=None,
                     help="override the source path recorded in the manifest")
    ver.add_argument("--fixtures", default=None,
                     help="fixture directory to exercise the repair paths against")
    ver.add_argument("--self-test", action="store_true",
                     help="scan fixtures for damage and attempt recovery")
    ver.add_argument("--json", action="store_true", help="also emit a JSON summary")

    prb = sub.add_parser("probe", help="show the parse result without transforming")
    prb.add_argument("--input", required=True)
    prb.add_argument("--map", dest="mapping", action="append", default=[],
                     metavar="ROLE:INDEX")
    prb.add_argument("--json", action="store_true")
    return parser


# ---------------------------------------------------------------------------
# archive
# ---------------------------------------------------------------------------


def _cmd_archive(args: argparse.Namespace) -> int:
    manifest_path = Path(args.manifest)
    work_dir = Path(args.work_dir) if args.work_dir else manifest_path.parent / (
        manifest_path.stem + ".work"
    )
    limits = Limits(
        max_file_bytes=args.max_file_mb * 1024 * 1024,
        max_segment_seconds=args.max_segment_seconds,
        jobs=args.jobs,
    )
    try:
        report = archive(
            args.input,
            str(manifest_path),
            rate=args.rate,
            layout=args.layout,
            lufs=args.lufs,
            mapping=args.mapping,
            work_dir=str(work_dir),
            resume=args.resume,
            limits=limits,
            cut_every_seconds=args.cut_every,
            transitions=args.transitions,
        )
    except MediaError as exc:
        print(exc.render(), file=sys.stderr)
        return EXIT_FAILURE
    for line in report.summary_lines():
        print(line)
    for error in report.errors:
        print(error.render(), file=sys.stderr)
    # An isolated segment failure is an accepted outcome (the run continued and
    # the manifest records it).  Only a run that archived nothing is a failure.
    return EXIT_OK if report.healthy else EXIT_FAILURE


# ---------------------------------------------------------------------------
# verify
# ---------------------------------------------------------------------------


def _cmd_verify(args: argparse.Namespace) -> int:
    path = Path(args.manifest)
    try:
        manifest = Manifest.loads(path.read_text(encoding="utf-8"))
    except OSError as exc:
        print(f"error code=io stage=verify path={path} message={exc}", file=sys.stderr)
        return EXIT_FAILURE
    except MediaError as exc:
        print(exc.render(), file=sys.stderr)
        return EXIT_FAILURE

    with quiet_libav():
        report = verify_manifest(manifest, source_path=args.source)
    chain_errors = manifest.verify_chain()
    if chain_errors:
        report.errors.extend(chain_errors)
    recovered = 0
    if args.self_test:
        if not args.fixtures:
            raise _UsageError("--self-test requires --fixtures")
        recovered, self_test_notes, fixture_errors = _self_test(Path(args.fixtures))
        report.recovery_notes.extend(self_test_notes)
        report.errors.extend(fixture_errors)
        report.recovered_segments = recovered

    lines = report.lines()
    if not chain_errors:
        lines.insert(5, "stage_chain=ok")
    else:
        lines.insert(5, f"stage_chain=broken/{len(chain_errors)}")
    for line in lines:
        print(line)
    _record_verification(manifest, path, report, chain_errors)
    if args.json:
        print(json.dumps(_verify_json(report), indent=2, ensure_ascii=False, default=str))
    for error in report.errors:
        print(error.render(), file=sys.stderr)
    return EXIT_OK if report.ok else EXIT_FAILURE


def _record_verification(manifest: Manifest, path: Path, report, chain_errors) -> None:
    """Persist the verify stage next to the manifest.

    Verification is a pipeline stage like any other, so its result is written
    to disk with the manifest's digest as its input.  A later run can then be
    told the manifest was already verified, and by what inputs.
    """
    from .manifest import stage_input_digest, verify_record_path
    from .util import atomic_write_json

    record_path = verify_record_path(path)
    payload = {
        "tool": f"mediarc/{__version__}",
        "manifest": str(path),
        "manifest_sha256": manifest.digest(),
        "input_sha256": stage_input_digest(
            manifest=manifest.digest(),
            source=report.manifest.source.get("sha256") if report.manifest else None,
        ),
        "status": "ok" if report.ok else "failed",
        "roundtrip_rate": round(report.roundtrip_rate, 6),
        "keyframe_hit_rate": round(report.keyframe_hit_rate, 6),
        "segment_hashes": [report.hash_ok, report.hash_total],
        "audio_segment_hashes": [report.audio_ok, report.audio_total],
        "recovered_segments": report.recovered_segments,
        "isolated_segments": [list(item) for item in report.isolated_segments],
        "error_offset": report.error_offset,
        "stage_chain_errors": [e.to_dict() for e in chain_errors],
        "errors": [e.to_dict() for e in report.errors],
        "recovery": report.recovery_notes,
    }
    try:
        atomic_write_json(record_path, payload)
    except OSError as exc:
        print(
            f"error code=io stage=verify path={record_path} message={exc}",
            file=sys.stderr,
        )


def _verify_json(report) -> dict:
    return {
        "roundtrip_rate": round(report.roundtrip_rate, 6),
        "keyframe_hit_rate": round(report.keyframe_hit_rate, 6),
        "segment_hashes": [report.hash_ok, report.hash_total],
        "audio_segment_hashes": [report.audio_ok, report.audio_total],
        "recovered_segments": report.recovered_segments,
        "isolated_segments": [list(item) for item in report.isolated_segments],
        "stage_chain": [s.name for s in report.manifest.stages] if report.manifest else [],
        "error_offset": report.error_offset,
        "errors": [e.to_dict() for e in report.errors],
        "recovery": report.recovery_notes,
    }


#: Fixtures whose damage the self-test localises and repairs.
_DAMAGE_FIXTURES = ("damaged.mp4", "damaged.mkv")
#: Fixtures whose *container index* is defective without the payload being bad.
_INDEX_FIXTURES = ("keyframe_index.mp4",)
#: Fixtures that must be rejected or isolated, with the expected error code.
_EXPECTED_FAILURES = {
    "truncated.mkv": {"truncated-container", "corrupt-packet"},
    "color_conflict.mkv": {"color-conflict"},
    "empty_track.mkv": {"missing-stream"},
}


def _self_test(fixtures: Path) -> tuple[int, list[dict], list[MediaError]]:
    """Scan the damaged fixtures, recover what can be recovered, verify isolation."""
    notes: list[dict] = []
    errors: list[MediaError] = []
    recovered = 0
    if not fixtures.is_dir():
        errors.append(
            MediaError(
                "io", f"fixture directory {fixtures} does not exist",
                stage="verify", path=str(fixtures),
            )
        )
        return recovered, notes, errors

    # Repair work decodes damaged data on purpose; keep libav's log out of the
    # terminal so the numbered metrics stay readable.
    with quiet_libav():
        return _self_test_inner(fixtures)


def _self_test_inner(fixtures: Path) -> tuple[int, list[dict], list[MediaError]]:
    notes: list[dict] = []
    errors: list[MediaError] = []
    recovered = 0
    for name in _DAMAGE_FIXTURES:
        candidate = fixtures / name
        if not candidate.exists():
            continue
        try:
            probed = probe(str(candidate))
        except MediaError as exc:
            errors.append(exc)
            continue
        damage = integrity_scan(probed)
        for spot in damage:
            result = recover(
                probed,
                _pseudo_segment(probed, spot),
                spot,
                str(fixtures / "_recovered"),
            )
            if result.path:
                recovered += 1
            note = result.to_dict()
            note["fixture"] = str(candidate)
            note["reason"] = spot.reason
            note["decoded_frames"] = spot.decoded_frames
            note["expected_frames"] = spot.expected_frames
            note["repaired"] = bool(result.path)
            notes.append(note)
            if result.path and not Path(result.path).exists():
                errors.append(
                    MediaError(
                        "verification-failed",
                        f"recovery of {name} produced no output",
                        stage="verify", path=str(candidate),
                        offset=spot.offset,
                    )
                )
            if result.path:
                try:
                    av.open(result.path).close()
                except Exception:  # noqa: BLE001
                    errors.append(
                        MediaError(
                            "verification-failed",
                            f"recovered file for {name} is not playable",
                            stage="verify", path=result.path,
                            offset=spot.offset,
                        )
                    )
        if not damage:
            # A fixture advertised as damaged must show damage to be useful.
            notes.append({
                "fixture": str(candidate),
                "segment_id": None,
                "offset": None,
                "resume_offset": None,
                "salvaged_ticks": 0,
                "skipped_packets": [],
                "reason": "no-damage-detected",
            })

    for name in _INDEX_FIXTURES:
        candidate = fixtures / name
        if not candidate.exists():
            continue
        try:
            probed = probe(str(candidate))
        except MediaError as exc:
            errors.append(exc)
            continue
        for check in _index_checks(probed, None):
            if check.ok:
                continue
            notes.append({
                "fixture": str(candidate),
                "segment_id": None,
                "offset": None,
                "resume_offset": None,
                "salvaged_ticks": 0,
                "skipped_packets": [],
                "reason": "incomplete-keyframe-index",
                "declared": check.declared,
                "demuxed": check.demuxed,
                "missing_samples": check.missing,
            })
        if not any(
            n.get("fixture") == str(candidate)
            and n.get("reason") == "incomplete-keyframe-index"
            for n in notes
        ):
            errors.append(
                MediaError(
                    "verification-failed",
                    f"fixture {name} should show an incomplete keyframe index but did not",
                    stage="verify", path=str(candidate),
                )
            )

    for name, expected in _EXPECTED_FAILURES.items():
        candidate = fixtures / name
        if not candidate.exists():
            continue
        try:
            probed = probe(str(candidate))
        except MediaError as exc:
            if exc.code not in expected:
                errors.append(exc)
            continue
        codes = {e.code for e in probed.errors}
        if name == "color_conflict.mkv":
            from .color import resolve

            codes |= {w.code for w in resolve(probed.video, path=str(candidate)).warnings}
        if name == "empty_track.mkv":
            # An empty mapped track is diagnosed by the timeline stage, which is
            # where "has packets" becomes meaningful.
            from .pipeline import Limits, build_timeline

            _, timeline_errors = build_timeline(probed, Limits())
            codes |= {e.code for e in timeline_errors}
        if not codes & expected:
            errors.append(
                MediaError(
                    "verification-failed",
                    f"fixture {name} was expected to raise {sorted(expected)} but "
                    f"reported {sorted(codes) or 'nothing'}",
                    stage="verify", path=str(candidate),
                )
            )
    return recovered, notes, errors


def _pseudo_segment(probed, spot):
    from .manifest import SegmentRecord

    return SegmentRecord(
        segment_id=f"{Path(probed.path).stem}_{spot.gop_start_ticks}",
        track_index=spot.stream_index,
        start_ticks=spot.gop_start_ticks,
        end_ticks=spot.gop_start_ticks + 1,
        path="",
        sha256="",
    )


# ---------------------------------------------------------------------------
# probe
# ---------------------------------------------------------------------------


def _cmd_probe(args: argparse.Namespace) -> int:
    try:
        result = probe(args.input, args.mapping)
    except MediaError as exc:
        print(exc.render(), file=sys.stderr)
        return EXIT_FAILURE
    payload = {
        "path": result.path,
        "format": result.format_name,
        "size": result.size,
        "duration_us": result.duration_us,
        "time_base": str(result.time_base.rate),
        "source_sha256": result.source_sha256,
        "mapping": result.mapping.to_dict(),
        "streams": [
            {
                "index": s.index,
                "kind": s.kind,
                "codec": s.codec,
                "time_base": str(s.time_base),
                "packets": s.packet_count,
                "keyframes": [k.ticks for k in s.keyframes],
                "sample_rate": s.sample_rate,
                "layout": s.layout,
                "color_raw": s.color_raw,
            }
            for s in result.streams
        ],
        "errors": [e.to_dict() for e in result.errors],
    }
    if args.json:
        print(json.dumps(payload, indent=2, ensure_ascii=False))
    else:
        print(f"source={result.path} format={result.format_name} "
              f"time_base={result.time_base.rate}")
        print(f"mapping basis={result.mapping.basis} video={result.mapping.video} "
              f"audio={result.mapping.audio}")
        for stream in result.streams:
            print(f"stream {stream.index} kind={stream.kind} codec={stream.codec} "
                  f"packets={stream.packet_count} keyframes={len(stream.keyframes)}")
        for error in result.errors:
            print(error.render())
    return EXIT_OK if not result.errors else EXIT_FAILURE


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "archive":
            return _cmd_archive(args)
        if args.command == "verify":
            return _cmd_verify(args)
        if args.command == "probe":
            return _cmd_probe(args)
    except _UsageError as exc:
        parser.print_usage(sys.stderr)
        print(f"mediarc: error: {exc}", file=sys.stderr)
        return EXIT_USAGE
    except MediaError as exc:
        print(exc.render(), file=sys.stderr)
        return EXIT_FAILURE
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        return EXIT_FAILURE
    except Exception as exc:  # noqa: BLE001 - last-resort diagnostic
        print(f"unexpected failure: {type(exc).__name__}: {exc}", file=sys.stderr)
        if os.environ.get("MEDIARC_TRACEBACK"):
            traceback.print_exc()
        return EXIT_FAILURE
    parser.print_help(sys.stderr)
    return EXIT_USAGE


if __name__ == "__main__":
    raise SystemExit(main())
