"""mediaqa command line interface: sniff / pack / verify / query."""

import argparse
import json
import os
import sys

from . import consistency, extract, packager, report
from .store import Store


def _workspace_root():
    return os.path.realpath(os.getcwd())


def _safe_path(path, base=None):
    full = os.path.realpath(os.path.join(base or _workspace_root(), path))
    root = _workspace_root()
    if full != root and not full.startswith(root + os.sep):
        raise ValueError("path escapes workspace: %s" % path)
    return full


def _load_spec(spec_path):
    full = _safe_path(spec_path)
    with open(full) as fh:
        return json.load(fh), os.path.dirname(full)


def _select_tracks(spec, spec_dir):
    tracks, results = [], []
    for src in spec.get("sources", []):
        path = _safe_path(src["path"], spec_dir)
        result = extract.extract_file(path)
        results.append(result)
        if not result.ok:
            continue
        indexes = src.get("tracks", list(range(len(result.tracks))))
        tracks.extend(result.tracks[i] for i in indexes)
    return tracks, results


def cmd_sniff(args):
    files = extract.collect([_safe_path(p) for p in args.paths])
    if not files:
        raise ValueError("no media files found")
    results = [extract.extract_file(p) for p in files]
    bad = [r for r in results if not r.ok]
    findings = [consistency.Finding(
        "CONTAINER_PARSE", "error", "parseable container", r.error,
        None, r.path, "container parse failed") for r in bad]
    track_count = sum(len(r.tracks) for r in results)
    code = report.exit_code_for(findings, parse_failed=bool(bad))
    doc = {
        "command": "sniff", "files": len(results), "tracks": track_count,
        "bad": len(bad),
        "files_detail": [{
            "path": r.path, "container": r.container or None,
            "track_count": len(r.tracks), "duration_us": r.duration_us,
            "codecs": r.codecs(), "error": r.error,
        } for r in results],
        "exit_code": code,
    }
    print("files=%d tracks=%d bad=%d" % (len(results), track_count, len(bad)))
    for r in bad:
        print("  [error] CONTAINER_PARSE file=%s actual=%s" % (r.path, r.error))
    if args.out:
        report.write_json(doc, _safe_path(args.out))
    if args.db:
        Store(_safe_path(args.db)).record_run(
            "sniff", " ".join(args.paths), code, results, findings)
    return code


def cmd_verify(args):
    spec, spec_dir = _load_spec(args.spec)
    tracks, results = _select_tracks(spec, spec_dir)
    parse_failed = [r for r in results if not r.ok]
    findings, metrics = consistency.evaluate(tracks, results)
    findings.extend(consistency.Finding(
        "CONTAINER_PARSE", "error", "parseable container", r.error,
        None, r.path, "container parse failed") for r in parse_failed)
    code = report.exit_code_for(findings, parse_failed=bool(parse_failed))
    tallies = report.counts(findings)
    doc = {
        "command": "verify", "spec": args.spec,
        "track_count": metrics["track_count"],
        "max_duration_delta_us": metrics["max_duration_delta_us"],
        "start_delta_us": metrics["start_delta_us"],
        "finding_count": len(findings),
        "errors": tallies["error"], "warnings": tallies["warning"],
        "findings": [f.to_dict() for f in report.sort_findings(findings)],
        "exit_code": code,
    }
    print("track_count=%d max_duration_delta_us=%d finding_count=%d"
          " (errors=%d warnings=%d)" % (
              metrics["track_count"], metrics["max_duration_delta_us"],
              len(findings), tallies["error"], tallies["warning"]))
    report.print_findings(findings)
    if args.out:
        report.write_json(doc, _safe_path(args.out))
    if args.db:
        Store(_safe_path(args.db)).record_run(
            "verify", args.spec, code, results, findings)
    return code


def cmd_pack(args):
    spec, spec_dir = _load_spec(args.spec)
    out_dir = _safe_path(args.out)
    try:
        result = packager.build_package(spec, spec_dir, out_dir)
    except packager.PackRejected as exc:
        print("[error] %s %s" % (packager.PackRejected.rule_id, exc))
        return report.EXIT_PACK_REJECTED
    except packager.RoundtripMismatch as exc:
        print("[error] %s %s" % (packager.RoundtripMismatch.rule_id, exc))
        return report.EXIT_ERROR
    print("roundtrip=%s tracks=%d duration_delta_us=%d" % (
        result["roundtrip"], result["tracks"], result["duration_delta_us"]))
    if args.verify and (result["roundtrip"] != "ok"
                        or result["duration_delta_us"] != 0):
        print("[error] ROUNDTRIP_MISMATCH verification failed")
        return report.EXIT_ERROR
    if args.db:
        back = [extract.extract_file(os.path.join(out_dir, name))
                for name in result["files"]]
        Store(_safe_path(args.db)).record_run(
            "pack", args.spec, report.EXIT_OK, back, (),
            {"sha256": result["sha256"], "path": out_dir})
    return report.EXIT_OK


def cmd_query(args):
    store = Store(_safe_path(args.db))
    rows, total = store.query(
        severity=args.severity, rule_id=args.rule_id,
        container=args.container, since=args.since, until=args.until,
        limit=args.limit)
    for row in rows:
        print("id=%d at=%s [%s] %s file=%s track=%s expected=%s actual=%s"
              % row)
    print("total=%d" % total)
    return report.EXIT_OK


def build_parser():
    parser = argparse.ArgumentParser(
        prog="mediaqa", description="museum delivery media QA toolkit")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("sniff", help="extract track metadata from media files")
    p.add_argument("paths", nargs="+")
    p.add_argument("--out")
    p.add_argument("--db")
    p.set_defaults(func=cmd_sniff)

    p = sub.add_parser("verify", help="check a delivery spec for consistency")
    p.add_argument("spec")
    p.add_argument("--db")
    p.add_argument("--out")
    p.set_defaults(func=cmd_verify)

    p = sub.add_parser("pack", help="assemble a delivery package")
    p.add_argument("spec")
    p.add_argument("--out", required=True)
    p.add_argument("--verify", action="store_true")
    p.add_argument("--db")
    p.set_defaults(func=cmd_pack)

    p = sub.add_parser("query", help="query the findings index")
    p.add_argument("--db", required=True)
    p.add_argument("--severity", choices=["error", "warning"])
    p.add_argument("--rule-id", dest="rule_id")
    p.add_argument("--container")
    p.add_argument("--since")
    p.add_argument("--until")
    p.add_argument("--limit", type=int, default=20)
    p.set_defaults(func=cmd_query)
    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except (OSError, ValueError, KeyError, IndexError,
            json.JSONDecodeError) as exc:
        print("error: %s" % exc, file=sys.stderr)
        return report.EXIT_USAGE


if __name__ == "__main__":
    sys.exit(main())
