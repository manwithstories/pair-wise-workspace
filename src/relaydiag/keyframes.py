"""Keyframe cadence and frame-interval anomaly analysis.

The analyser works on *measured* frame times rather than the nominal frame
rate, which is what lets it tell three situations apart:

* **VFR** -- a frame whose duration is a clean integer fraction/multiple of the
  nominal tick, with no gap in the timeline; expected and harmless;
* **missing frame** -- a gap spanning one or more nominal frame ticks that no
  observed packet covers;
* **bad packet** -- a present packet whose payload is corrupt or whose
  timestamp ordering is wrong (reported by the caller via packet kinds).

Keyframe intervals are measured on real keyframe packets and compared to the
configured GOP so a drifted keyframe cadence is surfaced separately.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .timeline import TimeBase


@dataclass(slots=True)
class FrameEvent:
    """One observed video frame on its track time base."""

    pts: int
    keyframe: bool
    kind: str = "media"
    payload_ok: bool = True


@dataclass(slots=True)
class IntervalAnomaly:
    category: str          # "vfr" | "missing_frame" | "bad_packet"
    pts: int
    expected_pts: int
    delta_ticks: int
    seconds: float
    detail: str


@dataclass(slots=True)
class KeyframeReport:
    nominal_interval_s: float
    measured_intervals_s: list[float]
    mean_interval_s: float
    max_deviation_ms: float
    keyframe_count: int
    gop_anomalies: list[dict] = field(default_factory=list)
    frame_anomalies: list[IntervalAnomaly] = field(default_factory=list)
    vfr_points: list[int] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "nominal_interval_s": self.nominal_interval_s,
            "measured_intervals_s": [round(x, 6) for x in self.measured_intervals_s],
            "mean_interval_s": round(self.mean_interval_s, 6),
            "max_deviation_ms": round(self.max_deviation_ms, 3),
            "keyframe_count": self.keyframe_count,
            "gop_anomalies": self.gop_anomalies,
            "vfr_points_pts": self.vfr_points,
            "frame_anomalies": [
                {
                    "category": a.category,
                    "pts": a.pts,
                    "expected_pts": a.expected_pts,
                    "delta_ticks": a.delta_ticks,
                    "seconds": round(a.seconds, 6),
                    "detail": a.detail,
                }
                for a in self.frame_anomalies
            ],
        }


def analyze_keyframes(events: list[FrameEvent], base: TimeBase,
                      nominal_tick: int, gop: int) -> KeyframeReport:
    """Analyse measured keyframe spacing and per-frame timeline gaps."""
    key_pts = [e.pts for e in events if e.keyframe]
    intervals = [
        base.ticks_to_seconds(b - a)
        for a, b in zip(key_pts, key_pts[1:])
    ]
    nominal_interval = base.ticks_to_seconds(nominal_tick * gop)
    mean = sum(intervals, start=base.ticks_to_seconds(0)) / len(intervals) \
        if intervals else nominal_interval
    gop_anoms = []
    for a, b in zip(key_pts, key_pts[1:]):
        gap_ticks = b - a
        if gap_ticks != nominal_tick * gop:
            gop_anoms.append({
                "from_pts": a, "to_pts": b,
                "interval_s": float(base.ticks_to_seconds(gap_ticks)),
                "expected_s": float(nominal_interval),
            })
    max_dev = max(
        (abs(float(iv) - float(nominal_interval)) for iv in intervals),
        default=0.0,
    )

    frame_anoms, vfr_points = _frame_gaps(events, base, nominal_tick)
    return KeyframeReport(
        float(nominal_interval), [float(x) for x in intervals], float(mean),
        max_dev * 1000.0, len(key_pts), gop_anoms, frame_anoms, vfr_points,
    )


def _frame_gaps(events: list[FrameEvent], base: TimeBase,
                nominal_tick: int) -> tuple[list[IntervalAnomaly], list[int]]:
    anomalies: list[IntervalAnomaly] = []
    vfr: list[int] = []
    # Main media timeline only; side packets (vfr/duplicate/corrupt) are
    # classified explicitly.  Sort by unwrapped PTS so transmission-order
    # reversals (reported separately by the damage layer) cannot fabricate
    # missing-frame gaps in the presentation timeline.
    main = sorted((e for e in events if e.kind == "media"),
                  key=lambda e: e.pts)
    for prev, cur in zip(main, main[1:]):
        delta = cur.pts - prev.pts
        if delta == nominal_tick:
            if not cur.payload_ok:
                anomalies.append(IntervalAnomaly(
                    "bad_packet", cur.pts, prev.pts + nominal_tick, delta,
                    float(base.ticks_to_seconds(delta)), "corrupt payload"))
            continue
        if delta <= 0:
            anomalies.append(IntervalAnomaly(
                "bad_packet", cur.pts, prev.pts + nominal_tick, delta,
                float(base.ticks_to_seconds(delta)),
                "duplicate or reversed timestamp"))
            continue
        ratio = delta / nominal_tick
        if ratio == int(ratio) and ratio >= 2:
            # An exact whole-tick hole means one or more frames never arrived.
            anomalies.append(IntervalAnomaly(
                "missing_frame", cur.pts, prev.pts + nominal_tick, delta,
                float(base.ticks_to_seconds(delta)),
                f"~{int(ratio) - 1} frame(s) not observed"))
        else:
            # Half-tick / fractional spacing is legal VFR presentation.
            vfr.append(cur.pts)
            anomalies.append(IntervalAnomaly(
                "vfr", cur.pts, prev.pts + nominal_tick, delta,
                float(base.ticks_to_seconds(delta)),
                "variable frame rate spacing"))
    # Explicit side-packet kinds.
    for ev in events:
        if ev.kind == "vfr":
            vfr.append(ev.pts)
            anomalies.append(IntervalAnomaly(
                "vfr", ev.pts, ev.pts, 0,
                float(base.ticks_to_seconds(ev.pts)),
                "variable frame rate side packet"))
        elif ev.kind == "duplicate":
            anomalies.append(IntervalAnomaly(
                "bad_packet", ev.pts, ev.pts, 0, 0.0, "duplicate packet"))
        elif ev.kind == "corrupt" or not ev.payload_ok:
            anomalies.append(IntervalAnomaly(
                "bad_packet", ev.pts, ev.pts, 0,
                float(base.ticks_to_seconds(ev.pts)), "corrupt payload"))
    return anomalies, sorted(set(vfr))


def _is_clean_fraction(delta: int, tick: int) -> bool:
    """True when delta is a simple rational subdivision of the nominal tick."""
    from fractions import Fraction
    ratio = Fraction(delta, tick)
    return ratio.denominator <= 4
