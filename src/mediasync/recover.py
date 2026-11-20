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
from typing import Callable, Iterable, Sequence

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


def _hole_bounds(
    missing: MissingSequence,
    ordered: Sequence[TransportPacket],
) -> tuple[int, int]:
    """Media bounds of a missing sequence, from its surviving neighbours."""

    before = [p for p in ordered if p.seq < missing.seq]
    after = [p for p in ordered if p.seq > missing.seq]
    if before and after:
        start = before[-1].pts_ticks + before[-1].duration_ticks
        end = after[0].pts_ticks
    elif before:
        start = before[-1].pts_ticks + before[-1].duration_ticks
        end = start + max(before[-1].duration_ticks, 1)
    elif after:
        end = after[0].pts_ticks
        start = max(0, end - max(after[0].duration_ticks, 1))
    else:
        start, end = 0, 1
    if end <= start:
        end = start + 1
    return start, end


def _hole_tick(
    missing: MissingSequence, ordered: Sequence[TransportPacket]
) -> int:
    """Representative tick used to assign a hole to exactly one interval."""

    before = [p for p in ordered if p.seq < missing.seq]
    after = [p for p in ordered if p.seq > missing.seq]
    if before and after:
        return (before[-1].pts_ticks + after[0].pts_ticks) // 2
    if before:
        return before[-1].pts_ticks + before[-1].duration_ticks
    if after:
        return after[0].pts_ticks
    return 0


def _boundary_repair(
    interval: Interval,
    ordered: Sequence[TransportPacket],
    truncated_seqs: Iterable[int],
    config: RecoveryConfig,
) -> BoundaryRepair | None:
    """Clamp a damaged interval end to the last fully available packet."""

    truncated = sorted(truncated_seqs)
    if not truncated or not ordered:
        return None
    first_bad = truncated[0]
    usable = [p for p in ordered if p.seq < first_bad]
    if not usable:
        return None
    repaired_end = min(
        interval.end_ticks,
        usable[-1].pts_ticks + usable[-1].duration_ticks,
    )
    if repaired_end <= interval.start_ticks:
        return None
    shortfall = max(0, interval.end_ticks - repaired_end)
    if shortfall > config.boundary_budget_ticks:
        return None
    dropped = sum(
        1 for p in ordered if p.seq >= first_bad
    )
    return BoundaryRepair(
        interval_id=interval.interval_id,
        original_end_ticks=interval.end_ticks,
        repaired_end_ticks=repaired_end,
        dropped_bytes=dropped,
        reason="truncated-index-boundary",
    )


