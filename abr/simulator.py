"""事件循环：串起轨迹、估计器、缓冲与决策器，输出逐段记录。"""

from __future__ import annotations

from dataclasses import dataclass

from .buffer import BufferLedger
from .decider import Decider
from .estimator import Estimator
from .ladder import Ladder
from .trace import TraceSample

STEP_S = 1.0  # 轨迹为逐秒样本，时钟步长 1 秒
EPS = 1e-9


class NoUsableSegmentError(Exception):
    """轨迹不足一个分片，或没有任何分片完整下载（退出码 4）。"""


@dataclass(frozen=True)
class SegmentRecord:
    index: int
    bitrate_kbps: int
    estimate_kbps: float
    download_ms: float
    buffer_after_s: float
    clock_end_ms: float


def _effective_bandwidth(sample: TraceSample) -> float:
    """丢包折算有效带宽。"""
    return sample.bandwidth_kbps * (1.0 - sample.loss_pct / 100.0)


def simulate(
    samples: list[TraceSample],
    estimator: Estimator,
    decider: Decider,
    ladder: Ladder,
    buffer: BufferLedger,
) -> tuple[list[SegmentRecord], float]:
    """逐段回放，返回 (分片记录, startup_ms)。

    下载按分数秒结算：分片在某一秒中途下完时，该秒剩余带宽留给下一片。
    水位接近上限时暂停拉流而不是溢出截断。
    轨迹耗尽时分片若未下完则丢弃并结束；0 带宽长段只推进时钟、
    触发耗尽，不会死循环（trace 游标每步必前进）。
    """
    if len(samples) * STEP_S < ladder.chunk_sec:
        raise NoUsableSegmentError(
            f"trace duration {len(samples)}s shorter than one chunk ({ladder.chunk_sec}s)"
        )

    records: list[SegmentRecord] = []
    startup_ms: float | None = None
    cursor = 0
    sec_pos = 0.0  # 当前轨迹秒已被消耗的比例
    total = len(samples)

    while cursor < total:
        if buffer.should_pause() or buffer.level_s + ladder.chunk_sec > buffer.cap_s:
            avail = STEP_S - sec_pos
            buffer.pause(avail)
            cursor += 1
            sec_pos = 0.0
            continue

        estimate = estimator.estimate()
        bitrate = ladder.bitrates_kbps[decider.decide(estimate, buffer.level_s)]
        need_bytes = ladder.chunk_bytes(bitrate)

        downloaded = 0.0
        start_clock = buffer.clock_ms
        while downloaded < need_bytes and cursor < total:
            rate = _effective_bandwidth(samples[cursor]) * 1000.0 / 8.0  # bytes/s
            avail = STEP_S - sec_pos
            if rate <= 0.0:
                buffer.drain(avail)
                cursor += 1
                sec_pos = 0.0
                continue
            dt = min(avail, (need_bytes - downloaded) / rate)
            downloaded += rate * dt
            buffer.drain(dt)
            sec_pos += dt
            if sec_pos >= STEP_S - EPS:
                cursor += 1
                sec_pos = 0.0
        if downloaded < need_bytes:
            break  # 轨迹耗尽，末段不完整，丢弃

        buffer.add_chunk(ladder.chunk_sec)
        download_ms = buffer.clock_ms - start_clock
        throughput_kbps = need_bytes * 8.0 / (download_ms / 1000.0) / 1000.0
        estimator.add_sample(throughput_kbps)
        if startup_ms is None:
            startup_ms = buffer.clock_ms
        records.append(
            SegmentRecord(
                index=len(records),
                bitrate_kbps=bitrate,
                estimate_kbps=estimate,
                download_ms=download_ms,
                buffer_after_s=buffer.level_s,
                clock_end_ms=buffer.clock_ms,
            )
        )

    if not records:
        raise NoUsableSegmentError("no segment completed within the trace")
    return records, startup_ms if startup_ms is not None else 0.0
