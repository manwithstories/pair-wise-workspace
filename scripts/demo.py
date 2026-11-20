#!/usr/bin/env python3
"""End-to-end demo: generate fixtures, run, interrupt, and print the report.

Usage (from the project root, with the project venv)::

    .venv/bin/python scripts/demo.py --generate --workdir .demo \
        --sample-rate 48000 --layout stereo --target-lufs -16 \
        --interrupt-after 2

Then::

    .venv/bin/python -m mediacore.cli resume --workdir .demo --verify

The script is a thin wrapper around :mod:`mediacore.cli` so there is exactly
one implementation of the behaviour being demonstrated.
"""

from __future__ import annotations

import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
for candidate in (ROOT, ROOT / "src"):
    if str(candidate) not in sys.path:
        sys.path.insert(0, str(candidate))

from mediacore.cli import main  # noqa: E402


def run(argv: list[str]) -> int:
    return main(["demo", *argv])


if __name__ == "__main__":
    raise SystemExit(run(sys.argv[1:]))
