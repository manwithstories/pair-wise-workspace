"""批量生成多场景样例轨迹到 traces/ 目录。

用法：python scripts/gen_traces.py [输出目录]
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from abr.trace import generate_samples, write_csv

SCENARIOS = (
    # (文件名, seed, 时长 s, 状态序列, 中断次数)
    ("mixed.csv", 7, 600, ["wifi", "4g", "3g"], 3),
    ("commute.csv", 11, 600, ["4g", "3g", "edge"], 5),
    ("stable_wifi.csv", 3, 600, ["wifi"], 0),
    ("subway.csv", 21, 600, ["4g", "edge"], 8),
)


def main() -> int:
    out_dir = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("traces")
    for name, seed, duration, regimes, outages in SCENARIOS:
        samples = generate_samples(seed=seed, duration_s=duration,
                                   regimes=regimes, outages=outages)
        path = out_dir / name
        write_csv(path, samples)
        print(f"written {path} ({len(samples)}s, seed={seed}, "
              f"regimes={','.join(regimes)}, outages={outages})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
