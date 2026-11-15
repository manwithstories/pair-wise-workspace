"""Command line interface: gen (fixtures) and verify (validation)."""
from __future__ import annotations

import argparse
import glob as globmod
import sys
from pathlib import Path

from .fixtures import CORRUPT_KINDS, FixtureParams, write_fixture
from .reader import StructureError
from .report import format_report, format_summary, write_json
from .validate import validate_file

EXIT_OK = 0
EXIT_ARGS = 2
EXIT_STRUCTURE = 3
EXIT_MISMATCH = 4


def _resolve_within_cwd(path_str: str) -> Path:
    """Resolve a user path and refuse anything outside the workspace."""
    path = Path(path_str).resolve()
    cwd = Path.cwd().resolve()
    if path != cwd and cwd not in path.parents:
        raise SystemExit(f"error: path escapes workspace: {path_str}")
    return path


def _cmd_gen(args: argparse.Namespace) -> int:
    out = _resolve_within_cwd(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    params = FixtureParams(samples=args.samples, fragments=args.fragments,
                           seed=args.seed, corrupt=args.corrupt)
    try:
        write_fixture(out, params)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_ARGS
    corrupt = f" corrupt={params.corrupt}" if params.corrupt else ""
    print(f"wrote {out} samples={params.samples}"
          f" fragments={params.fragments} seed={params.seed}{corrupt}")
    return EXIT_OK


def _expand_inputs(inputs: list[str]) -> list[Path]:
    """Expand files, directories and glob patterns into a sorted file list."""
    files: list[Path] = []
    for item in inputs:
        path = Path(item)
        if path.is_dir():
            files.extend(sorted(path.glob("*.mp4")))
        elif any(ch in item for ch in "*?["):
            files.extend(sorted(Path(p) for p in globmod.glob(item)))
        elif path.is_file():
            files.append(path)
        else:
            raise SystemExit(f"error: no such file or pattern: {item}")
    return files


def _cmd_verify(args: argparse.Namespace) -> int:
    files = _expand_inputs(args.inputs)
    if not files:
        print("error: no input files matched", file=sys.stderr)
        return EXIT_ARGS
    json_target = Path(args.json).resolve() if args.json else None
    if json_target and len(files) > 1:
        json_target.mkdir(parents=True, exist_ok=True)
    reports = []
    errors: list[tuple[str, str]] = []
    worst = EXIT_OK
    for path in files:
        try:
            report = validate_file(path)
        except StructureError as exc:
            print(f"file={path}\n  structure_error={exc}\n  verdict=ERROR")
            errors.append((str(path), str(exc)))
            worst = max(worst, EXIT_STRUCTURE)
            continue
        reports.append(report)
        print(format_report(report))
        if json_target:
            target = (json_target / f"{path.stem}.json"
                      if len(files) > 1 else json_target)
            target.parent.mkdir(parents=True, exist_ok=True)
            write_json(report, target)
        if report.verdict != "PASS":
            worst = max(worst, EXIT_MISMATCH)
    if len(files) > 1:
        print(format_summary(reports, errors))
    return worst


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="isobmff",
        description="Offline fMP4 fragment index rebuild and validation")
    sub = parser.add_subparsers(dest="command", required=True)
    gen = sub.add_parser("gen", help="generate a synthetic fMP4 fixture")
    gen.add_argument("--out", required=True, help="output .mp4 path")
    gen.add_argument("--samples", type=int, default=240)
    gen.add_argument("--fragments", type=int, default=6)
    gen.add_argument("--seed", type=int, default=7)
    gen.add_argument("--corrupt", choices=CORRUPT_KINDS, default=None)
    gen.set_defaults(func=_cmd_gen)
    ver = sub.add_parser("verify", help="rebuild and validate fragment index")
    ver.add_argument("inputs", nargs="+",
                     help="files, directories or glob patterns")
    ver.add_argument("--json", default=None,
                     help="JSON output file (single) or directory (batch)")
    ver.set_defaults(func=_cmd_verify)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
