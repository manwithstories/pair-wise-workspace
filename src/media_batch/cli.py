"""Terminal entry point: demo / run / resume / verify subcommands.

Exit codes: 0 = clean pass, 1 = input corrupt (error list generated),
2 = interrupted run, recovery failure or output validation failure.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path

from . import fixtures, pipeline
from .errors import MediaBatchError
from .index import CheckpointIndex


def _emit(obj: dict) -> None:
    print(json.dumps(obj, ensure_ascii=False, indent=2))


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
    report = {"status": "ok", "fixtures": info, **report}
    _emit(report)
    return 1 if report["errors"] else 0


def _cmd_run(args) -> int:
    try:
        report = pipeline.run(args.workdir,
                              interrupt_after=args.interrupt_after)
    except pipeline.PipelineInterrupted as exc:
        _emit({"status": "interrupted", "committed_chunks": exc.committed})
        return 2
    _emit({"status": "ok", **report})
    return 1 if report["errors"] else 0


def _cmd_resume(args) -> int:
    report = pipeline.run(args.workdir, resume=True)
    out = {"status": "ok", **report}
    if args.json:
        _emit(out)
    else:
        _emit({k: out[k] for k in ("status", "recovery_seconds",
                                   "reprocessed_chunks", "chunks_committed",
                                   "output_sha256")})
    return 0


def _wav_data_offset(raw: bytes) -> int:
    """Byte offset of the PCM payload inside a RIFF/WAV file."""
    pos = 12
    while pos + 8 <= len(raw):
        fourcc = raw[pos:pos + 4]
        size = int.from_bytes(raw[pos + 4:pos + 8], "little")
        if fourcc == b"data":
            return pos + 8
        pos += 8 + size + (size & 1)
    return -1


def _cmd_verify(args) -> int:
    workdir = Path(args.workdir)
    final = workdir / "output" / "program.out.wav"
    if args.corrupt_at is not None:
        data = bytearray(final.read_bytes())
        if args.corrupt_at >= len(data):
            raise MediaBatchError("io_error", "corrupt offset beyond end of file",
                                  str(final), args.corrupt_at)
        data[args.corrupt_at] ^= 0xFF
        final.write_bytes(bytes(data))
    index = CheckpointIndex(workdir / "work" / "index.db")
    expected = index.get_meta("output_sha256")
    raw = final.read_bytes()
    actual = hashlib.sha256(raw).hexdigest()
    if actual == expected:
        _emit({"status": "ok", "output_sha256": actual})
        return 0
    # locate the first differing payload byte against the checkpointed PCM
    offset = -1
    pcm_ref = workdir / "work" / "output.pcm"
    data_off = _wav_data_offset(raw)
    if pcm_ref.exists() and data_off >= 0:
        ref = pcm_ref.read_bytes()
        payload = raw[data_off:data_off + len(ref)]
        for i, (a, b) in enumerate(zip(payload, ref)):
            if a != b:
                offset = data_off + i
                break
        if offset < 0 and len(raw) - data_off != len(ref):
            offset = data_off + min(len(payload), len(ref))
    _emit({"status": "output_mismatch", "code": "output_mismatch",
           "source": str(final), "error_source_offset": offset,
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
