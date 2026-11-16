#!/usr/bin/env python3
"""Convenience wrapper for generating a reproducible sample network trace.

Equivalent to ``python -m abr_engine generate-trace`` but callable directly
as a script. All randomness comes from the seeded NumPy generator inside
:mod:`abr_engine.trace`, so identical arguments always produce an
identical JSON file.

Example:
    python scripts/make_trace.py --segments 180 --segment-seconds 2 \
        --seed 42 --output sample_trace.json
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

# Allow running the script directly from a fresh checkout (no install).
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from abr_engine.io import save_trace  # noqa: E402
from abr_engine.trace import generate_trace  # noqa: E402


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Generate an ABR network trace.")
    parser.add_argument("--segments", type=int, required=True)
    parser.add_argument("--segment-seconds", type=float, default=2.0)
    parser.add_argument("--seed", type=int, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--name", default="trace")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    trace = generate_trace(
        segments=args.segments,
        segment_seconds=args.segment_seconds,
        seed=args.seed,
        name=args.name,
    )
    path = save_trace(trace, args.output)
    print(f"wrote {len(trace)} segments to {path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
