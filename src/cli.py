"""Command line interface: ``inspect``, ``remux``, ``verify``.

Exit codes are part of the contract:

===== ==========================================================
0     every requested operation succeeded
2     input or argument error (bad path, bad flag, unreadable JSON)
3     EBML or MP4 structure is corrupt
4     timestamps or the sample index cannot be reconstructed
5     the written output failed verification
===== ==========================================================

Output is JSON with ``--json`` and a compact terminal summary otherwise. Both
forms carry the same numbers; only the presentation differs.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Sequence

from . import __version__
from ._cli_args import build_parser, resolve_input, strictness_from
from ._cli_render import render
from ._inspect import inspect_document
from .ebml_reader import ReaderError, ReaderOptions, parse_bytes
from .ebml_schema import SchemaError, Strictness
from .fixture_builder import demo_spec, fixture_sha256, write_fixture
from .matroska_model import MatroskaDocument, ModelError, build_document
from .remux import RemuxError, RemuxOptions, RemuxResult, remux
from .timestamp import TimingConfig
from .verify import verify_output

EXIT_OK = 0
EXIT_INPUT = 2
EXIT_STRUCTURE = 3
EXIT_TIMESTAMP = 4
EXIT_VERIFY = 5

def _keep_ids() -> frozenset[bytes]:
    from . import ebml_schema as ids

    return frozenset({ids.SIMPLE_BLOCK, ids.BLOCK})


class CliError(Exception):
    """A failure already classified by exit code."""

    def __init__(self, code: str, message: str, exit_code: int, details: dict | None = None) -> None:
        self.code = code
        self.exit_code = exit_code
        self.details = details or {}
        super().__init__(message)


def _envelope(command: str, *, data=None, error: CliError | None = None) -> dict[str, object]:
    return {
        "ok": error is None,
        "command": command,
        "version": __version__,
        "data": data,
        "error": None
        if error is None
        else {"code": error.code, "message": str(error), "details": error.details},
    }


# ── document loading ──────────────────────────────────────────────────────


def load_document(path: Path, *, strictness: Strictness = Strictness.STRICT) -> tuple[MatroskaDocument, list[dict]]:
    """Parse ``path`` into a document, translating failures into CLI errors."""
    if not path.exists():
        raise CliError("input_not_found", f"{path} does not exist", EXIT_INPUT)
    if not path.is_file():
        raise CliError("input_not_file", f"{path} is not a regular file", EXIT_INPUT)
    raw = path.read_bytes()
    if not raw:
        raise CliError("input_empty", f"{path} is empty", EXIT_STRUCTURE)
    options = ReaderOptions(
        strictness=strictness,
        keep_payload_ids=_keep_ids(),
    )
    try:
        root, reader = parse_bytes(raw, options)
    except ReaderError as exc:
        raise CliError("ebml_corrupt", str(exc), EXIT_STRUCTURE, exc.as_dict()) from exc
    except SchemaError as exc:
        raise CliError("ebml_corrupt", str(exc), EXIT_STRUCTURE, exc.as_dict()) from exc
    try:
        document = build_document(
            root,
            path=path,
            file_size=len(raw),
            element_count=reader.element_count,
            unknown_length_count=reader.unknown_length_count,
            warnings=reader.warnings,
        )
    except ModelError as exc:
        raise CliError("matroska_corrupt", str(exc), EXIT_STRUCTURE, exc.as_dict()) from exc
    return document, reader.warnings


def _element_tree(document: MatroskaDocument) -> list[dict[str, object]]:
    """A compact structural outline for ``inspect`` (no payload bytes)."""
    rows: list[dict[str, object]] = []
    for cluster in document.clusters:
        rows.append(
            {
                "name": "Cluster",
                "offset": cluster.offset,
                "size": cluster.size,
                "unknown_size": cluster.unknown_size,
                "timecode": cluster.timecode,
                "blocks": len(cluster.blocks),
            }
        )
    return rows


# ── commands ──────────────────────────────────────────────────────────────


def cmd_inspect(args: argparse.Namespace) -> tuple[int, dict[str, object]]:
    path = resolve_input(args)
    document, parse_warnings = load_document(path, strictness=strictness_from(args))
    payload = inspect_document(document, parse_warnings)
    payload["input"] = str(path)
    payload["input_sha256"] = fixture_sha256(path.read_bytes())
    # A document that parses but whose timing cannot be planned is a remux
    # blocker, so `inspect` reports it with the timestamp exit code.
    code = EXIT_TIMESTAMP if payload.get("timestamp_error") else EXIT_OK
    return code, payload


def cmd_remux(args: argparse.Namespace) -> tuple[int, dict[str, object]]:
    path = resolve_input(args)
    output = Path(args.output)
    lenient = strictness_from(args) is Strictness.LENIENT
    document, _ = load_document(path, strictness=strictness_from(args))
    # Strictness governs both the EBML read and the remux policy: by default an
    # untranslatable codec stops the write; lenient mode records it and
    # continues with the tracks it can carry.
    strict_codecs = lenient is False and not args.allow_unsupported_codecs
    options = RemuxOptions(
        timing=TimingConfig(
            normalize_origin=not args.no_normalize,
            repair_duplicates=not args.strict_timestamps,
            force_monotonic_pts=args.force_monotonic_pts,
        ),
        include_subtitles=not args.drop_subtitles,
        strict_codecs=strict_codecs,
        chunk_samples=max(1, args.chunk_samples),
    )
    try:
        result: RemuxResult = remux(document, path, output, options=options)
    except RemuxError as exc:
        exit_code = EXIT_TIMESTAMP if exc.code in {
            "timestamp_rewrite_failed",
            "timestamp_index_failed",
        } else EXIT_STRUCTURE
        raise CliError(exc.code, str(exc), exit_code, exc.details) from exc
    payload = result.as_dict()
    payload["input"] = str(path)
    if args.verify:
        report = verify_output(result.output_path, expected_sha256=result.sha256, document=document)
        payload["verification"] = report.as_dict()
        if not report.ok:
            return EXIT_VERIFY, payload
    if lenient:
        payload["warning_count"] = len(result.warnings)
    return EXIT_OK, payload


def cmd_verify(args: argparse.Namespace) -> tuple[int, dict[str, object]]:
    path = Path(args.input)
    if not path.exists():
        raise CliError("input_not_found", f"{path} does not exist", EXIT_INPUT)
    document = None
    if args.source:
        document, _ = load_document(Path(args.source), strictness=strictness_from(args))
    report = verify_output(
        path,
        expected_sha256=args.expect_sha256,
        document=document,
    )
    payload = report.as_dict()
    return (EXIT_OK if report.ok else EXIT_VERIFY), payload


# ── output ────────────────────────────────────────────────────────────────


def _emit(command: str, code: int, envelope: dict[str, object], *, as_json: bool) -> None:
    if as_json:
        print(json.dumps(envelope, indent=2, sort_keys=True))
    elif envelope["ok"]:
        print(render(command, envelope["data"] or {}))
    else:
        error = envelope["error"]
        print(f"{command} failed: {error['code']}: {error['message']}", file=sys.stderr)
    sys.exit(code)


# ── parser ────────────────────────────────────────────────────────────────


# ── entry point ───────────────────────────────────────────────────────────


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    # argparse exits 2 on bad arguments, which matches our input-error code.
    args = parser.parse_args(argv)
    command = args.command
    handler = {"inspect": cmd_inspect, "remux": cmd_remux, "verify": cmd_verify}[command]
    try:
        code, payload = handler(args)
    except CliError as exc:
        _emit(command, exc.exit_code, _envelope(command, error=exc), as_json=args.json)
        return exc.exit_code
    except (OSError, ValueError) as exc:
        error = CliError("internal_error", str(exc), EXIT_STRUCTURE)
        _emit(command, EXIT_STRUCTURE, _envelope(command, error=error), as_json=args.json)
        return EXIT_STRUCTURE
    envelope = _envelope(command, data=payload)
    if getattr(args, "json_out", None):
        try:
            target = Path(args.json_out)
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(json.dumps(envelope, indent=2, sort_keys=True))
        except OSError as exc:
            error = CliError("json_out_failed", f"cannot write {args.json_out}: {exc}", EXIT_INPUT)
            _emit(command, EXIT_INPUT, _envelope(command, error=error), as_json=args.json)
            return EXIT_INPUT
    _emit(command, code, envelope, as_json=args.json)
    return code


if __name__ == "__main__":  # pragma: no cover - exercised via subprocess
    raise SystemExit(main())
