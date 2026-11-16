"""测试共享夹具与导入路径引导。"""

from __future__ import annotations

import sys
from pathlib import Path

# 让所有测试模块在未安装包时也能导入 src/abr
ROOT = Path(__file__).resolve().parent.parent
SRC = ROOT / "src"
if str(SRC) not in sys.path:
    sys.path.insert(0, str(SRC))

from abr.trace import Trace  # noqa: E402
from abr.types import NetworkSample, TracePattern, ThroughputSample  # noqa: E402

BITRATES = (300.0, 800.0, 1500.0, 2500.0, 4000.0, 6000.0)


def throughput(seg: int, kbps: float, dur: float = 2.0,
               sent: int = 100_000, loss: float = 0.0) -> ThroughputSample:
    return ThroughputSample(seg, kbps, dur, sent, loss)


def flat_trace(bandwidth_schedule, duration=120.0, step=0.1,
               latency=40.0, loss=0.0) -> Trace:
    """用 (起始时间, 带宽) 阶跃表构造确定性轨迹。"""
    n = int(duration / step) + 1
    samples = []
    for i in range(n):
        t = i * step
        bw = bandwidth_schedule[0][1]
        for start, value in bandwidth_schedule:
            if t >= start:
                bw = value
        samples.append(NetworkSample(t, bw, latency, loss))
    return Trace(samples, TracePattern.STABLE, seed=1)
