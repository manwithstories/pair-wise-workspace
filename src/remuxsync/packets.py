"""Deterministic packet container: framed transport packets and stream recovery.

The sample generator wraps elementary-stream bytes (or audio frames) into
fixed-header packets so the pipeline can exercise real link defects:

* loss (gap in sequence numbers, optionally healed by a retransmission),
* duplicate delivery and reordered delivery within a bounded time window,
* truncated headers / truncated payloads,
* 33-bit capture-timestamp wrap.

The reorder buffer is bounded by a time window (default 120 ms) and never
buffers the whole stream: packets are released in timestamp order as soon as
the window slides past them. A late packet arriving outside the window is
recorded as an unrecoverable error instead of being silently inserted.
"""

from __future__ import annotations

import struct
from dataclasses import dataclass, field

from .clock import TICKS_PER_MS, WRAP_MODULUS, unwrap_timestamps
from .errors import ErrorLog, Recovery, Stage

MAGIC = 0x5258  # "RX"
MAGIC_BYTES = struct.pack(">H", MAGIC)
HEADER_STRUCT = struct.Struct(">HBIIQ")  # magic, flags, seq, length, ts33
HEADER_SIZE = HEADER_STRUCT.size  # 20 bytes
RESYNC_CHUNK = 1 << 14
MAX_PACKET_PAYLOAD = 1 << 24

# flag bits
FLAG_KEY = 1 << 0
FLAG_AUDIO = 1 << 1
FLAG_RETX = 1 << 2
FLAG_TS_WRAP = 1 << 3
FLAG_TRUNC = 1 << 4


@dataclass
class Packet:
    seq: int
    ts33: int          # raw 33-bit capture timestamp
    ts_unwrapped: int  # filled in by parse_stream
    flags: int
    payload: bytes
    offset: int        # raw byte offset of the packet header in the input file
    hold_ticks: int = 0  # ticks spent inside the reorder buffer before release
    truncated: bool = False

    @property
    def is_key(self) -> bool:
        return bool(self.flags & FLAG_KEY)

    @property
    def is_audio(self) -> bool:
        return bool(self.flags & FLAG_AUDIO)


@dataclass
class PacketStream:
    """Packets released in unwrapped-timestamp order."""

    packets: list[Packet] = field(default_factory=list)
    expected_seq_max: int = -1
    recovered_seqs: set[int] = field(default_factory=set)
    unrecovered_seqs: set[int] = field(default_factory=set)


def encode_packet(seq: int, ts33: int, payload: bytes, flags: int) -> bytes:
    """Encode one packet (header + payload)."""
    return HEADER_STRUCT.pack(MAGIC, flags, seq, len(payload),
                              ts33 & (WRAP_MODULUS - 1)) + payload


def _resync(f) -> int:
    """Scan forward to the next plausible packet header; -1 at EOF."""
    pos = f.tell()
    tail = b""
    while True:
        chunk = f.read(RESYNC_CHUNK)
        if not chunk:
            return -1
        data = tail + chunk
        idx = data.find(MAGIC_BYTES)
        while idx >= 0:
            candidate = pos + idx
            if len(data) - idx >= HEADER_SIZE:
                _m, _fl, _sq, length, _ts = HEADER_STRUCT.unpack(
                    data[idx:idx + HEADER_SIZE])
                if _m == MAGIC and length <= MAX_PACKET_PAYLOAD:
                    f.seek(candidate)
                    return candidate
            idx = data.find(MAGIC_BYTES, idx + 1)
        tail = data[-(HEADER_SIZE - 1):]
        pos += len(data) - len(tail)


