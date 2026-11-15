"""Command line interface: probe / remux / verify.

Exit codes: 0 ok, 2 argument error, 3 EBML corrupt, 4 unsupported
codec, 5 timestamps not expressible, 6 verification mismatch.
"""

import argparse
import json
import sys

from . import report as report_mod
from .errors import Mkv2Mp4Error
from .mkv.document import MkvDocument
from .mp4 import muxer


def _write_json(payload, path):
    if path:
        with open(path, "w", encoding="utf-8") as fh:
            json.dump(payload, fh, indent=2, ensure_ascii=False)
            fh.write("\n")


def cmd_probe(args):
    doc = MkvDocument.parse(args.input, light=True)
    report = report_mod.probe_report(doc)
    _write_json(report, args.json)
    print("file: %s (%d bytes)" % (report["file"], report["size_bytes"]))
    print("elements: %d  max_depth: %d  unknown_sized: %d" % (
        report["element_count"], report["max_depth"],
        len(report["unknown_sized"])))
    for item in report["unknown_sized"]:
        print("  unknown-length %s %s @ 0x%x"
              % (item["id"], item["name"], item["offset"]))
    print("cues: %s" % report["cues"]["status"])
    for t in report["tracks"]:
        if t["type"] == "video":
            detail = "%dx%d" % (t["width"], t["height"])
        else:
            detail = "%g Hz, %d ch" % (t.get("sample_rate", 0),
                                       t.get("channels", 0))
        print("  track #%d -> track_ID %d [%s] %s lang=%s default=%s %s"
              % (t["track_number"], t["track_ID"], t["type"], t["codec"],
                 t["language"], t["default"], detail))
    return 0


def cmd_remux(args):
    doc = MkvDocument.parse(args.input)
    result = muxer.remux(doc, args.output, layout=args.layout,
                         timescale=args.timescale)
    report = report_mod.remux_report(doc, result)
    _write_json(report, args.json)
    print("remuxed %s -> %s (layout=%s, timescale=%d)" % (
        report["input"], report["output"], args.layout, args.timescale))
    for t in report["tracks"]:
        print("  track_ID %d [%s] samples=%d first_pts=%.3fms "
              "last_pts=%.3fms edit_list=%s" % (
                  t["track_ID"], t["type"], t["sample_count"],
                  t["first_pts_ms"], t["last_pts_ms"], t["edit_list"]))
    print("sample_table_sha256: %s" % report["sample_table_sha256"])
    return 0


def cmd_verify(args):
    with open(args.expect, "r", encoding="utf-8") as fh:
        expect = json.load(fh)
    actual = muxer.inspect(args.input)
    mismatches = muxer.verify(expect, actual)
    for item in mismatches:
        print("mismatch: %s" % item)
    print("mismatches: %d" % len(mismatches))
    return 6 if mismatches else 0


def build_parser():
    parser = argparse.ArgumentParser(
        prog="mkv2mp4",
        description="Pure-Python Matroska -> MP4 remuxer (AVC/AAC).")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("probe", help="stream an element-tree summary")
    p.add_argument("input")
    p.add_argument("--json", metavar="PATH")
    p.set_defaults(func=cmd_probe)

    p = sub.add_parser("remux", help="remux MKV to MP4 (no re-encode)")
    p.add_argument("input")
    p.add_argument("output")
    p.add_argument("--layout", choices=("faststart", "moov-last"),
                   default="faststart")
    p.add_argument("--timescale", type=int, default=90000)
    p.add_argument("--json", metavar="PATH")
    p.set_defaults(func=cmd_remux)

    p = sub.add_parser("verify", help="re-parse a product and check it")
    p.add_argument("input")
    p.add_argument("--expect", required=True, metavar="JSON")
    p.set_defaults(func=cmd_verify)
    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except Mkv2Mp4Error as exc:
        print("error: %s" % exc, file=sys.stderr)
        return exc.exit_code
    except (OSError, ValueError) as exc:
        print("error: %s" % exc, file=sys.stderr)
        return 2
