"""Terminal entry point: `demo`, `scan`, `verify` and `resume` commands.

Exit codes (documented in ``mediacore.errors``):

* 0 - everything verified
* 2 - bad arguments, bad input, or an incompatible checkpoint
* 3 - a recoverable interruption; re-running the same command resumes
* 4 - a verification metric is outside its documented limit
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any, Sequence

from .checkpoint import CheckpointConflict
from .errors import (
    EXIT_INPUT, EXIT_INTERRUPTED, EXIT_METRICS, EXIT_OK,
    Interrupted, MediaCorruptError, MediaError, MediaInputError, MetricsExceeded,
)

PROG = "mediacore"


def _add_demo_arguments(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--codec", choices=("h264", "h265", "av1"), default="h264")
    parser.add_argument("--duration", type=int, default=12, help="seconds (default 12)")
    parser.add_argument("--bitrate", type=int, default=900_000, help="target video bps")
    parser.add_argument("--seed", type=int, default=41)
    parser.add_argument("--width", type=int, default=320)
    parser.add_argument("--height", type=int, default=180)
    parser.add_argument("--fps", type=int, default=30)
    parser.add_argument("--gop", type=int, default=15)
    parser.add_argument("--out", type=Path, required=True, help="output directory")
    parser.add_argument("--corrupt-tail", action="store_true",
                        help="truncate the generated stream to exercise error paths")
    parser.add_argument("--interrupt-after", type=int, default=0, metavar="N",
                        help="exit with status 3 after N committed stages")
    parser.add_argument("--index", type=Path, default=None,
                        help="optional untrusted index JSON to validate")
    parser.add_argument("--json", action="store_true", help="emit the summary as JSON")
    parser.add_argument("--quiet", action="store_true", help="suppress the summary line")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog=PROG,
        description="Bounded-memory raw-bitstream timeline recovery and verification.")
    sub = parser.add_subparsers(dest="command", required=True)

    demo = sub.add_parser("demo", help="generate a local fixture and run the pipeline")
    _add_demo_arguments(demo)

    scan = sub.add_parser("scan", help="scan one raw bitstream and print defects")
    scan.add_argument("--input", type=Path, required=True)
    scan.add_argument("--codec", choices=("h264", "h265", "av1"), required=True)
    scan.add_argument("--json", action="store_true")

    verify = sub.add_parser("verify", help="verify a manifest and its rebuilt index")
    verify.add_argument("--manifest", type=Path, required=True)
    verify.add_argument("--index", type=Path, default=None)
    verify.add_argument("--deviation-pct", type=float, default=None)
    verify.add_argument("--alignment-ms", type=float, default=None)
    verify.add_argument("--json", action="store_true")

    resume = sub.add_parser("resume", help="resume an interrupted demo run")
    _add_demo_arguments(resume)

    return parser


def _spec_from_args(args: argparse.Namespace):
    from .fixtures import FixtureSpec

    return FixtureSpec(
        codec=args.codec, duration_s=args.duration, bitrate=args.bitrate,
        seed=args.seed, width=args.width, height=args.height,
        fps=args.fps, gop=args.gop, corrupt_tail=args.corrupt_tail)


def _config_from_args(args: argparse.Namespace):
    from .pipeline import PipelineConfig

    return PipelineConfig(
        codec=args.codec, target_bps=args.bitrate, gop=args.gop,
        fps=args.fps, seed=args.seed, width=args.width, height=args.height,
        duration_s=args.duration, interrupt_after=args.interrupt_after,
        corrupt_tail=args.corrupt_tail)


def _summary(manifest, *, out: Path) -> dict[str, Any]:
    return {
        "run_id": manifest.run_id,
        "target_bps": manifest.target_bps,
        "actual_bps": round(manifest.actual_bps, 1),
        "deviation_pct": round(manifest.deviation_pct, 3),
        "video_psnr_db": manifest.quality["video"].get("psnr_db"),
        "audio_rms": round(manifest.audio.get("rms", 0.0), 6),
        "manifest_alignment_max_ms": round(manifest.manifest_alignment_max_ms, 3),
        "random_access_max_ms": round(manifest.random_access_max_ms, 3),
        "segments": len(manifest.segments),
        "issues": len(manifest.issues),
        "manifest_digest": manifest.digest,
        "manifest_path": str(out / "manifest.json"),
        "index_path": str(out / "index.json"),
        "truncated_tail_offset": manifest.video.get("truncated_tail_offset"),
    }


def _print_summary(summary: dict[str, Any], *, quiet: bool, as_json: bool) -> None:
    if quiet:
        return
    if as_json:
        print(json.dumps(summary, indent=2, sort_keys=True))
        return
    print(f"target_bps={summary['target_bps']} actual_bps={summary['actual_bps']} "
          f"deviation_pct={summary['deviation_pct']}")
    print(f"video_psnr_db={summary['video_psnr_db']} audio_rms={summary['audio_rms']}")
    print(f"manifest_alignment_max_ms={summary['manifest_alignment_max_ms']} "
          f"random_access_max_ms={summary['random_access_max_ms']}")
    if summary.get("truncated_tail_offset") is not None:
        print(f"corrupt_tail_offset={summary['truncated_tail_offset']}")
    print(f"segments={summary['segments']} issues={summary['issues']} "
          f"manifest={summary['manifest_path']} digest={summary['manifest_digest']}")


def _cmd_demo(args: argparse.Namespace, *, resume_only: bool = False) -> int:
    from .fixtures import make_fixtures
    from .manifest import verify_manifest
    from .pipeline import run_pipeline

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    spec = _spec_from_args(args)
    fixture_info = make_fixtures(out / "input", spec)
    config = _config_from_args(args)
    index_payload = None
    if args.index is not None:
        index_payload = _read_json(args.index)
    result = run_pipeline(
        fixture_info["video_path"], fixture_info["audio"]["path"],
        out / "run", config, index_payload=index_payload)
    manifest = result.manifest
    manifest.save(str(out / "manifest.json"))
    (out / "index.json").write_text(
        json.dumps(result.index.to_dict(), indent=2, sort_keys=True) + "\n")
    failures = verify_manifest(manifest)
    summary = _summary(manifest, out=out)
    summary["resumed"] = result.resumed
    _print_summary(summary, quiet=args.quiet, as_json=args.json)
    if manifest.video.get("truncated_tail_offset") is not None:
        offset = manifest.video["truncated_tail_offset"]
        reason = manifest.video.get("truncated_tail_reason", "truncated tail")
        print(f"error (corrupt_input) at offset {offset}: {reason}", file=sys.stderr)
        return EXIT_INPUT
    if failures:
        for failure in failures:
            print(f"verify_failed: {failure}", file=sys.stderr)
        raise MetricsExceeded("; ".join(failures), code="metrics_exceeded")
    return EXIT_OK




def _cmd_scan(args: argparse.Namespace) -> int:
    from .bitstream import scan, chunk_bytes

    path = Path(args.input)
    if not path.exists():
        raise MediaInputError(f"input not found: {path}", code="input_missing")
    data = path.read_bytes()
    units = []
    report = None
    for item in scan(chunk_bytes(data, 65_536), codec=args.codec):
        if hasattr(item, "units"):
            report = item
        else:
            units.append(item)
    payload = {
        "codec": args.codec,
        "bytes": len(data),
        "units": len(units),
        "keyframes": sum(1 for u in units if u.keyframe),
        "truncated_tail_offset": report.truncated_tail_offset if report else None,
        "issues": [i.to_dict() for i in (report.issues if report else [])],
    }
    if args.json:
        print(json.dumps(payload, indent=2, sort_keys=True))
    else:
        print(f"codec={payload['codec']} bytes={payload['bytes']} "
              f"units={payload['units']} keyframes={payload['keyframes']}")
        for issue in payload["issues"]:
            print(f"issue code={issue['code']} offset={issue['offset']} "
                  f"message={issue['message']}")
    return EXIT_OK




def _cmd_verify(args: argparse.Namespace) -> int:
    raise NotImplementedError("verify command is added by the next patch")


def _read_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise MediaInputError(f"cannot read JSON from {path}: {exc}",
                             code="json_invalid") from exc


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command in ("demo", "resume"):
            return _cmd_demo(args, resume_only=args.command == "resume")
        if args.command == "scan":
            return _cmd_scan(args)
        if args.command == "verify":
            return _cmd_verify(args)
        parser.error(f"unknown command {args.command!r}")
    except Interrupted as exc:
        print(f"interrupted: {exc.message} (re-run to resume)", file=sys.stderr)
        return EXIT_INTERRUPTED
    except MetricsExceeded as exc:
        print(f"metrics_exceeded: {exc.message}", file=sys.stderr)
        return EXIT_METRICS
    except CheckpointConflict as exc:
        print(f"checkpoint_conflict ({exc.reason}): {exc.message}", file=sys.stderr)
        return EXIT_INPUT
    except MediaError as exc:
        location = f" at offset {exc.offset}" if exc.offset is not None else ""
        print(f"error ({exc.code}){location}: {exc.message}", file=sys.stderr)
        return exc.exit_code
    except KeyboardInterrupt:
        print("interrupted by user", file=sys.stderr)
        return EXIT_INTERRUPTED
    return EXIT_OK


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
