"""Sequence expansion, de-duplication and bounded reordering for transport packets.

Packets carry a wrapping sequence number and a monotonic arrival timestamp.
The de-jitter stage never sleeps: loss is decided from arrival timestamps, so
a rerun with the same input yields exactly the same ordering and statistics.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Iterable, Sequence

from .errors import InputError

DEFAULT_SEQ_WRAP = 1 << 16


@dataclass(frozen=True)
class TransportPacket:
    """One captured packet with wire sequence and arrival time."""

    seq: int
    arrival_ms: int
    data: bytes
    keyframe: bool = False
    pts_ticks: int = 0
    duration_ticks: int = 1

    def __post_init__(self) -> None:
        if self.seq < 0:
            raise InputError(f"negative sequence number {self.seq}")
        if self.arrival_ms < 0:
            raise InputError(f"negative arrival time {self.arrival_ms}")
        if self.duration_ticks < 0:
            raise InputError(f"negative duration {self.duration_ticks}")


@dataclass(frozen=True)
class Conflict:
    """Two arrivals claimed one sequence number with different payloads."""

    seq: int
    first_arrival_ms: int
    second_arrival_ms: int
    first_size: int
    second_size: int

    def to_dict(self) -> dict[str, object]:
        return {
            "seq": self.seq,
            "first_arrival_ms": self.first_arrival_ms,
            "second_arrival_ms": self.second_arrival_ms,
            "first_size": self.first_size,
            "second_size": self.second_size,
        }


@dataclass(frozen=True)
class LatePacket:
    seq: int
    arrival_ms: int
    deadline_ms: int

    @property
    def lateness_ms(self) -> int:
        return self.arrival_ms - self.deadline_ms

    def to_dict(self) -> dict[str, object]:
        return {
            "seq": self.seq,
            "arrival_ms": self.arrival_ms,
            "deadline_ms": self.deadline_ms,
            "lateness_ms": self.lateness_ms,
        }


@dataclass(frozen=True)
class MissingSequence:
    seq: int
    deadline_ms: int
    overtaken_by: int

    def to_dict(self) -> dict[str, object]:
        return {
            "seq": self.seq,
            "deadline_ms": self.deadline_ms,
            "overtaken_by": self.overtaken_by,
        }


@dataclass(frozen=True)
class JitterConfig:
    wait_budget_ms: int = 45
    reorder_window: int = 256
    seq_wrap: int = DEFAULT_SEQ_WRAP
    max_packets: int = 200_000

    def __post_init__(self) -> None:
        if self.wait_budget_ms < 0:
            raise InputError("wait budget cannot be negative")
        if self.reorder_window < 0:
            raise InputError("reorder window cannot be negative")
        if self.seq_wrap <= 1:
            raise InputError("sequence wrap must exceed one")
        if self.max_packets <= 0:
            raise InputError("packet limit must be positive")


@dataclass
class JitterResult:
    ordered: list[TransportPacket] = field(default_factory=list)
    missing: list[MissingSequence] = field(default_factory=list)
    late: list[LatePacket] = field(default_factory=list)
    conflicts: list[Conflict] = field(default_factory=list)
    duplicates: int = 0
    reordered: int = 0
    max_reorder_distance: int = 0

    @property
    def received(self) -> int:
        return len(self.ordered)

    def summary(self) -> dict[str, object]:
        return {
            "received": self.received,
            "missing": len(self.missing),
            "late": len(self.late),
            "duplicates": self.duplicates,
            "conflicts": len(self.conflicts),
            "reordered": self.reordered,
            "max_reorder_distance": self.max_reorder_distance,
        }


def expand_sequences(seqs: Sequence[int], wrap: int = DEFAULT_SEQ_WRAP) -> list[int]:
    """Expand wrapping sequence numbers, tolerating local reordering.

    Each sequence is placed in the epoch nearest the previous value, so a
    single out-of-order packet across the wrap boundary is still recognised
    as a neighbour instead of jumping a full epoch.
    """

    if wrap <= 1:
        raise InputError("sequence wrap must exceed one")
    expanded: list[int] = []
    previous_raw: int | None = None
    previous_value = 0
    for seq in seqs:
        if not isinstance(seq, int) or isinstance(seq, bool):
            raise InputError(f"sequence must be an integer, got {seq!r}")
        if not 0 <= seq < wrap:
            raise InputError(f"sequence {seq} outside transport range [0, {wrap})")
        if previous_raw is None:
            previous_value = seq
            expanded.append(seq)
        else:
            diff = (seq - previous_raw + wrap // 2) % wrap - wrap // 2
            previous_value += diff
            expanded.append(previous_value)
        previous_raw = seq
    return expanded


def dejitter(
    packets: Iterable[TransportPacket],
    config: JitterConfig | None = None,
) -> JitterResult:
    """Reorder packets and adjudicate loss inside a wait budget.

    A gap is declared lost once a later packet has overtaken it and either
    ``wait_budget_ms`` has elapsed since that overtaking arrival or the
    overtaking distance exceeds ``reorder_window``.  A packet that shows up
    after its gap was declared lost is reported as late and dropped, so no
    stage downstream can silently splice a reordered frame back in.
    """

    config = config or JitterConfig()
    arrivals = list(packets)
    if len(arrivals) > config.max_packets:
        raise InputError(f"{len(arrivals)} packets exceed limit {config.max_packets}")
    result = JitterResult()
    if not arrivals:
        return result
    # The de-jitterer is fed arrival order, which is monotonic by construction
    # on a real wire but may tie within a millisecond. A grossly out-of-order
    # log means the caller did not send arrival order at all.
    for previous, current in zip(arrivals, arrivals[1:]):
        if current.arrival_ms < previous.arrival_ms - config.reorder_window:
            raise InputError(
                "packets must be supplied in arrival order "
                f"({previous.arrival_ms} -> {current.arrival_ms})"
            )
    arrivals.sort(key=lambda packet: packet.arrival_ms)
    expanded = expand_sequences([packet.seq for packet in arrivals], config.seq_wrap)
    playhead = expanded[0]
    buffer: dict[int, TransportPacket] = {}
    emitted: dict[int, TransportPacket] = {}
    lost: dict[int, MissingSequence] = {}
    overtaken_at: dict[int, int] = {}
    highest_buffered = playhead - 1

    def expire(now: int, *, final: bool) -> None:
        """Emit contiguous packets, declaring overdue gaps lost."""

        nonlocal playhead
        while True:
            while playhead in buffer:
                emitted[playhead] = buffer.pop(playhead)
                overtaken_at.pop(playhead, None)
                playhead += 1
            if playhead in lost:
                playhead += 1
                continue
            deadline = overtaken_at.get(playhead)
            next_pending = min(buffer) if buffer else None
            if next_pending is not None:
                due = deadline is not None and now >= deadline + config.wait_budget_ms
                far = next_pending - playhead > config.reorder_window
                if due or far:
                    lost[playhead] = MissingSequence(
                        seq=playhead,
                        deadline_ms=(deadline or now) + config.wait_budget_ms,
                        overtaken_by=next_pending,
                    )
                    overtaken_at.pop(playhead, None)
                    playhead += 1
                    continue
                return
            if deadline is not None and (final or now >= deadline + config.wait_budget_ms):
                lost[playhead] = MissingSequence(
                    seq=playhead,
                    deadline_ms=deadline + config.wait_budget_ms,
                    overtaken_by=-1,
                )
                overtaken_at.pop(playhead, None)
                playhead += 1
                continue
            return

    for packet, ext in zip(arrivals, expanded):
        expire(packet.arrival_ms, final=False)
        if ext in lost:
            result.late.append(
                LatePacket(ext, packet.arrival_ms, lost[ext].deadline_ms)
            )
            continue
        if ext in emitted or ext in buffer or ext < playhead:
            result.duplicates += 1
            existing = emitted.get(ext) or buffer.get(ext)
            if existing is not None:
                _check_conflict(result, existing, packet, ext)
            continue
        if ext <= highest_buffered and buffer:
            result.reordered += 1
            result.max_reorder_distance = max(
                result.max_reorder_distance, highest_buffered - ext
            )
        for gap in range(max(highest_buffered + 1, playhead), ext):
            overtaken_at.setdefault(gap, packet.arrival_ms)
        highest_buffered = max(highest_buffered, ext)
        buffer[ext] = packet

    expire(arrivals[-1].arrival_ms, final=True)
    result.ordered = [emitted[ext] for ext in sorted(emitted)]
    result.missing = [lost[ext] for ext in sorted(lost) if ext not in emitted]
    return result


def _check_conflict(
    result: JitterResult,
    first: TransportPacket,
    second: TransportPacket,
    ext: int,
) -> None:
    if first.data == second.data and first.keyframe == second.keyframe:
        return
    result.conflicts.append(
        Conflict(
            seq=ext,
            first_arrival_ms=first.arrival_ms,
            second_arrival_ms=second.arrival_ms,
            first_size=len(first.data),
            second_size=len(second.data),
        )
    )
