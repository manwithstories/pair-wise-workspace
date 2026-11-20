#!/usr/bin/env python3
"""Acceptance demo driver.

Usage (from the project root):

    .venv/bin/python scripts/demo.py --seed 41 --drop-rate 0.02 \
        --queue-frames 8 --consumer-delay-ms 15 --out /tmp/bcastqc-demo

Generates deterministic fixtures (clean, dropped, truncated and bad-index
variants), runs the QC pipeline with a bounded queue and a slow consumer, then
prints the acceptance fields plus the failure-case results.  The exit code is
the pipeline contract: 0 pass, 2 invalid input/timeline, 3 unrecoverable
media error, 4 resource/output error.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

# Allow running from a checkout without installation.
ROOT = Path(__file__).resolve().parents[1]
if str(ROOT / "src") not in sys.path:
    sys.path.insert(0, str(ROOT / "src"))

from bcastqc.cli import main as cli_main  # noqa: E402


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="bcastqc acceptance demo")
    parser.add_argument("--seed", type=int, default=41)
    parser.add_argument("--drop-rate", type=float, default=0.02)
    parser.add_argument("--queue-frames", type=int, default=8)
    parser.add_argument("--consumer-delay-ms", type=float, default=15.0)
    parser.add_argument("--frames", type=int, default=75)
    parser.add_argument("--out", default="/tmp/bcastqc-demo")
    parser.add_argument("--keep-existing", action="store_true")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return cli_main(
        [
            "demo",
            "--seed", str(args.seed),
            "--drop-rate", str(args.drop_rate),
            "--queue-frames", str(args.queue_frames),
            "--consumer-delay-ms", str(args.consumer_delay_ms),
            "--frames", str(args.frames),
            "--out", str(args.out),
            *(["--keep-existing"] if args.keep_existing else []),
        ]
    )


if __name__ == "__main__":
    raise SystemExit(main())
