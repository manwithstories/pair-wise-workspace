"""Command line interface: ``probe``, ``sample``, ``repair`` and ``verify``.

Every command prints a single machine-readable JSON object to stdout (and a
short human line to stderr), so the same output feeds both a terminal
operator and a batch driver.  Exit codes follow the contract:

===  ============================================================
0    repair and verification succeeded (damage was skipped)
2    input is structurally unrecoverable
3    checkpoint or index does not match the current input/run
4    internal processing failure
===  ============================================================
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from . import probe as probe_mod
from .errors import (
    EXIT_INTERNAL,
    EXIT_OK,
    EXIT_RESUME_MISMATCH,
    EXIT_UNRECOVERABLE,
    MediaBridgeError,
)
from .samplegen import generate
from .timeline import TimelineProfile, load_fraction


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mediabridge",
        description="Repair, index and verify MP4/MOV archives without reading them whole.",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    probe_cmd = sub.add_parser("probe", help="describe a container and its packets")
    probe_cmd.add_argument("source", type=Path)

    sample = sub.add_parser("sample", help="generate deterministic fixtures")
    sample.add_argument("--out", type=Path, required=True)

    repair = sub.add_parser("repair", help="repair timestamps and build a random-access index")
    repair.add_argument("source", type=Path)
    repair.add_argument("--index", dest="index_path", type=Path, required=True)
    repair.add_argument("--checkpoint", dest="checkpoint_path", type=Path, required=True)
    repair.add_argument("--out", dest="output", type=Path, default=None,
                        help="repaired output path (default: <source stem>.repaired.mp4)")
    repair.add_argument("--queue-size", type=int, default=8)
    repair.add_argument("--chunk-packets", type=int, default=128)
    repair.add_argument("--tolerance-seconds", default="5")
    repair.add_argument("--interrupt-after", type=int, default=None,
                        help="stop after this many packets (simulates a crash)")
    repair.add_argument("--consumer-delay", type=float, default=0.0,
                        help="per-packet consumer delay to exercise backpressure")
    repair.add_argument("--resume", action="store_true")
    repair.add_argument("--no-damage-detection", action="store_true",
                        help="skip decoding; trust container flags only")
    repair.add_argument("--json", action="store_true", default=True)

    verify = sub.add_parser("verify", help="check index targets and resume equivalence")
    verify.add_argument("source", type=Path)
    verify.add_argument("--index", dest="index_path", type=Path, required=True)
    verify.add_argument("--output", dest="output", type=Path, default=None)
    verify.add_argument("--samples", type=int, default=12, help="random seek targets")
    verify.add_argument("--checkpoint", dest="checkpoint_path", type=Path, default=None)
    verify.add_argument("--report-json", action="store_true", default=False)
    verify.add_argument("--compare-index", dest="compare_index", type=Path, default=None,
                        help="reference index (e.g. from a full run) to prove equivalence")
    return parser


def _emit(payload: dict, human: str) -> None:
    print(json.dumps(payload, sort_keys=True))
    print(human, file=sys.stderr)


def _exit_code_for(exc: MediaBridgeError) -> int:
    return exc.exit_code


# ---------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------
def cmd_probe(args: argparse.Namespace) -> int:
    result = probe_mod.probe(args.source)
    _emit(result.as_dict(), f"probe: {result.packet_count} packets, "
                            f"{len(result.tracks)} tracks")
    return EXIT_OK


def cmd_sample(args: argparse.Namespace) -> int:
    specs = generate(args.out)
    payload = {
        "command": "sample",
        "out": str(args.out),
        "samples": [spec.as_dict() for spec in specs],
    }
    _emit(payload, f"sample: wrote {len(specs)} fixtures to {args.out}")
    return EXIT_OK


def cmd_repair(args: argparse.Namespace) -> int:
    from .repair import RepairConfig, repair

    output = args.output or args.source.with_name(args.source.stem + ".repaired.mp4")
    profile = TimelineProfile(tolerance_seconds=load_fraction(
        args.tolerance_seconds if "/" in str(args.tolerance_seconds)
        else float(args.tolerance_seconds)
    ))
    config = RepairConfig(
        source=args.source,
        output=output,
        index_path=args.index_path,
        checkpoint_path=args.checkpoint_path,
        queue_size=max(1, args.queue_size),
        chunk_packets=max(1, args.chunk_packets),
        profile=profile,
        resume=args.resume,
        interrupt_after=args.interrupt_after,
        consumer_delay=max(0.0, args.consumer_delay),
        detect_damage=not args.no_damage_detection,
    )
    summary = repair(config)
    _emit(summary.as_dict(), _repair_human(summary))
    return summary.exit_code


def _repair_human(summary) -> str:
    if summary.status == "ok":
        return (f"repair: {summary.packets_written}/{summary.packets_total} packets, "
                f"{summary.chunks_committed} chunks, sha256={summary.output_sha256[:16]}...")
    if summary.status == "interrupted":
        return (f"repair: interrupted after {summary.packets_written} packets "
                f"({summary.chunks_committed} chunks committed)")
    return f"repair: {summary.status} ({summary.error.get('code') if summary.error else 'error'})"


def cmd_verify(args: argparse.Namespace) -> int:
    from .verify import VerifyConfig, verify

    config = VerifyConfig(
        source=args.source,
        index_path=args.index_path,
        output=args.output,
        sample_targets=args.samples,
        checkpoint_path=args.checkpoint_path,
        compare_index=args.compare_index,
    )
    summary = verify(config)
    _emit(summary.as_dict(), _verify_human(summary))
    return summary.exit_code


def _verify_human(summary) -> str:
    if summary.status == "ok":
        return (f"verify: {summary.seek_checks_passed}/{summary.seek_checks} seeks, "
                f"{summary.chunks_checked} chunks, {summary.index_rows} index rows")
    return f"verify: {summary.status} ({summary.error.get('code') if summary.error else 'failed'})"


def main(argv: list[str] | None = None) -> int:
    parser = _build_parser()
    args = parser.parse_args(argv)
    handler = {
        "probe": cmd_probe,
        "sample": cmd_sample,
        "repair": cmd_repair,
        "verify": cmd_verify,
    }[args.command]
    try:
        return handler(args)
    except MediaBridgeError as exc:
        payload = {
            "command": args.command,
            "status": "failed",
            "exit_code": exc.exit_code,
            "error": exc.as_dict(),
        }
        _emit(payload, f"{args.command}: {exc.code}: {exc.message}")
        return exc.exit_code
    except KeyboardInterrupt:
        _emit({"command": args.command, "status": "cancelled", "exit_code": EXIT_INTERNAL},
              f"{args.command}: cancelled")
        return EXIT_INTERNAL
    except Exception as exc:  # never leak a traceback without a JSON line
        payload = {
            "command": args.command,
            "status": "failed",
            "exit_code": EXIT_INTERNAL,
            "error": {"code": "internal_error",
                      "message": f"{type(exc).__name__}: {exc}",
                      "exit_code": EXIT_INTERNAL},
        }
        _emit(payload, f"{args.command}: internal error: {exc}")
        return EXIT_INTERNAL


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
