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
