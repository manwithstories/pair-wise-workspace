"""Terminal entry points: probe, remux and verify."""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

from mkvmp4.ebml.reader import EBMLDecodeError
from mkvmp4.ebml.vint import VINTError
from mkvmp4.probe import probe_dict, probe_file
from mkvmp4.remux import RemuxError, RepairLimitError, UnsupportedCodecError, remux_file
from mkvmp4.verify import VerifyError, verify_file

EXIT_OK = 0
EXIT_BAD_INPUT = 2
EXIT_UNSUPPORTED = 3
EXIT_REPAIR_LIMIT = 4
EXIT_VERIFY = 5


def _json_out(payload: dict[str, object], path: str | None) -> None:
    text = json.dumps(payload, ensure_ascii=False, indent=2)
    if path:
        Path(path).parent.mkdir(parents=True, exist_ok=True)
        Path(path).write_text(text + "\n", encoding="utf-8")
    else:
        print(text)


def _probe(args: argparse.Namespace) -> int:
    if args.max_depth < 0:
        raise ValueError("--max-depth must be non-negative")
    result = probe_file(args.input, max_depth=args.max_depth)
    payload = probe_dict(result, args.input, args.max_depth)
    _json_out(payload, args.json)
    print(f"elements: {result.elements}")
    print(f"unknown_size: {result.unknown_size}")
    print(f"clusters: {result.clusters}")
    print(f"digest: {result.digest[:24]}")
    return EXIT_OK


def _remux(args: argparse.Namespace) -> int:
    if not 0.0 <= args.max_repair_ratio <= 1.0:
        raise ValueError("--max-repair-ratio must be between 0 and 1")
    manifest = remux_file(args.input, args.output, manifest_path=args.manifest,
                          repair=args.repair, max_repair_ratio=args.max_repair_ratio)
    for track in manifest["tracks"]:  # type: ignore[index]
        name = f" {track['name']}" if track.get("name") else ""
        unit = " 条" if track["codec"] == "tx3g" else " samples"
        print(f"track {track['track_ID']} {track['codec']}{name} {track['samples']}{unit} "
              f"{track['first_pts']:.6f}->{track['last_pts']:.6f}")
    repairs = manifest["repairs"]
    print(f"repairs: duplicate={repairs['duplicate']} backward={repairs['backward']} "
          f"gap={repairs['gap']} ratio={repairs['ratio']:.6f}")
    print(f"cues_rebuilt: {manifest['cues_rebuilt']}")
    print(f"mdat_bytes: {manifest['mdat_bytes']}")
    print(f"digest: {str(manifest['digest'])[:24]}")
    return EXIT_OK


def _verify(args: argparse.Namespace) -> int:
    report = verify_file(args.input, args.against)
    if args.json:
        _json_out(report, args.json)
    for error in report["errors"]:
        print(f"error: {error}", file=sys.stderr)
    print(f"ok: {str(report['ok']).lower()}")
    print(f"moov_before_mdat: {str(report['moov_before_mdat']).lower()}")
    print(f"samples_match: {str(report['samples_match']).lower()}")
    return EXIT_OK if report["ok"] else EXIT_VERIFY


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mkvmp4", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    probe = sub.add_parser("probe", help="print a depth-limited EBML element summary")
    probe.add_argument("input")
    probe.add_argument("--max-depth", type=int, default=4)
    probe.add_argument("--json")
    probe.set_defaults(handler=_probe)
    remux = sub.add_parser("remux", help="stream Matroska into faststart MP4")
    remux.add_argument("input")
    remux.add_argument("-o", "--output", required=True)
    remux.add_argument("--manifest")
    remux.add_argument("--repair", choices=("clamp", "resample", "fail"), default="clamp")
    remux.add_argument("--max-repair-ratio", type=float, default=0.05)
    remux.set_defaults(handler=_remux)
    verify = sub.add_parser("verify", help="read MP4 sample tables back")
    verify.add_argument("input")
    verify.add_argument("--against", required=True, help="remux manifest")
    verify.add_argument("--json")
    verify.set_defaults(handler=_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.handler(args)
    except UnsupportedCodecError as exc:
        print(f"unsupported codec: {exc}", file=sys.stderr)
        return EXIT_UNSUPPORTED
    except RepairLimitError as exc:
        print(f"repair limit: {exc}", file=sys.stderr)
        return EXIT_REPAIR_LIMIT
    except VerifyError as exc:
        print(f"verify failed: {exc}", file=sys.stderr)
        return EXIT_VERIFY
    except RemuxError as exc:
        print(f"input error: {exc}", file=sys.stderr)
        return EXIT_BAD_INPUT
    except (EBMLDecodeError, VINTError, OSError, ValueError) as exc:
        print(f"input error: {exc}", file=sys.stderr)
        return EXIT_BAD_INPUT


if __name__ == "__main__":
    raise SystemExit(main())

