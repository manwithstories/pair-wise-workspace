"""Terminal entry point: demo / run / resume / verify subcommands.

Exit codes: 0 = clean pass, 1 = input corrupt (error list generated),
2 = interrupted run, recovery failure or output validation failure.

The JSON output always carries the acceptance metrics:
``error_source_offset``, ``recovery_seconds``, ``reprocessed_chunks``,
``output_sha256`` and ``subtitle_errors``.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from . import fixtures, pipeline
from .errors import MediaBatchError
from .index import CheckpointIndex
from .output import locate_chunk_damage, sha256_streaming


def _emit(obj: dict) -> None:
    print(json.dumps(obj, ensure_ascii=False, indent=2))


def _summary(report: dict) -> dict:
    """Metrics required by acceptance, plus the most useful diagnostics."""
    errors = report.get("errors", [])
    first = errors[0] if errors else None
    return {
        "status": report.get("status", "ok"),
        "error_source_offset": first["source_offset"] if first else -1,
        "recovery_seconds": report.get("recovery_seconds", 0.0),
        "reprocessed_chunks": report.get("reprocessed_chunks", []),
        "output_sha256": report.get("output_sha256", ""),
        "subtitle_errors": report.get("subtitle_errors", []),
        "chunks_committed": report.get("chunks_committed", 0),
        "duplicates": report.get("duplicates", {}),
        "silence_ms": report.get("silence_ms", []),
        "errors": errors,
    }


def _cmd_demo(args) -> int:
    workdir = Path(args.workdir)
    info = fixtures.generate(workdir)
    try:
        report = pipeline.run(workdir, interrupt_after=args.interrupt_after)
    except pipeline.PipelineInterrupted as exc:
        _emit({"status": "interrupted", "committed_chunks": exc.committed,
               "workdir": str(workdir),
               "next": f"resume --workdir {workdir} --json"})
        return 2
    out = {"status": "ok", "fixtures": info, **_summary(report)}
    _emit(out)
    return 1 if out["errors"] else 0


def _cmd_run(args) -> int:
    try:
        report = pipeline.run(args.workdir,
                              interrupt_after=args.interrupt_after)
    except pipeline.PipelineInterrupted as exc:
        _emit({"status": "interrupted", "committed_chunks": exc.committed})
        return 2
    out = _summary(report)
    _emit(out)
    return 1 if out["errors"] else 0


def _cmd_resume(args) -> int:
    report = pipeline.run(args.workdir, resume=True)
    out = _summary(report)
    if not args.json:
        for key in ("status", "recovery_seconds", "reprocessed_chunks",
                    "chunks_committed", "error_source_offset",
                    "output_sha256"):
            print(f"{key}: {out[key]}")
        return 0
    _emit(out)
    return 0


def _wav_data_offset(path: Path) -> int:
    """Byte offset of the PCM payload inside a RIFF/WAV file."""
    with open(path, "rb") as fh:
        header = fh.read(12)
        if len(header) < 12 or header[:4] != b"RIFF":
            return -1
        while True:
            chunk = fh.read(8)
            if len(chunk) < 8:
                return -1
            fourcc = chunk[:4]
            size = int.from_bytes(chunk[4:], "little")
            if fourcc == b"data":
                return fh.tell()
            fh.seek(size + (size & 1), 1)


def _cmd_verify(args) -> int:
    workdir = Path(args.workdir)
    final = workdir / "output" / "program.out.wav"
    if not final.exists():
        raise MediaBatchError("io_error", f"output not found: {final}",
                              str(final), 0)
    if args.corrupt_at is not None:
        if args.corrupt_at >= final.stat().st_size:
            raise MediaBatchError("io_error", "corrupt offset beyond end of file",
                                  str(final), args.corrupt_at)
        with open(final, "r+b") as fh:
            fh.seek(args.corrupt_at)
            byte = fh.read(1)
            fh.seek(args.corrupt_at)
            fh.write(bytes([byte[0] ^ 0xFF]))
    try:
        return _verify_output(workdir, final, args.corrupt_at is not None)
    finally:
        if args.corrupt_at is not None:
            # restore the flipped byte so verify stays non-destructive
            with open(final, "r+b") as fh:
                fh.seek(args.corrupt_at)
                byte = fh.read(1)
                fh.seek(args.corrupt_at)
                fh.write(bytes([byte[0] ^ 0xFF]))


def _verify_output(workdir: Path, final: Path, corrupted: bool) -> int:
    index = CheckpointIndex(workdir / "work" / "index.db")
    expected = index.get_meta("output_sha256")
    records = index.committed_chunks()
    index.close()
    actual = sha256_streaming(final)
    if actual == expected:
        _emit({"status": "ok", "output_sha256": actual,
               "error_source_offset": -1})
        return 0
    data_offset = _wav_data_offset(final)
    chunk, offset = locate_chunk_damage(
        records, final, workdir / "work" / "output.pcm", data_offset)
    _emit({"status": "output_mismatch", "code": "output_mismatch",
           "source": str(final), "error_source_offset": offset,
           "chunk_seq": chunk.seq if chunk else -1,
           "expected_sha256": expected, "actual_sha256": actual})
    return 2


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="media_batch",
                                description="batch media segmentation with recovery")
    sub = p.add_subparsers(dest="command", required=True)
    d = sub.add_parser("demo", help="generate fixtures and run the pipeline")
    d.add_argument("--workdir", required=True)
    d.add_argument("--interrupt-after", type=int, default=None)
    d.set_defaults(func=_cmd_demo)
    r = sub.add_parser("run", help="run the pipeline on an existing workdir")
    r.add_argument("--workdir", required=True)
    r.add_argument("--interrupt-after", type=int, default=None)
    r.set_defaults(func=_cmd_run)
    s = sub.add_parser("resume", help="resume from the last valid checkpoint")
    s.add_argument("--workdir", required=True)
    s.add_argument("--json", action="store_true")
    s.set_defaults(func=_cmd_resume)
    v = sub.add_parser("verify", help="validate the final output")
    v.add_argument("--workdir", required=True)
    v.add_argument("--corrupt-at", type=int, default=None,
                   help="flip one byte at this offset before validating (demo)")
    v.set_defaults(func=_cmd_verify)
    return p


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except MediaBatchError as exc:
        print(json.dumps({"status": "error", **exc.error.to_dict()},
                         ensure_ascii=False), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
