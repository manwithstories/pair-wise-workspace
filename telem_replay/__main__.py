"""Command line entry point: ``replay`` / ``resume`` / ``stats``."""

from __future__ import annotations

import argparse
import json
import os
import sqlite3
import sys
import time
from typing import List, Optional

from .pipeline import ReplayPipeline


def _fmt_int(n: int) -> str:
    return f"{n:,}"


def _report(stats, elapsed: float, out_path: str, ckpt) -> None:
    overhead = (stats.checkpoint_seconds / elapsed * 100.0) if elapsed > 0 else 0.0
    # A run that had nothing left to do (a no-op resume) reports no throughput
    # rather than dividing by a near-zero wall clock.
    rate = f" [{_fmt_int(int(stats.lines_read / elapsed))} lines/s]" if elapsed > 0.05 else ""
    if getattr(stats, "noop", False):
        print("nothing to do: this input was already replayed to the end")
    else:
        print(
            f"processed  {_fmt_int(stats.lines_read)} lines "
            f"({_fmt_int(stats.frames_committed)} frames) in {elapsed:.2f}s{rate}"
        )
    print(f"corrupt    {_fmt_int(stats.corrupt)}")
    print(f"duplicate  {_fmt_int(stats.duplicate)}")
    print(f"resumed    {_fmt_int(stats.resumed)}")
    if stats.warnings:
        print(f"warnings   {_fmt_int(stats.warnings)} (evicted frames; see stderr)")
    if not getattr(stats, "noop", False):
        # The overhead ratio is meaningless for a run that did no work.
        print(
            f"checkpoints {_fmt_int(stats.checkpoint_writes)} writes, "
            f"{stats.checkpoint_seconds:.3f}s ({overhead:.2f}% of run)"
        )
    print(f"output     {out_path}")


def _warn_digest(path: str, limit: int = 5) -> None:
    if not os.path.exists(path):
        return
    with open(path, "r", encoding="utf-8") as fh:
        for i, line in enumerate(fh):
            if i >= limit:
                print(f"  ... (more in {path})")
                break
            try:
                rec = json.loads(line)
            except ValueError:
                continue
            print(f"  {rec.get('reason', '?')}")


def _add_common(p: argparse.ArgumentParser, *, need_output: bool = True) -> None:
    p.add_argument("input", help="input JSON Lines log")
    p.add_argument("--state", default="state.db", help="SQLite checkpoint file")
    if need_output:
        p.add_argument("--output", default="out.jsonl", help="timeline output")
        p.add_argument("--dlq", default="dlq.jsonl", help="dead letter output")
        p.add_argument("--window", type=int, default=256,
                       help="reassembly window slots per session (default 256)")
        p.add_argument("--max-sessions", type=int, default=64,
                       help="max sessions held in memory (default 64)")
        p.add_argument("--checkpoint-frames", type=int, default=2000,
                       help="commit every N frames (default 2000)")
        p.add_argument("--checkpoint-seconds", type=float, default=30.0,
                       help="commit at most every N seconds (default 30)")
        p.add_argument("--sync", choices=("normal", "full"), default="normal",
                       help="SQLite durability (default normal)")
        p.add_argument("--progress", type=int, default=0, metavar="N",
                       help="print progress every N lines (0 = quiet)")


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="python -m telem_replay",
        description="Offline, resumable replay of SCADA gateway telemetry logs.",
    )
    sub = p.add_subparsers(dest="command", required=True)

    r = sub.add_parser("replay", help="start (or restart) a replay from the beginning")
    _add_common(r)
    r.add_argument("--fresh", action="store_true",
                   help="discard any existing checkpoint and start over")

    s = sub.add_parser("resume", help="continue a replay from the last checkpoint")
    _add_common(s)

    t = sub.add_parser("stats", help="print counters from a checkpoint file")
    t.add_argument("state", help="SQLite checkpoint file")
    t.add_argument("--json", action="store_true", help="emit JSON instead of text")

    return p


def _check_paths(args: argparse.Namespace) -> Optional[int]:
    """Fail fast with a clear message instead of a traceback."""
    if not os.path.exists(args.input):
        print(f"input not found: {args.input}", file=sys.stderr)
        return 2
    for label, path in (("state", args.state), ("output", getattr(args, "output", None)),
                        ("dlq", getattr(args, "dlq", None))):
        if not path:
            continue
        parent = os.path.dirname(os.path.abspath(path))
        if not os.path.isdir(parent):
            print(f"cannot write {label}: directory does not exist: {parent}",
                  file=sys.stderr)
            return 2
    return None


