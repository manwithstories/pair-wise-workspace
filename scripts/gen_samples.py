from __future__ import annotations

import argparse
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from mediaprobe.samplegen import generate_samples  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate deterministic media probe fixtures")
    parser.add_argument("--out", default="fixtures")
    parser.add_argument("--count", type=int, default=14)
    parser.add_argument("--seed", type=int, default=7)
    args = parser.parse_args()
    paths = generate_samples(args.out, args.count, args.seed)
    print(f"generated={len(paths)} out={args.out} seed={args.seed}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
