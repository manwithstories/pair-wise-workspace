"""Recovery planning: retransmission, boundary repair and mandatory gap marking.

Recovery never silently fills a hole.  A missing transport sequence is either
restored by a simulated retransmission (and recorded as such) or it stays a
explicit :class:`~mediasync.errors.Gap`.  A damaged index boundary may be
recovered by clamping to the last fully indexed packet, but only when the
shortfall stays within a bounded budget.  Every interval ends up ``complete``
or ``incomplete`` with a reason, and dependents of an incomplete interval are
never reported as complete.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from typing import Iterable, Sequence

from .errors import Gap, InputError, PlanError
from .jitter import MissingSequence, TransportPacket
from .timeline import Interval, Timeline

#: Recovery may not invent more than this much media at a boundary.
DEFAULT_BOUNDARY_BUDGET_TICKS = 2 * 90_000


@dataclass(frozen=True)
class RecoveryConfig:
    max_retransmit_attempts: int = 2
    retransmit_success_rate: float = 0.85
    boundary_budget_ticks: int = DEFAULT_BOUNDARY_BUDGET_TICKS
    seed: int = 0

    def __post_init__(self) -> None:
        if self.max_retransmit_attempts < 0:
            raise InputError("retransmit attempts cannot be negative")
        if not 0.0 <= self.retransmit_success_rate <= 1.0:
            raise InputError("retransmit success rate must be within [0, 1]")
        if self.boundary_budget_ticks < 0:
            raise InputError("boundary budget cannot be negative")


@dataclass(frozen=True)
class Retransmission:
    """One simulated retransmission decision for a missing sequence."""

    track_id: str
    seq: int
    attempt: int
    succeeded: bool

    def to_dict(self) -> dict[str, object]:
        return {
            "track_id": self.track_id,
            "seq": self.seq,
            "attempt": self.attempt,
            "succeeded": self.succeeded,
        }


@dataclass(frozen=True)
class BoundaryRepair:
    """A damaged index boundary clamped back to recoverable media."""

    interval_id: str
    original_end_ticks: int
    repaired_end_ticks: int
    dropped_bytes: int
    reason: str

    @property
    def shortfall_ticks(self) -> int:
        return max(0, self.original_end_ticks - self.repaired_end_ticks)

    def to_dict(self) -> dict[str, object]:
        return {
            "interval_id": self.interval_id,
            "original_end_ticks": self.original_end_ticks,
            "repaired_end_ticks": self.repaired_end_ticks,
            "shortfall_ticks": self.shortfall_ticks,
            "dropped_bytes": self.dropped_bytes,
            "reason": self.reason,
        }


@dataclass(frozen=True)
class Segment:
    """The recoverable span of one timeline interval."""

    interval_id: str
    track_id: str
    start_ticks: int
    end_ticks: int
    complete: bool
    seqs: tuple[int, ...] = ()
    recovered_seqs: tuple[int, ...] = ()
    loss_seqs: tuple[int, ...] = ()
    reason: str = ""

    @property
    def duration_ticks(self) -> int:
        return max(0, self.end_ticks - self.start_ticks)

    @property
    def partial(self) -> bool:
        return not self.complete

    def to_dict(self) -> dict[str, object]:
        return {
            "interval_id": self.interval_id,
            "track_id": self.track_id,
            "start_ticks": self.start_ticks,
            "end_ticks": self.end_ticks,
            "duration_ticks": self.duration_ticks,
            "complete": self.complete,
            "packets": len(self.seqs),
            "recovered": len(self.recovered_seqs),
            "lost": len(self.loss_seqs),
            "reason": self.reason,
        }


@dataclass
class TrackRecovery:
    """Recovery outcome for one interval before dependency propagation."""

    interval: Interval
    segment: Segment
    retransmissions: list[Retransmission] = field(default_factory=list)
    repairs: list[BoundaryRepair] = field(default_factory=list)
    gaps: list[Gap] = field(default_factory=list)


@dataclass
class RecoveryReport:
    segments: list[Segment] = field(default_factory=list)
    retransmissions: list[Retransmission] = field(default_factory=list)
    repairs: list[BoundaryRepair] = field(default_factory=list)
    gaps: list[Gap] = field(default_factory=list)
    propagated: list[dict[str, object]] = field(default_factory=list)

    @property
    def recovered(self) -> int:
        return sum(1 for item in self.retransmissions if item.succeeded)

    @property
    def attempts(self) -> int:
        return len(self.retransmissions)

    @property
    def unrecoverable_seqs(self) -> int:
        return sum(len(segment.loss_seqs) for segment in self.segments)

    @property
    def has_unrecoverable(self) -> bool:
        return bool(self.gaps) or any(segment.partial for segment in self.segments)

    def incomplete_tracks(self) -> list[str]:
        return sorted({segment.track_id for segment in self.segments if segment.partial})

    def summary(self) -> dict[str, object]:
        return {
            "segments": len(self.segments),
            "complete": sum(1 for s in self.segments if s.complete),
            "incomplete": sum(1 for s in self.segments if s.partial),
            "retransmit_attempts": self.attempts,
            "recovered": self.recovered,
            "unrecoverable": self.unrecoverable_seqs,
            "gaps": len(self.gaps),
            "boundary_repairs": len(self.repairs),
            "propagated": len(self.propagated),
        }


class RetransmitOracle:
    """Deterministic, order-independent retransmission outcomes.

    The decision is a keyed hash of ``(seed, track, sequence, attempt)`` so a
    rerun with the same seed reproduces every success and failure regardless
    of the order tracks are processed in.
    """

    def __init__(self, config: RecoveryConfig) -> None:
        self.config = config

    def attempt(self, track_id: str, seq: int, attempt: int) -> bool:
        if attempt < 1:
            raise InputError("retransmission attempts are 1-based")
        key = f"{self.config.seed}|{track_id}|{seq}|{attempt}".encode()
        digest = hashlib.blake2b(key, digest_size=8).digest()
        value = int.from_bytes(digest, "big") / float(1 << 64)
        return value < self.config.retransmit_success_rate
