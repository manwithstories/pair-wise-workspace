"""网络轨迹生成与校验。

按状态迁移模型（马尔可夫链）逐秒生成带宽样本，叠加突发丢包与
短时中断（outage），落盘为 CSV；读取时逐行校验，错误带行号。
"""

from __future__ import annotations

import csv
from dataclasses import dataclass
from pathlib import Path

import numpy as np

CSV_FIELDS = ("ts_ms", "bandwidth_kbps", "rtt_ms", "loss_pct")


class TraceValidationError(Exception):
    """轨迹文件校验失败，line_no 为 1 起始的文件行号（0 表示整体性问题）。"""

    def __init__(self, message: str, line_no: int = 0) -> None:
        self.line_no = line_no
        super().__init__(f"line {line_no}: {message}" if line_no else message)


@dataclass(frozen=True)
class Regime:
    name: str
    bw_mean_kbps: float
    bw_std_kbps: float
    rtt_ms: float
    loss_pct: float


REGIMES: dict[str, Regime] = {
    "wifi": Regime("wifi", bw_mean_kbps=6000.0, bw_std_kbps=1200.0, rtt_ms=20.0, loss_pct=0.2),
    "4g": Regime("4g", bw_mean_kbps=3500.0, bw_std_kbps=1000.0, rtt_ms=45.0, loss_pct=0.8),
    "3g": Regime("3g", bw_mean_kbps=2000.0, bw_std_kbps=600.0, rtt_ms=120.0, loss_pct=2.0),
    "edge": Regime("edge", bw_mean_kbps=600.0, bw_std_kbps=200.0, rtt_ms=300.0, loss_pct=5.0),
}

STAY_PROB = 0.95          # 每秒停留在当前状态的概率
LOSS_BURST_PROB = 0.02    # 每秒触发突发丢包的概率
LOSS_BURST_PCT = 12.0     # 突发丢包幅度
MIN_BW_KBPS = 50.0        # 非中断期的带宽下限


@dataclass(frozen=True)
class TraceSample:
    ts_ms: int
    bandwidth_kbps: float
    rtt_ms: float
    loss_pct: float


def generate_samples(
    seed: int,
    duration_s: int,
    regimes: list[str],
    outages: int = 0,
) -> list[TraceSample]:
    """按 seed 确定性生成逐秒轨迹样本。"""
    if duration_s <= 0:
        raise ValueError("duration_s must be positive")
    unknown = [r for r in regimes if r not in REGIMES]
    if unknown:
        raise ValueError(f"unknown regimes: {','.join(unknown)}")
    if not regimes:
        raise ValueError("at least one regime is required")

    rng = np.random.default_rng(seed)
    pool = [REGIMES[r] for r in regimes]
    state_idx = int(rng.integers(0, len(pool)))

    # 预生成中断区间：[start, end) 秒，带宽压为 0
    outage_spans: list[tuple[int, int]] = []
    if outages > 0 and duration_s > 8:
        starts = rng.integers(1, duration_s - 6, size=outages)
        lengths = rng.integers(2, 7, size=outages)
        outage_spans = sorted((int(s), int(s) + int(n)) for s, n in zip(starts, lengths))

    samples: list[TraceSample] = []
    for sec in range(duration_s):
        if len(pool) > 1 and rng.random() > STAY_PROB:
            state_idx = int(rng.integers(0, len(pool)))
        regime = pool[state_idx]

        in_outage = any(start <= sec < end for start, end in outage_spans)
        if in_outage:
            bw, rtt, loss = 0.0, regime.rtt_ms * 3.0, 100.0
        else:
            bw = float(max(MIN_BW_KBPS, rng.normal(regime.bw_mean_kbps, regime.bw_std_kbps)))
            rtt = float(max(1.0, rng.normal(regime.rtt_ms, regime.rtt_ms * 0.1)))
            loss = float(max(0.0, rng.normal(regime.loss_pct, regime.loss_pct * 0.3)))
            if rng.random() < LOSS_BURST_PROB:
                loss += LOSS_BURST_PCT
                bw *= 0.5
        samples.append(
            TraceSample(
                ts_ms=sec * 1000,
                bandwidth_kbps=round(bw, 3),
                rtt_ms=round(rtt, 3),
                loss_pct=round(min(loss, 100.0), 3),
            )
        )
    return samples


def write_csv(path: str | Path, samples: list[TraceSample]) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="", encoding="utf-8") as fh:
        writer = csv.writer(fh)
        writer.writerow(CSV_FIELDS)
        for s in samples:
            writer.writerow([s.ts_ms, f"{s.bandwidth_kbps:.3f}", f"{s.rtt_ms:.3f}", f"{s.loss_pct:.3f}"])


def read_csv(path: str | Path) -> list[TraceSample]:
    """读取并逐行校验轨迹 CSV，任何违规都抛 TraceValidationError（带行号）。"""
    path = Path(path)
    if not path.exists():
        raise TraceValidationError(f"trace file not found: {path}")
    text = path.read_text(encoding="utf-8")
    if not text.strip():
        raise TraceValidationError("empty trace file")

    rows = text.splitlines()
    header = [c.strip() for c in rows[0].split(",")]
    missing = [f for f in CSV_FIELDS if f not in header]
    if missing:
        raise TraceValidationError(f"missing columns: {','.join(missing)}", line_no=1)
    col = {name: header.index(name) for name in CSV_FIELDS}

    samples: list[TraceSample] = []
    prev_ts: int | None = None
    for line_no, raw in enumerate(rows[1:], start=2):
        if not raw.strip():
            continue
        cells = raw.split(",")
        if len(cells) < len(header):
            raise TraceValidationError(f"expected {len(header)} columns, got {len(cells)}", line_no)
        try:
            ts_ms = int(float(cells[col["ts_ms"]]))
            bw = float(cells[col["bandwidth_kbps"]])
            rtt = float(cells[col["rtt_ms"]])
            loss = float(cells[col["loss_pct"]])
        except ValueError:
            raise TraceValidationError(f"non-numeric field: {raw!r}", line_no) from None
        if prev_ts is not None and ts_ms <= prev_ts:
            raise TraceValidationError(f"non-monotonic ts_ms: {ts_ms} <= {prev_ts}", line_no)
        if bw < 0:
            raise TraceValidationError(f"negative bandwidth_kbps: {bw}", line_no)
        if not 0.0 <= loss <= 100.0:
            raise TraceValidationError(f"loss_pct out of range [0,100]: {loss}", line_no)
        prev_ts = ts_ms
        samples.append(TraceSample(ts_ms=ts_ms, bandwidth_kbps=bw, rtt_ms=rtt, loss_pct=loss))

    if not samples:
        raise TraceValidationError("trace has header but no data rows")
    return samples
