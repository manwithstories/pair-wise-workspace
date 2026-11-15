"""mp4qc command line: fixtures / sample-table / verify / rebuild."""
from __future__ import annotations

import argparse
import itertools
import sys

from .boxio import BoxError, BoxReader, TruncatedError
from .report import (EXIT_OK, EXIT_UNPARSEABLE, Report, exit_code,
                     print_report, write_json)


def build_parser():
    parser = argparse.ArgumentParser(
        prog="mp4qc",
        description="ISO BMFF (fragmented MP4) index verify & rebuild tool")
    sub = parser.add_subparsers(dest="command", required=True)
    fx = sub.add_parser("fixtures", help="generate sample media fixtures")
    fx.add_argument("--out", required=True, help="output directory")
    fx.add_argument("--seed", type=int, default=7)
    st = sub.add_parser("sample-table", help="expand stbl sample table")
    st.add_argument("file")
    st.add_argument("--track", type=int, default=None)
    st.add_argument("--json", default=None)
    vf = sub.add_parser("verify", help="verify sidx against moof boundaries")
    vf.add_argument("file")
    vf.add_argument("--json", default=None)
    vf.add_argument("--strict", action="store_true",
                    help="treat warnings as errors")
    rb = sub.add_parser("rebuild", help="rebuild sidx from real boundaries")
    rb.add_argument("file")
    rb.add_argument("--out", required=True)
    rb.add_argument("--json", default=None)
    return parser


def cmd_fixtures(args):
    from .fixtures import write_fixtures
    paths = write_fixtures(args.out, seed=args.seed)
    report = Report("fixtures", args.out)
    for path in paths:
        report.add("info", "wrote %s" % path)
    report.data["files"] = paths
    report.data["seed"] = args.seed
    print_report(report)
    return EXIT_OK


def _table_dict(table):
    first_samples = [
        {"index": i, "offset": off, "size": size,
         "duration": table.durations[i] if i < len(table.durations) else None}
        for i, (off, size) in enumerate(
            itertools.islice(table.sample_ranges(), 5))
    ]
    return {
        "track_id": table.track_id,
        "codec": table.codec,
        "sample_count": table.sample_count,
        "stts_entries": table.stts_entries,
        "stsc_entries": table.stsc_entries,
        "stsz_entries": table.stsz_entries,
        "chunk_count": table.chunk_count,
        "sync_samples": (len(table.sync_samples)
                         if table.sync_samples is not None else None),
        "has_ctts": table.ctts_offsets is not None,
        "first_samples": first_samples,
    }


def cmd_sample_table(args):
    from .stbl import iter_traks, parse_sample_table
    report = Report("sample-table", args.file)
    with BoxReader(args.file) as reader:
        moov = next((b for b in reader.boxes() if b.type == "moov"), None)
        if moov is None:
            report.add("error", "no moov box found")
            return report
        tracks = []
        for trak, track_id in iter_traks(reader, moov):
            if args.track is not None and track_id != args.track:
                continue
            table = parse_sample_table(reader, trak, track_id)
            for warning in table.warnings:
                report.add("warn", "track %s: %s" % (track_id, warning))
            tracks.append(_table_dict(table))
    if args.track is not None:
        if not tracks:
            report.add("error", "track %d not found" % args.track)
        else:
            report.data.update(tracks[0])
    else:
        report.data["track_count"] = len(tracks)
        report.data["tracks"] = tracks
    return report


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        if args.command == "fixtures":
            return cmd_fixtures(args)
        if args.command == "sample-table":
            report = cmd_sample_table(args)
            code = exit_code(report)
        elif args.command == "verify":
            from .verify import verify_file
            report = verify_file(args.file, strict=args.strict)
            code = exit_code(report, strict=args.strict)
        elif args.command == "rebuild":
            from .rebuild import rebuild_file
            report, code = rebuild_file(args.file, args.out)
        else:  # pragma: no cover
            return 2
        print_report(report)
        if getattr(args, "json", None):
            write_json(report, args.json)
        return code
    except TruncatedError as exc:
        print("[ERROR] %s" % exc, file=sys.stderr)
        return EXIT_UNPARSEABLE
    except BoxError as exc:
        print("[ERROR] cannot parse: %s" % exc, file=sys.stderr)
        return EXIT_UNPARSEABLE
    except OSError as exc:
        print("[ERROR] %s" % exc, file=sys.stderr)
        return EXIT_UNPARSEABLE


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())

