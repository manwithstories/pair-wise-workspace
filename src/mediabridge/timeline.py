"""Shared time-base handling for every stage that touches timestamps.

The container hands us integer timestamps expressed in a per-stream
``time_base``.  Two streams rarely share a time base (typical MOV: video
``1/15360`` or ``1/90000``, audio ``1/48000``), and damaged archives add
non-zero starts, duplicates, reversals, gaps, wraps and VFR jitter on top.
This module is the single place that converts between "ticks" and "seconds"
and decides what a timestamp *should* be after repair.

Design notes
------------
* All timestamp arithmetic uses :class:`fractions.Fraction`.  Values are
  rounded only when written to a container or the index, never twice.
* :class:`TrackClock` is a pure, serialisable state machine per track.
  ``observe`` returns a :class:`RepairDecision`: keep the timestamp, rewrite
  it with a prediction, or raise :class:`TimestampToleranceError` when the
  discontinuity is structural.
* The clock state is persisted in the checkpoint at every committed chunk so
  a resumed run replays exactly the same decisions as an uninterrupted one.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from typing import Iterable

from .errors import TimestampToleranceError

#: Absolute floor for repaired timestamps; container writers dislike negative
#: dts and AAC priming (-1024 ticks) is the only common negative case.
ORIGIN_FLOOR = Fraction(0)

#: A jump larger than this many seconds is structural, not clock noise.
DEFAULT_TOLERANCE = Fraction(5, 1)

#: Consecutive non-increasing timestamps repaired before a track is declared
#: unrecoverable.  A frozen encoder clock can emit a long run of identical
#: dts; each is repaired by prediction, so the bound only exists to stop an
#: endless garbage stream from being "repaired" forever.
DEFAULT_MAX_CONSECUTIVE_REVERSALS = 256

#: Timestamp wrap modulus shared by MOV/MP4 32-bit tfdt/baseMediaDecodeTime.
WRAP_MODULUS = 1 << 32

#: Relative slack when deciding whether a jump is a 32-bit wrap.
WRAP_SLACK = Fraction(1, 100)


def seconds(ticks: int, time_base: Fraction) -> Fraction:
    """Exact conversion from ticks in ``time_base`` to seconds."""
    return Fraction(ticks) * time_base


def to_ticks(value: Fraction, time_base: Fraction) -> int:
    """Round a duration in seconds to the nearest tick in ``time_base``."""
    scaled = Fraction(value) / time_base
    if scaled < 0:
        return -int(-scaled + Fraction(1, 2))
    return int(scaled + Fraction(1, 2))


def rescale(ticks: int, source_time_base: Fraction, target_time_base: Fraction) -> int:
    """Convert ticks between time bases, rounding to nearest tick."""
    return to_ticks(seconds(ticks, source_time_base), target_time_base)


def parse_time_base(value: object) -> Fraction:
    """Accept a Fraction, a number, a ``[num, den]`` pair or ``"num/den"``."""
    if isinstance(value, Fraction):
        return value
    if isinstance(value, (list, tuple)) and len(value) == 2:
        return Fraction(int(value[0]), int(value[1]))
    if isinstance(value, str) and "/" in value:
        num, den = value.split("/", 1)
        return Fraction(int(num), int(den))
    if isinstance(value, (int, float)):
        return Fraction(value).limit_denominator(1_000_000)
    raise TypeError(f"cannot interpret time base {value!r}")


def format_time_base(tb: Fraction) -> list[int]:
    return [tb.numerator, tb.denominator]


def dump_fraction(value: Fraction) -> list[int]:
    return [value.numerator, value.denominator]


def load_fraction(value: object) -> Fraction:
    if isinstance(value, (list, tuple)) and len(value) == 2:
        return Fraction(int(value[0]), int(value[1]))
    return Fraction(str(value))


@dataclass(frozen=True)
class TrackSpec:
    """Static description of one stream, frozen into checkpoint and index."""

    stream_index: int
    track_index: int
    kind: str  # "video" | "audio"
    codec: str
    time_base: Fraction
    start_time: int | None = None
    average_rate: Fraction | None = None
    sample_rate: int | None = None
    channels: int | None = None
    keyframe_interval: int | None = None

    def as_dict(self) -> dict:
        payload = {
            "stream_index": self.stream_index,
            "track_index": self.track_index,
            "kind": self.kind,
            "codec": self.codec,
            "time_base": format_time_base(self.time_base),
        }
        if self.start_time is not None:
            payload["start_time"] = self.start_time
        if self.average_rate is not None:
            payload["average_rate"] = dump_fraction(self.average_rate)
        if self.sample_rate is not None:
            payload["sample_rate"] = self.sample_rate
        if self.channels is not None:
            payload["channels"] = self.channels
        if self.keyframe_interval is not None:
            payload["keyframe_interval"] = self.keyframe_interval
        return payload

    @classmethod
    def from_dict(cls, payload: dict) -> "TrackSpec":
        rate = payload.get("average_rate")
        return cls(
            stream_index=payload["stream_index"],
            track_index=payload["track_index"],
            kind=payload["kind"],
            codec=payload["codec"],
            time_base=parse_time_base(payload["time_base"]),
            start_time=payload.get("start_time"),
            average_rate=load_fraction(rate) if rate else None,
            sample_rate=payload.get("sample_rate"),
            channels=payload.get("channels"),
            keyframe_interval=payload.get("keyframe_interval"),
        )

    def nominal_interval(self) -> Fraction | None:
        """Expected spacing between packets in seconds, when known."""
        if self.kind == "video" and self.average_rate:
            return Fraction(1, 1) / self.average_rate
        if self.kind == "audio" and self.sample_rate:
            return Fraction(1024, self.sample_rate)
        return None


@dataclass
class TimelineProfile:
    """Repair rules persisted verbatim so resume cannot drift."""

    tolerance_seconds: Fraction = DEFAULT_TOLERANCE
    max_consecutive_reversals: int = DEFAULT_MAX_CONSECUTIVE_REVERSALS
    unify_origin: bool = True

    def as_dict(self) -> dict:
        return {
            "tolerance_seconds": dump_fraction(self.tolerance_seconds),
            "max_consecutive_reversals": self.max_consecutive_reversals,
            "unify_origin": self.unify_origin,
        }

    @classmethod
    def from_dict(cls, payload: dict) -> "TimelineProfile":
        raw = payload.get("tolerance_seconds", [5, 1])
        return cls(
            tolerance_seconds=load_fraction(raw),
            max_consecutive_reversals=int(payload.get("max_consecutive_reversals", DEFAULT_MAX_CONSECUTIVE_REVERSALS)),
            unify_origin=bool(payload.get("unify_origin", True)),
        )


@dataclass
class PacketTimestamps:
    """A container packet's raw timestamps plus the track it belongs to."""

    track_index: int
    stream_index: int
    packet_index: int
    global_index: int
    dts: int | None
    pts: int | None
    time_base: Fraction
    size: int = 0
    pos: int | None = None
    is_keyframe: bool = False

    @property
    def dts_seconds(self) -> Fraction | None:
        return None if self.dts is None else seconds(self.dts, self.time_base)

    @property
    def pts_seconds(self) -> Fraction | None:
        return None if self.pts is None else seconds(self.pts, self.time_base)


