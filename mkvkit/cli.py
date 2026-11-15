"""mkvkit command line interface: inspect / remux / verify.

Exit codes: 0 success, 2 not Matroska or truncated EBML, 3 unsupported
codec, 4 unrecoverable timestamps, 5 write self-check failure.
"""
from __future__ import annotations

import argparse
import json
import os
import sys

from .ebml.reader import EbmlReader, EbmlError
from .ebml.tree import Tree, summarize, export_json
from .mkv.demux import demux, validate_matroska, UnsupportedCodecError
from .timing.rewrite import TimestampError
from .isobmff.mux import remux, SelfCheckError
from .isobmff.verify import verify_mp4

EXIT_OK = 0
EXIT_FAILED = 1
EXIT_NOT_MKV = 2
EXIT_CODEC = 3
EXIT_TIMESTAMPS = 4
EXIT_SELFCHECK = 5


def _cmd_inspect(args) -> int:
    with EbmlReader(args.input, tolerant=True) as reader:
        roots = reader.parse()
        validate_matroska(reader, roots)
        tree = Tree(roots, list(reader.truncations))
    summary = summarize(tree)
    if args.json:
        export_json(tree, args.depth, args.json)
    print(f"file: {args.input}")
    for key in (
        "elements_total", "unknown_size_elements", "max_depth",
        "clusters", "blocks", "truncated_elements",
    ):
        print(f"{key}: {summary[key]}")
    return EXIT_OK


def _cmd_remux(args) -> int:
    protected = [os.path.realpath(args.input)]
    destinations = [os.path.realpath(args.output)]
    if args.report:
        destinations.append(os.path.realpath(args.report))
    if len(set(destinations)) != len(destinations):
        print("error: output and report paths must differ", file=sys.stderr)
        return EXIT_FAILED
    if any(path in protected for path in destinations):
        print("error: output must not overwrite the input file", file=sys.stderr)
        return EXIT_FAILED
    report = remux(args.input, args.output)
    if args.report:
        parent = os.path.dirname(os.fspath(args.report))
        if parent:
            os.makedirs(parent, exist_ok=True)
        with open(args.report, "w", encoding="utf-8") as fh:
            json.dump(report, fh, indent=1)
    header = f"{'track':>5}  {'codec':<6} {'timescale':>9} {'frames':>7} {'first_pts_ns':>12} {'last_pts_ns':>12}"
    print(header)
    for t in report["tracks"]:
        print(
            f"{t['track']:>5}  {t['codec']:<6} {t['timescale']:>9} "
            f"{t['frames']:>7} {t['first_pts_ns']:>12} {t['last_pts_ns']:>12}"
        )
    print(
        f"av_start_delta_ms: {report['av_start_delta_ms']}  "
        f"co64_used: {str(report['co64_used']).lower()}  "
        f"issues: {report['issues']}"
    )
    return EXIT_OK


def _cmd_verify(args) -> int:
    result = verify_mp4(args.input)
    if args.json:
        print(json.dumps(result, indent=1))
    else:
        for key, value in result.items():
            print(f"{key}: {value}")
    return EXIT_OK if result["ok"] else EXIT_SELFCHECK


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mkvkit", description="Matroska to MP4 remuxing toolkit"
    )
    sub = parser.add_subparsers(dest="command", required=True)

    p_inspect = sub.add_parser("inspect", help="dump EBML element summary")
    p_inspect.add_argument("input")
    p_inspect.add_argument("--depth", type=int, default=4)
    p_inspect.add_argument("--json", metavar="PATH")
    p_inspect.set_defaults(func=_cmd_inspect)

    p_remux = sub.add_parser("remux", help="remux Matroska to MP4")
    p_remux.add_argument("input")
    p_remux.add_argument("-o", "--output", required=True)
    p_remux.add_argument("--report", metavar="PATH")
    p_remux.set_defaults(func=_cmd_remux)

    p_verify = sub.add_parser("verify", help="verify a written MP4")
    p_verify.add_argument("input")
    p_verify.add_argument("--json", action="store_true")
    p_verify.set_defaults(func=_cmd_verify)
    return parser


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except UnsupportedCodecError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_CODEC
    except TimestampError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_TIMESTAMPS
    except SelfCheckError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_SELFCHECK
    except EbmlError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_NOT_MKV
    except OSError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_NOT_MKV


if __name__ == "__main__":
    sys.exit(main())
