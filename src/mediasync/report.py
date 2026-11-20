"""Manifest assembly, quality statistics and verification gates.

The report is the contract between the pipeline, the CLI and the tests: it
records bitrate target versus measured, per-track media extents, transport
accounting, audio levels, alignment error and every unrecoverable gap.  The
verify routine re-checks a saved manifest without trusting its ``status``.
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence

from .clock import TICKS_PER_SECOND
from .errors import InputError, QualityGateError

SCHEMA = "mediasync/manifest/v1"
BITRATE_TOLERANCE = 0.05
MAX_ERROR_MS = 20.0


@dataclass(frozen=True)
class TrackManifest:
    track_id: str
    kind: str
    role: str
    codec: str
    start_ticks: int
    end_ticks: int
    complete: bool
    aligned: bool
    max_error_ms: float
    drift_ppm: float = 0.0
    packets: int = 0
    recovered: int = 0
    lost: int = 0
    status: str = "ok"
    reason: str = ""

    @property
    def duration_ticks(self) -> int:
        return max(0, self.end_ticks - self.start_ticks)

    def to_dict(self) -> dict[str, Any]:
        return {
            "track_id": self.track_id,
            "kind": self.kind,
            "role": self.role,
            "codec": self.codec,
            "start_ticks": self.start_ticks,
            "end_ticks": self.end_ticks,
            "duration_ticks": self.duration_ticks,
            "start_seconds": round(self.start_ticks / TICKS_PER_SECOND, 6),
            "end_seconds": round(self.end_ticks / TICKS_PER_SECOND, 6),
            "complete": self.complete,
            "aligned": self.aligned,
            "max_error_ms": round(self.max_error_ms, 6),
            "drift_ppm": round(self.drift_ppm, 6),
            "packets": self.packets,
            "recovered": self.recovered,
            "lost": self.lost,
            "status": self.status,
            "reason": self.reason,
        }


@dataclass
class Manifest:
    seed: int
    target_bps: int
    actual_bps: int
    duration_ticks: int
    tracks: list[TrackManifest] = field(default_factory=list)
    gaps: list[dict[str, Any]] = field(default_factory=list)
    transport: dict[str, Any] = field(default_factory=dict)
    audio: dict[str, Any] = field(default_factory=dict)
    sync: dict[str, Any] = field(default_factory=dict)
    notes: list[str] = field(default_factory=list)

    @property
    def deviation(self) -> float:
        if self.target_bps <= 0:
            raise InputError("target bitrate must be positive")
        return (self.actual_bps - self.target_bps) / self.target_bps

    @property
    def video_frames(self) -> int:
        return int(
            sum(
                item.packets
                for item in self.tracks
                if item.kind in ("video", "av")
            )
        )

    @property
    def unrecoverable(self) -> int:
        return int(self.transport.get("unrecoverable", 0))

    @property
    def successful_tracks(self) -> list[TrackManifest]:
        return [item for item in self.tracks if item.status == "ok"]

    @property
    def manifest_max_error_ms(self) -> float:
        aligned = [item.max_error_ms for item in self.tracks if item.aligned]
        return max(aligned, default=0.0)

    def status(self) -> str:
        return "PASS" if self._gate_failures() == [] else "FAIL"

    def _gate_failures(self) -> list[str]:
        failures: list[str] = []
        if self.target_bps <= 0:
            failures.append("target_bps must be positive")
        elif abs(self.deviation) > BITRATE_TOLERANCE:
            failures.append(
                f"bitrate deviation {self.deviation:.3%} exceeds "
                f"{BITRATE_TOLERANCE:.0%}"
            )
        if self.video_frames <= 0:
            failures.append("no video frames were delivered")
        rms = float(self.audio.get("rms", 0.0) or 0.0)
        if not math.isfinite(rms) or rms <= 0.0:
            failures.append("audio RMS must be positive and finite")
        if self.unrecoverable > 0:
            failures.append(f"{self.unrecoverable} unrecoverable packets")
        for item in self.successful_tracks:
            if item.max_error_ms > MAX_ERROR_MS:
                failures.append(
                    f"{item.track_id}: alignment error {item.max_error_ms:.3f} ms "
                    f"exceeds {MAX_ERROR_MS:g} ms"
                )
            if not item.complete:
                failures.append(f"{item.track_id}: marked ok but incomplete")
        return failures

    def to_dict(self) -> dict[str, Any]:
        return {
            "schema": SCHEMA,
            "seed": self.seed,
            "status": self.status(),
            "timeline": {
                "start_ticks": 0,
                "end_ticks": self.duration_ticks,
                "duration_seconds": round(self.duration_ticks / TICKS_PER_SECOND, 6),
            },
            "bitrate": {
                "target_bps": self.target_bps,
                "actual_bps": self.actual_bps,
                "deviation": round(self.deviation, 6),
                "deviation_pct": round(self.deviation * 100.0, 4),
                "tolerance": BITRATE_TOLERANCE,
            },
            "video": {
                "frames": self.video_frames,
                "tracks": sum(
                    1 for item in self.tracks if item.kind in ("video", "av")
                ),
            },
            "transport": dict(self.transport),
            "audio": dict(self.audio),
            "sync": dict(self.sync),
            "tracks": [item.to_dict() for item in self.tracks],
            "gaps": list(self.gaps),
            "notes": list(self.notes),
        }

    def write(self, path: str | Path) -> Path:
        path = Path(path)
        path.parent.mkdir(parents=True, exist_ok=True)
        payload = self.to_dict()
        path.write_text(
            json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
            encoding="utf-8",
        )
        return path
