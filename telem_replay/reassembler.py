"""Bounded sliding-window reassembly of fragmented frames.

Contract
--------
* Fragments are grouped per ``session_id`` and keyed by ``seq``.
* A frame is emitted only once every one of its ``frag_count`` fragments has
  arrived, and frames are emitted in ascending ``seq`` order.
* A frame that completes ahead of its turn is parked in a bounded **delay
  queue**; the head is released as soon as the preceding sequence number
  arrives, so a single gap does not stall the stream.
* Memory is bounded by ``window_size`` in-flight frames per session and
  ``max_sessions`` tracked sessions, independent of input size.

Safe resume offset
------------------
The pipeline cannot checkpoint the live read cursor: fragments read but not yet
committed would be skipped on resume and their frames lost.  So every retained
line contributes its **start byte offset**, and the window reports the minimum
over everything it still holds.  Persisting that value (together with each
session's committed ``last_seq``) makes replay exact.
"""

from __future__ import annotations

from collections import OrderedDict, deque
from typing import Deque, Dict, List, Optional, Tuple

from .protocol import Frame, ProtocolError, frame_crc

DEFAULT_WINDOW = 256
DEFAULT_DELAY_QUEUE = 512


class _Slot:
    """Accumulator for one in-flight (possibly fragmented) frame."""

    __slots__ = ("seq", "frag_count", "total_len", "cover", "crc", "chunks",
                 "received", "bytes_in", "first_offset")

    def __init__(self, seq: int, frag_count: int, total_len: int, cover: bytes,
                 crc: int, first_offset: int) -> None:
        self.seq = seq
        self.frag_count = frag_count
        self.total_len = total_len
        self.cover = cover
        self.crc = crc
        self.chunks: Dict[int, bytes] = {}
        self.received = 0
        self.bytes_in = 0
        self.first_offset = first_offset

    def add(self, frag_index: int, body: bytes) -> None:
        if frag_index in self.chunks:
            return
        self.chunks[frag_index] = body
        self.received += 1
        self.bytes_in += len(body)

    @property
    def complete(self) -> bool:
        return self.received == self.frag_count

    def assemble(self) -> bytes:
        return b"".join(self.chunks[i] for i in range(self.frag_count))


