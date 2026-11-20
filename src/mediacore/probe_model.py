"""Scan result data model.

Plain dataclasses only: no I/O, no PyAV.  Keeping the result model separate
means the pipeline, report and tests can construct scans by hand.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .errors import MediaError
from .timebase import DriftMeter, Timebase, Us

@dataclass(frozen=True)
class PacketInfo:
    ordinal: int
    pts_ticks: int | None
    dts_ticks: int | None
    pts_us: Us
    dts_us: Us
    size: int
    keyframe: bool
    pos: int | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "ordinal": self.ordinal,
            "pts_ticks": self.pts_ticks,
            "dts_ticks": self.dts_ticks,
            "pts_us": self.pts_us,
            "dts_us": self.dts_us,
            "size": self.size,
            "keyframe": self.keyframe,
            "pos": self.pos,
        }


@dataclass
class TrackScan:
    """Everything the pipeline knows about one scanned track."""

    track_id: str
    path: str
    stream_index: int
    kind: str
    timebase: Timebase
    start_us: Us
    duration_us: Us
    declared_frames: int
    packets: list[PacketInfo] = field(default_factory=list)
    # Per-sample duration table reported in the container ("stts").  This is
    # the authoritative expected packet count: it survives stco damage, so a
    # sample-table hole stays visible as declared > demuxed.
    declared_samples: int = 0
    sample_rate: int | None = None
    layout: str | None = None
    codec: str | None = None
    drift: DriftMeter = field(default_factory=DriftMeter)
    damage: list[MediaError] = field(default_factory=list)

    @property
    def packet_count(self) -> int:
        return len(self.packets)

    @property
    def packets_with_pos(self) -> list[PacketInfo]:
        return [p for p in self.packets if p.pos is not None]

    @property
    def expected_packets(self) -> int:
        """Best available expected packet count (stts beats the header)."""
        return self.declared_samples or self.declared_frames

    @property
    def first_pts_us(self) -> Us:
        return self.packets[0].pts_us if self.packets else 0

    @property
    def last_pts_us(self) -> Us:
        return self.packets[-1].pts_us if self.packets else 0

    @property
    def end_us(self) -> Us:
        """End of the last packet, derived from its successor interval."""
        if len(self.packets) >= 2:
            step = self.packets[-1].pts_us - self.packets[-2].pts_us
        else:
            step = self.interval_us()
        return self.last_pts_us + max(step, 0)

    def interval_us(self) -> int:
        """Median inter-packet interval in microseconds (robust to a gap)."""
        deltas = [
            b.pts_us - a.pts_us
            for a, b in zip(self.packets, self.packets[1:])
            if b.pts_us > a.pts_us
        ]
        if not deltas:
            return self.duration_us or 1
        deltas.sort()
        return deltas[len(deltas) // 2]

    def duration_tolerance_us(self) -> int:
        return max(self.interval_us(), 1)

    def damaged(self) -> bool:
        return bool(self.damage)

    def worst(self) -> MediaError | None:
        if not self.damage:
            return None
        order = {"index_gap": 0, "truncated": 1, "corrupt": 2, "index_missing": 3}
        return min(self.damage, key=lambda e: (order.get(e.code.split(".")[-1], 9), e.context.get("packet_ordinal", 0)))

    def to_dict(self) -> dict[str, Any]:
        return {
            "track_id": self.track_id,
            "path": self.path,
            "stream_index": self.stream_index,
            "kind": self.kind,
            "timebase": str(self.timebase),
            "start_us": self.start_us,
            "duration_us": self.duration_us,
            "declared_frames": self.declared_frames,
            "packet_count": self.packet_count,
            "sample_rate": self.sample_rate,
            "layout": self.layout,
            "codec": self.codec,
            "drift": self.drift.to_dict(),
            "damage": [d.to_dict() for d in self.damage],
        }


@dataclass
class DamageReport:
    """Aggregate of every anomaly across a scan set."""

    scans: dict[str, TrackScan] = field(default_factory=dict)

    def add(self, scan: TrackScan) -> None:
        self.scans[scan.track_id] = scan

    def damaged_scans(self) -> list[TrackScan]:
        return [s for s in self.scans.values() if s.damaged()]

    def any_damage(self) -> bool:
        return any(s.damaged() for s in self.scans.values())

    def first_fault(self) -> tuple[TrackScan, MediaError] | None:
        """The earliest-located anomaly across all tracks.

        Ordering is by timeline position first (so the operator sees the
        damage nearest the head of the material), then by track id for
        determinism.
        """
        best: tuple[int, str, TrackScan, MediaError] | None = None
        for scan in self.scans.values():
            for err in scan.damage:
                key = (int(err.context.get("pts_us", 0)), scan.track_id, scan, err)
                cand = (key[0], key[1], key[2], key[3])
                if best is None or (cand[0], cand[1]) < (best[0], best[1]):
                    best = cand
        if best is None:
            return None
        return best[2], best[3]

    def damage_kinds(self) -> list[str]:
        kinds = sorted({d.code.split(".")[-1] for s in self.scans.values() for d in s.damage})
        return kinds

    def to_dict(self) -> dict[str, Any]:
        first = self.first_fault()
        return {
            "damaged": self.any_damage(),
            "kinds": self.damage_kinds(),
            "first_fault": first[1].to_dict() if first else None,
            "first_fault_track": first[0].track_id if first else None,
            "tracks": {tid: s.to_dict() for tid, s in sorted(self.scans.items())},
        }

