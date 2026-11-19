"""Repair stage: dedup, windowed reorder and 90 kHz timestamp fixing.

Sequence numbers wrap at 16 bits, timestamps at 32 bits; both are
extended to monotonic 64-bit domains.  When timing cannot be inferred
reliably the affected interval is marked uncertain rather than being
assigned fabricated precise times.
"""

from __future__ import annotations

from .model import (
    MEDIA_CLOCK_RATE,
    SEQ_MOD,
    SEQ_HALF,
    TS_MOD,
    PacketError,
    RepairResult,
    RtpPacket,
    Stage,
    UncertainInterval,
)

# A forward timestamp jump larger than this (in 90 kHz ticks, default
# 5 seconds) is treated as abnormal and its interval marked uncertain.
DEFAULT_JUMP_THRESHOLD = MEDIA_CLOCK_RATE * 5


def _extend_seq(seq: int, prev_seq: int, cycles: int) -> tuple[int, int]:
    """Extend a 16-bit sequence number into 64-bit wrap-aware domain.

    Returns (extended_seq, updated_cycles).  A forward jump across the
    0xFFFF boundary bumps the cycle base; a large apparent backward jump
    is a late packet from the previous cycle.
    """
    if seq < prev_seq and (prev_seq - seq) > SEQ_HALF:
        cycles += SEQ_MOD
        return cycles + seq, cycles
    if seq > prev_seq and (seq - prev_seq) > SEQ_HALF:
        return cycles + seq - SEQ_MOD, cycles
    return cycles + seq, cycles


def repair_packets(
    packets: list[RtpPacket],
    window: int = 64,
    jump_threshold: int = DEFAULT_JUMP_THRESHOLD,
) -> RepairResult:
    """Dedup, reorder within ``window`` and repair timestamps per SSRC."""
    result = RepairResult()
    by_ssrc: dict[int, list[int]] = {}
    for idx, pkt in enumerate(packets):
        by_ssrc.setdefault(pkt.ssrc, []).append(idx)
    for ssrc in sorted(by_ssrc):
        _repair_ssrc(by_ssrc[ssrc], packets, window, jump_threshold, result)
    # Emit order: interleave SSRC streams by extended sequence of first
    # appearance; within one SSRC the order is already repaired.
    result.order.sort(key=lambda i: (packets[i].arrival_index // (window * 4),
                                     packets[i].ssrc,
                                     result.ext_seq[i]))
    return result


def _repair_ssrc(
    indices: list[int],
    packets: list[RtpPacket],
    window: int,
    jump_threshold: int,
    result: RepairResult,
) -> None:
    seen: set[tuple[int, int]] = set()
    buffer: dict[int, int] = {}  # ext_seq -> packet index
    cycles = 0
    prev_seq: int | None = None
    next_emit: int | None = None
    max_buffered = -1
    prev_ts_ext: int | None = None
    ts_cycles = 0

    def emit(idx: int) -> None:
        nonlocal prev_ts_ext, ts_cycles
        pkt = packets[idx]
        ts = pkt.timestamp
        if prev_ts_ext is not None:
            low = prev_ts_ext % TS_MOD
            if ts < low and (low - ts) > (TS_MOD // 2):
                ts_cycles += TS_MOD  # 32-bit timestamp wrapped forward
        ts_ext = ts_cycles + ts
        if prev_ts_ext is not None:
            delta = ts_ext - prev_ts_ext
            if delta < 0:
                result.errors.append(
                    PacketError(pkt.offset, pkt.offset + pkt.length,
                                Stage.REPAIR, "timestamp_backward",
                                f"ts moved backwards by {-delta} ticks")
                )
                result.uncertain.append(
                    UncertainInterval(result.ext_seq[idx], result.ext_seq[idx],
                                      "timestamp_backward")
                )
            elif delta > jump_threshold:
                result.uncertain.append(
                    UncertainInterval(result.ext_seq[idx], result.ext_seq[idx],
                                      f"timestamp_jump_{delta}")
                )
        result.ext_ts[idx] = ts_ext
        prev_ts_ext = ts_ext
        result.order.append(idx)

    for idx in indices:
        pkt = packets[idx]
        key = (pkt.sequence, pkt.timestamp)
        if key in seen:
            result.duplicates += 1
            continue
        seen.add(key)

        if prev_seq is None:
            ext = pkt.sequence
            cycles = 0
        else:
            ext, cycles = _extend_seq(pkt.sequence, prev_seq, cycles)
        prev_seq = pkt.sequence

        if next_emit is None:
            next_emit = ext
        if ext < next_emit:
            # Too late: the window already slid past this packet.
            result.out_of_window += 1
            result.errors.append(
                PacketError(pkt.offset, pkt.offset + pkt.length, Stage.REPAIR,
                            "out_of_window",
                            f"ext_seq {ext} below emit frontier {next_emit}")
            )
            continue
        if ext > next_emit + (window - 1):
            # Force-flush the head of the buffer to stay within window.
            while next_emit in buffer and ext > next_emit + (window - 1):
                emit(buffer.pop(next_emit))
                next_emit += 1
            while next_emit not in buffer and next_emit < ext - (window - 1):
                result.lost += 1
                next_emit += 1
        if ext in buffer:
            result.duplicates += 1
            continue
        if ext < max_buffered:
            result.reordered += 1
        max_buffered = max(max_buffered, ext)
        buffer[ext] = idx
        result.ext_seq[idx] = ext
        while next_emit in buffer:
            emit(buffer.pop(next_emit))
            next_emit += 1

    # Flush remaining buffered packets; gaps count as lost.
    for ext in sorted(buffer):
        while next_emit is not None and next_emit < ext:
            result.lost += 1
            next_emit += 1
        emit(buffer[ext])
        next_emit = ext + 1