class SessionReassembler:
    """Reassembles frames for a single ``session_id``."""

    __slots__ = ("session_id", "window_size", "delay_limit", "warnings",
                 "_slots", "_delay", "_late", "_next_seq", "_anchored",
                 "_resumed", "_evicted", "_overflow")

    def __init__(self, session_id: str, window_size: int, delay_limit: int,
                 warnings: List[dict]) -> None:
        self.session_id = session_id
        self.window_size = window_size
        self.delay_limit = delay_limit
        self.warnings = warnings
        self._slots: "OrderedDict[int, _Slot]" = OrderedDict()
        # seq -> assembled body, awaiting a contiguous release point.
        self._delay: "OrderedDict[int, bytes]" = OrderedDict()
        # Frames that arrived *below* the release cursor during warm-up, see
        # _anchor() for why these are kept rather than dropped.
        self._late: "OrderedDict[int, bytes]" = OrderedDict()
        self._next_seq = 0
        self._anchored = False
        self._resumed = False
        self._evicted = 0
        self._overflow = 0

    @property
    def evicted(self) -> int:
        return self._evicted

    @property
    def pending(self) -> int:
        return len(self._delay)

    def _anchor(self, seq: int) -> None:
        """Latch the first sequence number we see as the release origin.

        A log may legitimately begin mid-stream, so there is no defined starting
        sequence number; anchoring on the first observed seq keeps the opening
        frame from being trapped in the delay queue forever.  The consequence is
        that a frame slightly *below* the anchor may still turn up afterwards
        (seq 1 observed before seq 0), so those are held in ``_late`` and
        released at flush rather than discarded.  After a resume the anchor is
        authoritative, so anything at or below it is a true duplicate.
        """
        if not self._anchored:
            self._anchored = True
            self._next_seq = seq

    def _evict_oldest(self) -> None:
        """Drop the least-recently-touched incomplete frame to make room."""
        seq, slot = self._slots.popitem(last=False)
        self._evicted += 1
        self.warnings.append(
            {
                "kind": "window_evicted",
                "session_id": self.session_id,
                "seq": seq,
                "received": slot.received,
                "frag_count": slot.frag_count,
            }
        )

    def _finish(self, seq: int, cover: bytes, body: bytes,
                crc: int) -> List[Tuple[int, bytes]]:
        """Verify CRC and either release now or park for ordered release."""
        actual = frame_crc(cover, body)
        if actual != crc:
            raise ProtocolError(
                f"crc mismatch on seq={seq}: got 0x{actual:08x}, want 0x{crc:08x}"
            )
        if seq == self._next_seq:
            self._next_seq = seq + 1
            out = [(seq, body)]
            out.extend(self._release_ready())
            return out
        if seq < self._next_seq:
            if self._resumed:
                return []  # authoritative cursor: a genuine duplicate
            # Warm-up: a frame from just before the anchor.  Keep it.
            self._late.setdefault(seq, body)
            return []
        if seq not in self._delay:
            if len(self._delay) >= self.delay_limit:
                # Delay queue is full: the gap ahead of us is not going to be
                # filled soon.  Release in order rather than grow without bound.
                self._overflow += 1
                self.warnings.append(
                    {
                        "kind": "delay_queue_overflow",
                        "session_id": self.session_id,
                        "seq": seq,
                        "limit": self.delay_limit,
                    }
                )
                self._delay[seq] = body
                return self._release_oldest()
            self._delay[seq] = body
        return []

    def _release_ready(self) -> List[Tuple[int, bytes]]:
        """Release parked frames that are now contiguous with the cursor."""
        out: List[Tuple[int, bytes]] = []
        while True:
            nxt = self._delay.pop(self._next_seq, None)
            if nxt is None:
                return out
            out.append((self._next_seq, nxt))
            self._next_seq += 1

    def _release_oldest(self) -> List[Tuple[int, bytes]]:
        """Overflow path: release the lowest parked seq and slide the cursor."""
        out: List[Tuple[int, bytes]] = []
        while self._delay:
            seq = min(self._delay)
            body = self._delay.pop(seq)
            if seq < self._next_seq:
                continue
            self._next_seq = seq + 1
            out.append((seq, body))
            out.extend(self._release_ready())
        return out

    def feed(self, frame: Frame, byte_offset: int) -> List[Tuple[int, bytes]]:
        """Offer one fragment; return ``(seq, body)`` frames released, in order."""
        self._anchor(frame.seq)

        if frame.seq < self._next_seq:
            if self._resumed:
                # The cursor came from a checkpoint, so it is authoritative and
                # this really is a replayed boundary line.
                return []
            # Warm-up: a frame from just below the anchor.  Let _finish stash it
            # in _late so it is still delivered at flush.

        if frame.frag_count == 1 and not frame.chunked():
            return self._finish(frame.seq, frame.cover, frame.body, frame.crc)

        slot = self._slots.get(frame.seq)
        if slot is None:
            while len(self._slots) >= self.window_size:
                self._evict_oldest()
            slot = _Slot(frame.seq, frame.frag_count, frame.total_len,
                         frame.cover, frame.crc, byte_offset)
            self._slots[frame.seq] = slot
        else:
            if (frame.frag_count != slot.frag_count
                    or frame.total_len != slot.total_len
                    or frame.crc != slot.crc
                    or frame.cover != slot.cover):
                raise ProtocolError(
                    f"inconsistent fragment metadata for seq={frame.seq}: a "
                    f"reassembled frame must agree on crc cover and frame crc"
                )
            self._slots.move_to_end(frame.seq)

        slot.add(frame.frag_index, frame.body)
        if not slot.complete:
            return []

        self._slots.pop(frame.seq, None)
        body = slot.assemble()
        if len(body) != slot.total_len:
            raise ProtocolError(
                f"seq={frame.seq}: reassembled {len(body)} bytes, "
                f"header declared {slot.total_len}"
            )
        return self._finish(frame.seq, slot.cover, body, slot.crc)

    def drain(self) -> List[Tuple[int, bytes]]:
        """Flush parked and late frames in ascending seq order (end of input)."""
        if not self._delay and not self._late:
            return []
        out: List[Tuple[int, bytes]] = []
        # Late frames all sit below the cursor by construction, so they are
        # released unconditionally; parked frames are released only if they are
        # still ahead of it.
        for seq in sorted(self._late):
            out.append((seq, self._late[seq]))
        self._late.clear()
        for seq in sorted(self._delay):
            if seq < self._next_seq:
                continue
            out.append((seq, self._delay[seq]))
            self._next_seq = seq + 1
        self._delay.clear()
        out.sort(key=lambda e: e[0])
        return out

    def restore(self, last_seq: int) -> None:
        """Re-anchor to a committed sequence number after a resume."""
        self._next_seq = last_seq + 1
        self._anchored = True
        self._resumed = True
        self._slots.clear()
        self._delay.clear()
        self._late.clear()

    def min_retained_offset(self) -> Optional[int]:
        """Smallest start offset of any line still held in memory.

        ``None`` means this session holds nothing in flight, so every line it
        has already contributed is fully accounted for by ``last_seq``.
        """
        if not self._slots:
            return None
        return min(slot.first_offset for slot in self._slots.values())