def recover_interval(
    interval: Interval,
    *,
    ordered: Sequence[TransportPacket],
    missing: Sequence[MissingSequence],
    truncated_seqs: Iterable[int] = (),
    config: RecoveryConfig | None = None,
    oracle: RetransmitOracle | None = None,
    resolver: Callable[[int], TransportPacket | None] | None = None,
) -> TrackRecovery:
    """Recover one interval by retransmission, boundary repair or gap marking.

    A hole belongs to the interval that contains its representative tick, so
    splitting a track into segments never double-counts one loss.  When a
    retransmission succeeds and a ``resolver`` is supplied, the restored
    packet is spliced back into the ordered stream; without a resolver the
    packet is only accounted for, never silently invented.
    """

    config = config or RecoveryConfig()
    oracle = oracle or RetransmitOracle(config)
    if interval.start_ticks >= interval.end_ticks:
        raise PlanError(f"{interval.interval_id}: invalid interval bounds")
    working = list(ordered)
    in_span = [
        packet for packet in working
        if packet.pts_ticks < interval.end_ticks
        and packet.pts_ticks + packet.duration_ticks > interval.start_ticks
    ]
    owned = [
        item for item in missing
        if interval.start_ticks <= _hole_tick(item, ordered) < interval.end_ticks
    ]
    recovered_seqs: list[int] = []
    retransmissions: list[Retransmission] = []
    repair: BoundaryRepair | None = None
    for item in owned:
        for attempt in range(1, config.max_retransmit_attempts + 1):
            ok = oracle.attempt(interval.track_id, item.seq, attempt)
            retransmissions.append(
                Retransmission(interval.track_id, item.seq, attempt, ok)
            )
            if not ok:
                continue
            restored = resolver(item.seq) if resolver is not None else None
            recovered_seqs.append(item.seq)
            if restored is not None:
                working.append(restored)
                working.sort(key=lambda packet: packet.seq)
                in_span.append(restored)
                in_span.sort(key=lambda packet: packet.seq)
            break

    truncated = sorted(truncated_seqs)
    if truncated:
        repair = _boundary_repair(interval, in_span, truncated, config)

    end_ticks = repair.repaired_end_ticks if repair else interval.end_ticks
    losses: list[int] = []
    gaps: list[Gap] = []
    for item in owned:
        if item.seq in recovered_seqs:
            continue
        start, end = _hole_bounds(item, ordered)
        start = max(start, interval.start_ticks)
        end = min(end, end_ticks)
        losses.append(item.seq)
        if end <= start:
            end = min(interval.end_ticks, start + 1)
            if end <= start:
                start = max(interval.start_ticks, end - 1)
        if end > start:
            gaps.append(Gap(interval.track_id, start, end, "packet-loss"))
    if repair is not None and repair.repaired_end_ticks < interval.end_ticks:
        gaps.append(
            Gap(
                interval.track_id,
                repair.repaired_end_ticks,
                interval.end_ticks,
                "truncated-index-boundary",
            )
        )

    complete = not losses and repair is None
    reason = ""
    if losses and repair is not None:
        reason = "packet-loss-and-truncated-boundary"
    elif losses:
        reason = "packet-loss"
    elif repair is not None:
        reason = "truncated-boundary-repaired"
    segment = Segment(
        interval_id=interval.interval_id,
        track_id=interval.track_id,
        start_ticks=interval.start_ticks,
        end_ticks=end_ticks,
        complete=complete,
        seqs=tuple(packet.seq for packet in in_span),
        recovered_seqs=tuple(sorted(recovered_seqs)),
        loss_seqs=tuple(sorted(losses)),
        reason=reason,
    )
    return TrackRecovery(
        interval=interval,
        segment=segment,
        retransmissions=retransmissions,
        repairs=[repair] if repair else [],
        gaps=gaps,
    )


def recover_timeline(
    timeline: Timeline,
    recoveries: dict[str, TrackRecovery],
    *,
    propagate_incomplete: bool = True,
) -> RecoveryReport:
    """Assemble interval recoveries in dependency order and propagate damage.

    A dependent interval is downgraded to incomplete when any prerequisite is
    incomplete, so a manifest can never claim a synchronized output that rests
    on an unrepaired gap.  The downgrade is recorded in ``propagated``.
    """

    timeline.validate()
    missing = [key for key in timeline.intervals if key not in recoveries]
    if missing:
        raise PlanError(
            "recovery missing for intervals: " + ", ".join(sorted(missing))
        )
    report = RecoveryReport()
    done: dict[str, Segment] = {}
    for interval in timeline.recovery_order():
        source = recoveries[interval.interval_id]
        segment = source.segment
        report.retransmissions.extend(source.retransmissions)
        report.repairs.extend(source.repairs)
        report.gaps.extend(source.gaps)
        if propagate_incomplete:
            broken = [
                name for name in interval.depends_on
                if name in done and not done[name].complete
            ]
            if broken and segment.complete:
                segment = Segment(
                    interval_id=segment.interval_id,
                    track_id=segment.track_id,
                    start_ticks=segment.start_ticks,
                    end_ticks=segment.end_ticks,
                    complete=False,
                    seqs=segment.seqs,
                    recovered_seqs=segment.recovered_seqs,
                    loss_seqs=segment.loss_seqs,
                    reason="prerequisite-incomplete",
                )
                report.propagated.append(
                    {
                        "interval_id": interval.interval_id,
                        "blocked_by": sorted(broken),
                    }
                )
                report.gaps.append(
                    Gap(
                        segment.track_id,
                        segment.start_ticks,
                        max(segment.start_ticks + 1, segment.end_ticks),
                        "prerequisite-incomplete",
                    )
                )
        done[interval.interval_id] = segment
        report.segments.append(segment)
    report.segments.sort(key=lambda item: (item.track_id, item.start_ticks))
    return report
