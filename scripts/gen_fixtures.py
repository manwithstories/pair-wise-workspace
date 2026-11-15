#!/usr/bin/env python3
"""Generate paired v1/v2 fixture files (wrapper around tlkit.fixtures)."""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))

from tlkit.fixtures import generate  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", default="fixtures")
    parser.add_argument("--seed", type=int, default=7)
    parser.add_argument("--clips", type=int, default=400)
    args = parser.parse_args()
    print(json.dumps(generate(args.out, args.seed, args.clips),
                     indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