class ReassemblyManager:
    """Owns one :class:`SessionReassembler` per session, with a global cap.

    ``window_size`` bounds one busy session; ``max_sessions`` bounds total
    tracked sessions so that logs with one line per session cannot grow memory
    without limit.
    """

    __slots__ = ("window_size", "max_sessions", "delay_limit", "_sessions",
                 "warnings", "evicted_total", "overflow_total", "_ejected")

    def __init__(self, window_size: int = DEFAULT_WINDOW,
                 max_sessions: int = 64,
                 delay_limit: int = DEFAULT_DELAY_QUEUE) -> None:
        if window_size < 1:
            raise ValueError("window_size must be >= 1")
        self.window_size = window_size
        self.max_sessions = max_sessions
        self.delay_limit = delay_limit
        self._sessions: "OrderedDict[str, SessionReassembler]" = OrderedDict()
        self.warnings: List[dict] = []
        self.evicted_total = 0
        self.overflow_total = 0
        self._ejected: Deque[Tuple[str, SessionReassembler]] = deque()

    def _get(self, session_id: str) -> SessionReassembler:
        s = self._sessions.get(session_id)
        if s is None:
            s = SessionReassembler(session_id, self.window_size,
                                   self.delay_limit, self.warnings)
            self._sessions[session_id] = s
            if len(self._sessions) > self.max_sessions:
                # Park the least-recently-used idle session.  Its partial frames
                # are dropped (and counted) rather than growing memory forever.
                sid, old = self._sessions.popitem(last=False)
                self.evicted_total += old.evicted
                self.overflow_total += old._overflow
                self.warnings.append(
                    {"kind": "session_ejected", "session_id": sid,
                     "pending": len(old._delay)}
                )
        else:
            self._sessions.move_to_end(session_id)
        return s

    def feed(self, session_id: str, frame: Frame,
             byte_offset: int) -> List[Tuple[int, bytes]]:
        return self._get(session_id).feed(frame, byte_offset)

    def restore(self, session_id: str, last_seq: int) -> None:
        self._get(session_id).restore(last_seq)

    def flush(self) -> List[Tuple[str, Tuple[int, bytes]]]:
        """Flush every session's delay queue in ascending seq order."""
        out: List[Tuple[str, Tuple[int, bytes]]] = []
        for session_id, s in self._sessions.items():
            for seq, body in s.drain():
                out.append((session_id, seq, body))
        return out

    def session_offsets(self) -> Dict[str, Optional[int]]:
        """Per-session oldest retained offset (``None`` when idle)."""
        return {sid: s.min_retained_offset() for sid, s in self._sessions.items()}

    def safe_offset(self) -> Optional[int]:
        """Minimum start offset across every session's retained fragments."""
        return min(
            (o for o in self.session_offsets().values() if o is not None),
            default=None,
        )

    def stats(self) -> Dict[str, int]:
        inflight = sum(len(s._slots) for s in self._sessions.values())
        parked = sum(len(s._delay) + len(s._late) for s in self._sessions.values())
        return {
            "sessions_tracked": len(self._sessions),
            "frames_inflight": inflight,
            "frames_parked": parked,
            # Includes frames still held by sessions that were ejected, so this
            # is a lifetime total rather than a snapshot of live state.
            "evicted": self.evicted_total + sum(s.evicted for s in self._sessions.values()),
            "delay_overflow": self.overflow_total,
        }