def cmd_replay(args: argparse.Namespace, resume: bool) -> int:
    bad = _check_paths(args)
    if bad is not None:
        return bad
    # --fresh is only offered by the 'replay' subcommand.
    if getattr(args, "fresh", False) and os.path.exists(args.state):
        for suffix in ("", "-wal", "-shm"):
            try:
                os.remove(args.state + suffix)
            except OSError:
                pass

    pipe = ReplayPipeline(
        args.input,
        state_path=args.state,
        output_path=args.output,
        dlq_path=args.dlq,
        window=args.window,
        max_sessions=args.max_sessions,
        resume=resume,
        checkpoint_frames=args.checkpoint_frames,
        checkpoint_seconds=args.checkpoint_seconds,
        sync=args.sync,
        progress_every=args.progress,
    )
    pipe.ckpt.record_input(args.input)

    t0 = time.time()
    try:
        stats = pipe.run()
    except KeyboardInterrupt:
        # Commit what is already safely replayable so 'resume' can pick it up.
        # A partial commit is always consistent with the truncated output.
        pipe._commit(force=True)
        pipe._close_outputs()
        pipe.ckpt.close()
        print("\ninterrupted; checkpoint saved. Run 'resume' to continue.")
        return 130
    elapsed = time.time() - t0

    pipe.ckpt.close()
    _report(stats, elapsed, args.output, pipe.ckpt)
    if stats.corrupt:
        print(f"dlq        {args.dlq}")
        _warn_digest(args.dlq)
    return 0


def cmd_stats(args: argparse.Namespace) -> int:
    if not os.path.exists(args.state):
        print(f"no such state file: {args.state}", file=sys.stderr)
        return 2
    conn = sqlite3.connect(f"file:{args.state}?mode=ro", uri=True)
    try:
        counters = dict(conn.execute("SELECT key, value FROM counters"))
        meta = dict(conn.execute("SELECT key, value FROM meta"))
        sessions = conn.execute("SELECT COUNT(*) FROM session_state").fetchone()[0]
    finally:
        conn.close()

    processed = counters.get("lines_read", 0)
    corrupt = counters.get("corrupt", 0)
    resumed = counters.get("resumed", 0)
    frames = counters.get("frames_committed", 0)

    if args.json:
        print(json.dumps(
            {
                "processed": processed,
                "frames_committed": frames,
                "corrupt": corrupt,
                "duplicate": counters.get("duplicate", 0),
                "resumed": resumed,
                "sessions": sessions,
                "input_path": meta.get("input_path", ""),
                "input_size": int(meta.get("input_size", -1)),
                "resume_offset": int(meta.get("global_offset", 0)),
                "high_water": int(meta.get("high_water", 0)),
                "complete": meta.get("input_done") == "1",
                "updated_at": float(meta.get("updated_at", 0)),
            },
            indent=2,
        ))
        return 0

    size = int(meta.get("input_size", -1))
    safe = int(meta.get("global_offset", 0))
    high = int(meta.get("high_water", 0))
    print(f"processed  {_fmt_int(processed)}")
    print(f"corrupt    {_fmt_int(corrupt)}")
    print(f"resumed    {_fmt_int(resumed)}")
    print(f"frames     {_fmt_int(frames)}")
    print(f"duplicates {_fmt_int(counters.get('duplicate', 0))}")
    print(f"sessions   {_fmt_int(sessions)}")
    if size > 0:
        # Two different numbers, and conflating them would be misleading:
        #  * high_water is how far the input has actually been consumed.
        #  * global_offset is the safe resume point, which deliberately lags it
        #    so fragments still held in the reassembly window are re-supplied.
        print(f"progress   {_fmt_int(high)} / {_fmt_int(size)} bytes "
              f"({high * 100.0 / size:.1f}%)")
        if safe < high:
            print(f"resume at  {_fmt_int(safe)} bytes "
                  f"({high - safe:,} bytes re-read on resume)")
    print(f"complete   {'yes' if meta.get('input_done') == '1' else 'no'}")
    print(f"input      {meta.get('input_path', '')}")
    return 0


def main(argv: Optional[List[str]] = None) -> int:
    args = build_parser().parse_args(argv)
    if args.command == "replay":
        return cmd_replay(args, resume=False)
    if args.command == "resume":
        if not os.path.exists(args.state):
            print(f"nothing to resume: {args.state} not found", file=sys.stderr)
            return 2
        return cmd_replay(args, resume=True)
    if args.command == "stats":
        return cmd_stats(args)
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