class RepairAction:
    KEEP = "keep"
    REWRITE = "rewrite"


@dataclass
class RepairDecision:
    action: str
    dts: int
    pts: int
    dts_seconds: Fraction
    pts_seconds: Fraction
    note: str = ""
    repaired: bool = False
    #: Set when the raw clock jumped further than the tolerance.  The packet
    #: is still repaired (the timeline is rebased), but the event is reported
    #: with its original timestamps so an operator can find the encoder fault.
    discontinuity: bool = False
    deviation_seconds: Fraction | None = None
    raw_dts: int | None = None
    raw_pts: int | None = None


@dataclass
class TrackClock:
    """Per-track timestamp repair, fully deterministic and serialisable.

    The repaired output time is ``raw - origin``.  Because damaged packets
    are dropped *before* they reach the clock (see ``pipeline``), every
    timestamp the clock sees is a genuine container value; the clock only
    has to cope with the sequence being non-monotonic, non-finite or wrapped.

    Repair rules, in order:

    * First packet anchors the track; negative starts are clamped to zero.
    * A 32-bit wrap (either direction) is *unwrapped*: the raw value gains
      ``+2**32`` so the repaired time continues smoothly.  This is the only
      case where the output is not ``raw - origin``.
    * A duplicate or backwards step is repaired by predicting one nominal
      interval past the last output - the timestamp is not trusted, but the
      packet is kept.
    * A forward jump larger than ``tolerance`` is structural and raises
      :class:`TimestampToleranceError` with the offending track, packet and
      raw values.

    The clock never depends on which chunk it is fed from, so a resumed run
    reproduces every decision of a full run.
    """

    spec: TrackSpec
    tolerance: Fraction
    max_consecutive_reversals: int
    origin: Fraction | None = None
    first_seen: bool = False
    last_out: Fraction | None = None
    last_out_pts: Fraction | None = None
    next_expected: Fraction | None = None
    interval: Fraction | None = None
    intervals: list[Fraction] = field(default_factory=list)
    last_raw_seconds: Fraction | None = None
    last_raw_ticks: int | None = None
    unwrap: int = 0
    #: Added to the raw source time base when the encoder clock jumps, so the
    #: repaired timeline continues without a hole or a backwards step.
    rebase: Fraction = Fraction(0)
    discontinuities: int = 0
    max_discontinuities: int = 64
    consecutive_reversals: int = 0
    packets_seen: int = 0
    repairs: int = 0
    wraps: int = 0
    awaiting_resync: bool = False
    last_note: str = ""

    # -- helpers ----------------------------------------------------------
    def _median_interval(self) -> Fraction | None:
        if not self.intervals:
            return self.interval
        ordered = sorted(self.intervals)
        mid = len(ordered) // 2
        if len(ordered) % 2:
            return ordered[mid]
        return (ordered[mid - 1] + ordered[mid]) / 2

    def _remember_interval(self, delta: Fraction) -> None:
        if delta <= 0:
            return
        self.intervals.append(delta)
        if len(self.intervals) > 64:
            del self.intervals[0]
        self.interval = self._median_interval()

    def _step(self) -> Fraction:
        return self.interval or self.spec.nominal_interval() or Fraction(1, 30)

    def _predict(self) -> Fraction:
        if self.next_expected is not None:
            return self.next_expected
        base = self.last_out if self.last_out is not None else (self.origin or ORIGIN_FLOOR)
        return base + self._step()

    def _emit(self, packet: PacketTimestamps, dts: Fraction, pts: Fraction, note: str,
              *, discontinuity: bool = False, deviation: Fraction | None = None) -> RepairDecision:
        self.last_out = dts
        self.last_out_pts = pts
        self.next_expected = dts + self._step()
        self.last_note = note
        if note:
            self.repairs += 1
        return RepairDecision(
            action=RepairAction.REWRITE if note else RepairAction.KEEP,
            dts=to_ticks(dts, packet.time_base),
            pts=to_ticks(pts, packet.time_base),
            dts_seconds=dts,
            pts_seconds=pts,
            note=note,
            repaired=bool(note),
            discontinuity=discontinuity,
            deviation_seconds=deviation,
            raw_dts=packet.dts,
            raw_pts=packet.pts,
        )

    # -- main state machine ----------------------------------------------
    def observe(self, packet: PacketTimestamps) -> RepairDecision:
        self.packets_seen += 1
        raw_dts = packet.dts_seconds
        raw_pts = packet.pts_seconds
        offset = (raw_pts - raw_dts) if (raw_pts is not None and raw_dts is not None) else Fraction(0)

        if packet.dts is None or raw_dts is None:
            if self.last_out is None and self.origin is None:
                raise TimestampToleranceError(
                    "packet has no timestamps before the track produced any",
                    track_index=packet.track_index,
                    stream_index=packet.stream_index,
                    packet_index=packet.packet_index,
                    context={"global_packet": packet.global_index},
                )
            predicted = self._predict()
            self.last_raw_seconds = predicted + (self.origin or ORIGIN_FLOOR)
            self.last_raw_ticks = packet.dts
            return self._emit(packet, predicted, predicted + offset, "missing timestamp reconstructed")

        effective = packet.dts + self.unwrap
        if self.last_raw_ticks is not None and self._is_wrap(self.last_raw_ticks, packet.dts):
            self.unwrap = 0 if self.unwrap else WRAP_MODULUS
            effective = packet.dts + self.unwrap
            self.wraps += 1
            note_hint = "timestamp wrap repaired"
        else:
            note_hint = ""

        if not self.first_seen:
            self.first_seen = True
            origin = self.origin if self.origin is not None else seconds(effective, packet.time_base)
            self.origin = origin
            out = seconds(effective, packet.time_base) - origin
            note = note_hint
            if out < 0:
                out = ORIGIN_FLOOR
                note = note or "negative start clamped to origin"
            self.last_raw_seconds = raw_dts
            self.last_raw_ticks = packet.dts
            self.next_expected = out + self._step()
            self.last_out = out
            self.last_out_pts = out + offset
            self.last_note = note
            if note:
                self.repairs += 1
            return RepairDecision(
                action=RepairAction.REWRITE if note else RepairAction.KEEP,
                dts=to_ticks(out, packet.time_base),
                pts=to_ticks(out + offset, packet.time_base),
                dts_seconds=out,
                pts_seconds=out + offset,
                note=note,
                repaired=bool(note),
            )

        out_dts = seconds(effective, packet.time_base) - (self.origin or ORIGIN_FLOOR) + self.rebase
        raw_delta = out_dts - (self.last_out if self.last_out is not None else out_dts)
        self.last_raw_ticks = packet.dts
        self.last_raw_seconds = raw_dts

        if self.awaiting_resync:
            # Packets were dropped between this track's previous packet and
            # this one (damage repair).  Keep the source timeline position
            # but do not treat the gap as a clock anomaly or let it pollute
            # the interval estimate.
            self.awaiting_resync = False
            self.consecutive_reversals = 0
            return self._emit(packet, out_dts, out_dts + offset, "")

        if note_hint:
            return self._emit(packet, out_dts, out_dts + offset, note_hint)

        if abs(raw_delta) > self.tolerance:
            # Clock discontinuity: the encoder jumped (forward or backward)
            # by more than the tolerance.  Rebase the whole track so the
            # repaired timeline continues one nominal step after the last
            # output, and report the original timestamps.
            self.discontinuities += 1
            if self.discontinuities > self.max_discontinuities:
                raise TimestampToleranceError(
                    "too many timestamp discontinuities to repair reliably",
                    track_index=packet.track_index,
                    stream_index=packet.stream_index,
                    packet_index=packet.packet_index,
                    dts=packet.dts,
                    pts=packet.pts,
                    time_base=format_time_base(packet.time_base),
                    deviation=float(raw_delta),
                    context={"discontinuities": self.discontinuities,
                             "raw_dts": packet.dts, "raw_pts": packet.pts},
                )
            target = self._predict()
            self.rebase += target - out_dts
            out_dts = target
            self.consecutive_reversals = 0
            direction = "forward" if raw_delta > 0 else "backwards"
            return self._emit(packet, out_dts, out_dts + offset,
                              f"{direction} clock jump rebased", discontinuity=True,
                              deviation=raw_delta)

        if raw_delta <= 0:
            self.consecutive_reversals += 1
            if self.consecutive_reversals > self.max_consecutive_reversals:
                raise TimestampToleranceError(
                    "timestamp sequence reverses more often than the tolerance allows",
                    track_index=packet.track_index,
                    stream_index=packet.stream_index,
                    packet_index=packet.packet_index,
                    dts=packet.dts,
                    pts=packet.pts,
                    time_base=format_time_base(packet.time_base),
                    context={
                        "raw_dts": packet.dts,
                        "raw_pts": packet.pts,
                        "consecutive_reversals": self.consecutive_reversals,
                    },
                )
            predicted = self._predict()
            note = "duplicate timestamp" if raw_delta == 0 else "backwards timestamp"
            return self._emit(packet, predicted, predicted + offset, note)

        self.consecutive_reversals = 0
        self._remember_interval(raw_delta)
        return self._emit(packet, out_dts, out_dts + offset, "")

    @staticmethod
    def _is_wrap(previous_ticks: int, current_ticks: int) -> bool:
        """True when consecutive raw ticks jumped by roughly the 32-bit modulus."""
        span = current_ticks - previous_ticks
        return abs(span) >= int(WRAP_MODULUS * (1 - WRAP_SLACK))

    # -- persistence ------------------------------------------------------
    def state_dict(self) -> dict:
        return {
            "stream_index": self.spec.stream_index,
            "origin": dump_fraction(self.origin) if self.origin is not None else None,
            "first_seen": self.first_seen,
            "last_out": dump_fraction(self.last_out) if self.last_out is not None else None,
            "last_out_pts": dump_fraction(self.last_out_pts) if self.last_out_pts is not None else None,
            "next_expected": dump_fraction(self.next_expected) if self.next_expected is not None else None,
            "interval": dump_fraction(self.interval) if self.interval is not None else None,
            "intervals": [dump_fraction(v) for v in self.intervals],
            "last_raw_seconds": dump_fraction(self.last_raw_seconds) if self.last_raw_seconds is not None else None,
            "last_raw_ticks": self.last_raw_ticks,
            "unwrap": self.unwrap,
            "rebase": dump_fraction(self.rebase),
            "discontinuities": self.discontinuities,
            "consecutive_reversals": self.consecutive_reversals,
            "packets_seen": self.packets_seen,
            "repairs": self.repairs,
            "wraps": self.wraps,
            "awaiting_resync": self.awaiting_resync,
            "last_note": self.last_note,
        }

    def restore_state(self, payload: dict) -> None:
        self.first_seen = bool(payload.get("first_seen"))
        for name in ("origin", "last_out", "last_out_pts", "next_expected", "interval", "last_raw_seconds"):
            raw = payload.get(name)
            setattr(self, name, load_fraction(raw) if raw is not None else None)
        self.intervals = [load_fraction(v) for v in payload.get("intervals", [])]
        self.last_raw_ticks = payload.get("last_raw_ticks")
        self.unwrap = int(payload.get("unwrap", 0))
        self.rebase = load_fraction(payload.get("rebase", [0, 1]))
        self.discontinuities = int(payload.get("discontinuities", 0))
        self.consecutive_reversals = int(payload.get("consecutive_reversals", 0))
        self.packets_seen = int(payload.get("packets_seen", 0))
        self.repairs = int(payload.get("repairs", 0))
        self.wraps = int(payload.get("wraps", 0))
        self.awaiting_resync = bool(payload.get("awaiting_resync", False))
        self.last_note = payload.get("last_note", "")

    def set_origin(self, origin: Fraction) -> None:
        self.origin = origin

    def resync(self) -> None:
        """Note that packets were dropped before the next observation.

        Called after damage repair skips packets.  The next packet keeps its
        source timeline position but bypasses the forward-jump check and the
        interval estimate, so a skipped run is not mistaken for a clock
        discontinuity.
        """
        self.awaiting_resync = True
        self.consecutive_reversals = 0

def build_clocks(tracks: Iterable[TrackSpec], profile: TimelineProfile) -> dict[int, TrackClock]:
    return {
        spec.stream_index: TrackClock(
            spec=spec,
            tolerance=profile.tolerance_seconds,
            max_consecutive_reversals=profile.max_consecutive_reversals,
        )
        for spec in tracks
    }
