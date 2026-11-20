"""Shared error model: duplicate, reorder, loss and repair confidence.

The ReorderBuffer emits packets in sequence order using a bounded window.
Duplicates are dropped and counted; gaps are reported as loss events
(isolated vs burst); packets arriving after their position has been
committed are counted as late and never rewrite already-committed output.
The FrameReassembler groups payload chunks into frames and assigns each
frame a repair confidence.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .rtp import RtpPacket, unpack_payload

# A run of at least this many consecutively lost packets is a burst.
BURST_MIN_LEN = 4


@dataclass(frozen=True)
class LossEvent:
    first_seq: int
    count: int
    burst: bool


@dataclass
class RepairStats:
    duplicates: int = 0
    late_packets: int = 0
    lost_packets: int = 0
    loss_bursts: int = 0
    loss_events: list[LossEvent] = field(default_factory=list)

    def record_loss(self, first_seq: int, count: int) -> None:
        burst = count >= BURST_MIN_LEN
        self.lost_packets += count
        if burst:
            self.loss_bursts += 1
        self.loss_events.append(LossEvent(first_seq=first_seq, count=count,
                                          burst=burst))


@dataclass(frozen=True)
class Emitted:
    """One ordered output slot: either a packet or a loss placeholder."""
    seq: int
    packet: RtpPacket | None  # None => lost


class ReorderBuffer:
    """Bounded-window sequence reordering for one SSRC."""

    def __init__(self, window: int, stats: RepairStats) -> None:
        if window < 1:
            raise ValueError("reorder window must be >= 1")
        self.window = window
        self.stats = stats
        self._next: int | None = None  # next expected extended seq
        self._buf: dict[int, RtpPacket] = {}
        self._seen: set[int] = set()

    def push(self, pkt: RtpPacket) -> list[Emitted]:
        out: list[Emitted] = []
        seq = pkt.seq
        if self._next is None:
            self._next = seq
        if seq in self._seen:
            self.stats.duplicates += 1
            return out
        if seq < self._next:
            # Position already committed: late arrival, never rewrite.
            self.stats.late_packets += 1
            self._seen.add(seq)
            return out
        self._seen.add(seq)
        self._buf[seq] = pkt
        # Emit while the head is available or the window is full.
        while self._next in self._buf or len(self._buf) >= self.window:
            if self._next in self._buf:
                out.append(Emitted(seq=self._next,
                                   packet=self._buf.pop(self._next)))
                self._next += 1
            else:
                # Find how far the gap runs (up to the next buffered seq).
                nxt = min(s for s in self._buf if s > self._next)
                gap = nxt - self._next
                for s in range(self._next, nxt):
                    out.append(Emitted(seq=s, packet=None))
                self.stats.record_loss(self._next, gap)
                self._next = nxt
        return out

    def flush(self) -> list[Emitted]:
        """Drain remaining buffered packets at end of input."""
        out: list[Emitted] = []
        if self._next is None:
            return out
        while self._buf:
            if self._next in self._buf:
                out.append(Emitted(seq=self._next,
                                   packet=self._buf.pop(self._next)))
                self._next += 1
            else:
                nxt = min(self._buf)
                gap = nxt - self._next
                for s in range(self._next, nxt):
                    out.append(Emitted(seq=s, packet=None))
                self.stats.record_loss(self._next, gap)
                self._next = nxt
        return out


@dataclass(frozen=True)
class Frame:
    index: int
    keyframe: bool
    codec: int
    data: bytes
    confidence: float  # 1.0 complete, (0,1) partial, 0.0 unrecoverable
    timestamp: int  # extended RTP timestamp of first contributing packet


class FrameReassembler:
    """Rebuilds frames from ordered (possibly lossy) payload chunks."""

    def __init__(self) -> None:
        self._parts: dict[int, dict] = {}

    def push(self, emitted: Emitted) -> Frame | None:
        if emitted.packet is None:
            return None  # loss slot; owning frame completes as partial
        hdr, chunk = unpack_payload(emitted.packet.payload)
        slot = self._parts.setdefault(hdr.frame_index, {
            "parts": hdr.parts, "keyframe": hdr.keyframe,
            "codec": hdr.codec, "chunks": {}, "ts": emitted.packet.timestamp,
        })
        slot["chunks"][hdr.part] = chunk
        slot["ts"] = min(slot["ts"], emitted.packet.timestamp)
        if len(slot["chunks"]) < slot["parts"]:
            return None
        data = b"".join(slot["chunks"][i] for i in range(slot["parts"]))
        del self._parts[hdr.frame_index]
        return Frame(index=hdr.frame_index, keyframe=slot["keyframe"],
                     codec=slot["codec"], data=data, confidence=1.0,
                     timestamp=slot["ts"])

    def finish(self, expected_frames: int | None = None) -> list[Frame]:
        """At end of stream, emit incomplete frames as partial/unrecoverable."""
        out: list[Frame] = []
        for index in sorted(self._parts):
            slot = self._parts[index]
            got = len(slot["chunks"])
            total = slot["parts"]
            confidence = got / total if total else 0.0
            data = b"".join(slot["chunks"][i] for i in sorted(slot["chunks"]))
            out.append(Frame(index=index, keyframe=slot["keyframe"],
                             codec=slot["codec"], data=data,
                             confidence=confidence,
                             timestamp=slot["ts"]))
        self._parts.clear()
        return out
