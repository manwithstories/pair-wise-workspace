#!/usr/bin/env python3
"""样例生成与故障链路演示：依次跑全部场景并汇总退出码。

用法：
    .venv/bin/python scripts/demo.py [--out /tmp/relay-cases] [--seed 41]
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))

from relay_timeline.cli import DEMO_CASES, main as cli_main


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", default="/tmp/relay-cases")
    parser.add_argument("--seed", type=int, default=41)
    args = parser.parse_args()

    failures = 0
    for case in DEMO_CASES:
        out = str(Path(args.out) / case)
        code = cli_main(
            ["demo", "--case", case, "--seed", str(args.seed), "--out", out]
        )
        # invalid-cut 应得 2，truncated-output 应得 3，其余应为 0
        expected = {"invalid-cut": 2, "truncated-output": 3}.get(case, 0)
        ok = code == expected
        failures += 0 if ok else 1
        print(f"[{'OK' if ok else 'FAIL'}] {case}: exit={code} "
              f"(期望 {expected})")
    print(f"完成：{len(DEMO_CASES) - failures}/{len(DEMO_CASES)} 个场景符合预期")
    return 0 if failures == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