def parse_stream(
    path: str,
    track: str,
    log: ErrorLog,
    reorder_window_ms: int = 120,
) -> PacketStream:
    """Parse a packet file, recovering loss/reorder/duplication defects.

    All error offsets are absolute byte offsets inside ``path``.
    """
    window_ticks = reorder_window_ms * TICKS_PER_MS
    raw: list[Packet] = []
    file_size = 0
    with open(path, "rb") as f:
        f.seek(0, 2)
        file_size = f.tell()
        f.seek(0)
        offset = 0
        while True:
            hdr = f.read(HEADER_SIZE)
            if not hdr:
                break
            if len(hdr) < HEADER_SIZE:
                log.add(Stage.PARSE, track,
                        "truncated packet header at end of stream",
                        byte_offset=offset, recovery=Recovery.UNRECOVERABLE)
                break
            magic, flags, seq, length, ts33 = HEADER_STRUCT.unpack(hdr)
            if magic != MAGIC or length > MAX_PACKET_PAYLOAD:
                # desync (e.g. a truncated payload earlier): resync on MAGIC
                log.add(Stage.PARSE, track,
                        f"invalid packet magic 0x{magic:04x}; scanning for resync",
                        byte_offset=offset, recovery=Recovery.SKIPPED_GARBAGE,
                        packet_seq=seq if magic != MAGIC else None)
                f.seek(offset + 2)
                hit = _resync(f)
                if hit < 0:
                    break
                offset = hit
                f.seek(offset)
                continue
            payload = f.read(length)
            if len(payload) < length:
                log.add(Stage.PARSE, track,
                        f"truncated packet payload: declared {length} bytes, "
                        f"got {len(payload)} at end of stream",
                        byte_offset=offset, recovery=Recovery.DROPPED_UNIT,
                        packet_seq=seq)
                break
            pkt = Packet(seq, ts33, 0, flags, payload, offset,
                         truncated=bool(flags & FLAG_TRUNC))
            if pkt.truncated:
                log.add(Stage.PARSE, track,
                        f"truncated unit in packet seq={seq}: "
                        f"{length} bytes delivered, awaiting retransmission",
                        byte_offset=offset, recovery=Recovery.DROPPED_UNIT,
                        packet_seq=seq)
            raw.append(pkt)
            offset += HEADER_SIZE + length

    if not raw:
        return PacketStream(expected_seq_max=-1)

    # --- 33-bit unwrap, then bounded timestamp-ordered reordering ----------
    unwrapped, wrap_idx = unwrap_timestamps([p.ts33 for p in raw])
    for p, ts in zip(raw, unwrapped):
        p.ts_unwrapped = ts
    for i in wrap_idx:
        log.add(Stage.TRANSFORM, track,
                "33-bit timestamp wrap corrected",
                byte_offset=raw[i].offset, recovery=Recovery.UNWRAPPED,
                packet_seq=raw[i].seq)

    seen_seq: set[int] = set()
    dup_seq: set[int] = set()
    for p in raw:
        if p.seq in seen_seq:
            dup_seq.add(p.seq)
        seen_seq.add(p.seq)

    result = PacketStream(expected_seq_max=max(seen_seq))
    released: dict[int, Packet] = {}          # seq -> chosen packet
    order: list[int] = []                     # release order (seqs)
    reordered_seqs: set[int] = set()
    high_water = -1                          # highest ts seen so far
    released_water: int | None = None        # highest ts released so far
    buffer: list[Packet] = []                # not-yet-released arrivals
    late_packets: list[Packet] = []

    def emit(pkt: Packet) -> None:
        nonlocal released_water
        prev = released.get(pkt.seq)
        if prev is not None:
            if pkt.flags & FLAG_RETX and not (prev.flags & FLAG_RETX):
                released[pkt.seq] = pkt  # prefer retransmitted copy
            log.add(Stage.TRANSFORM, track,
                    f"duplicate packet seq={pkt.seq} removed",
                    byte_offset=pkt.offset, recovery=Recovery.DEDUPLICATED,
                    packet_seq=pkt.seq)
            return
        # a packet whose timestamp is older than what was already released
        # was genuinely held for reordering rather than passing straight through
        if released_water is not None and pkt.ts_unwrapped < released_water:
            reordered_seqs.add(pkt.seq)
        pkt.hold_ticks = max(0, high_water - pkt.ts_unwrapped)
        released[pkt.seq] = pkt
        order.append(pkt.seq)
        released_water = pkt.ts_unwrapped if released_water is None \
            else max(released_water, pkt.ts_unwrapped)
        if pkt.flags & FLAG_RETX:
            log.add(Stage.TRANSFORM, track,
                    f"packet seq={pkt.seq} restored by retransmission",
                    byte_offset=pkt.offset, recovery=Recovery.RETRANSMITTED,
                    packet_seq=pkt.seq)

    def flush_ready() -> None:
        """Release buffered packets at or beyond the release watermark.

        An in-order packet (ts >= everything released) leaves immediately with
        zero added latency; a packet older than the watermark stays buffered
        until its predecessor arrives or the window forces it out.
        """
        buffer.sort(key=lambda q: (q.ts_unwrapped, q.seq))
        while buffer and (released_water is None
                          or buffer[0].ts_unwrapped >= released_water):
            emit(buffer.pop(0))

    for p in raw:
        high_water = max(high_water, p.ts_unwrapped)
        stale = [q for q in buffer
                 if high_water - q.ts_unwrapped > window_ticks]
        for q in stale:  # window expired: release despite the remaining gap
            buffer.remove(q)
            emit(q)
        if high_water - p.ts_unwrapped > window_ticks and p.seq not in released:
            late_packets.append(p)  # arrived too late to join the window
            continue
        buffer.append(p)
        flush_ready()
    buffer.sort(key=lambda q: (q.ts_unwrapped, q.seq))
    while buffer:
        emit(buffer.pop(0))

    for p in late_packets:
        log.add(Stage.TRANSFORM, track,
                f"out-of-order packet seq={p.seq} arrived outside "
                f"{reorder_window_ms} ms window",
                byte_offset=p.offset, recovery=Recovery.UNRECOVERABLE,
                packet_seq=p.seq)

    # --- sequence continuity: healed gaps vs unrecovered loss --------------
    for seq in sorted(seen_seq):
        p = next(q for q in raw if q.seq == seq)
        if seq in dup_seq:
            result.recovered_seqs.add(seq)
    for seq in range(min(seen_seq), max(seen_seq) + 1):
        if seq in released:
            result.packets.append(released[seq])
        elif seq in seen_seq:  # existed but rejected (late/truncated)
            result.unrecovered_seqs.add(seq)
        else:
            log.add(Stage.TRANSFORM, track,
                    f"missing packet seq={seq}: no delivery or retransmission",
                    byte_offset=file_size, recovery=Recovery.UNRECOVERABLE,
                    packet_seq=seq)
            result.unrecovered_seqs.add(seq)

    for seq in reordered_seqs:
        pkt = released[seq]
        log.add(Stage.TRANSFORM, track,
                f"packet seq={seq} reordered within window",
                byte_offset=pkt.offset, recovery=Recovery.REORDERED,
                packet_seq=seq)

    result.packets.sort(key=lambda p: (p.ts_unwrapped, p.seq))
    return result
