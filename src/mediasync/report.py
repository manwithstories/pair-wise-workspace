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


def build_manifest(
    *,
    seed: int,
    target_bps: int,
    total_bytes: int,
    duration_ticks: int,
    tracks: Sequence[TrackManifest],
    gaps: Iterable[Mapping[str, Any]] = (),
    transport: Mapping[str, Any] | None = None,
    audio: Mapping[str, Any] | None = None,
    sync: Mapping[str, Any] | None = None,
    notes: Iterable[str] = (),
) -> Manifest:
    """Compute actual bitrate from delivered bytes over the timeline duration."""

    if duration_ticks <= 0:
        raise InputError("timeline duration must be positive")
    if total_bytes < 0:
        raise InputError("delivered byte count cannot be negative")
    seconds = duration_ticks / TICKS_PER_SECOND
    actual_bps = int(round(total_bytes * 8 / seconds))
    return Manifest(
        seed=seed,
        target_bps=target_bps,
        actual_bps=actual_bps,
        duration_ticks=duration_ticks,
        tracks=list(tracks),
        gaps=[dict(gap) for gap in gaps],
        transport=dict(transport or {}),
        audio=dict(audio or {}),
        sync=dict(sync or {}),
        notes=list(notes),
    )


def load_manifest(path: str | Path) -> dict[str, Any]:
    path = Path(path)
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except OSError as exc:
        raise InputError(f"cannot read manifest {path}: {exc}") from exc
    except json.JSONDecodeError as exc:
        raise InputError(f"manifest {path} is not valid JSON: {exc}") from exc
    if not isinstance(payload, dict):
        raise InputError("manifest root must be an object")
    return payload


def verify_manifest(payload: Mapping[str, Any]) -> list[str]:
    """Re-check a manifest from scratch; returns gate failures."""

    if payload.get("schema") != SCHEMA:
        raise InputError(
            f"unsupported manifest schema {payload.get('schema')!r}; "
            f"expected {SCHEMA!r}"
        )
    failures: list[str] = []
    bitrate = payload.get("bitrate")
    if not isinstance(bitrate, Mapping):
        raise InputError("manifest is missing the bitrate section")
    try:
        target = int(bitrate["target_bps"])
        actual = int(bitrate["actual_bps"])
    except (KeyError, TypeError, ValueError) as exc:
        raise InputError(f"manifest bitrate section is malformed: {exc}") from exc
    if target <= 0:
        raise InputError("manifest target bitrate must be positive")
    deviation = (actual - target) / target
    if abs(deviation) > BITRATE_TOLERANCE:
        failures.append(
            f"bitrate deviation {deviation:.3%} exceeds {BITRATE_TOLERANCE:.0%}"
        )
    video = payload.get("video")
    if not isinstance(video, Mapping):
        raise InputError("manifest is missing the video section")
    frames = video.get("frames")
    if not isinstance(frames, int) or frames <= 0:
        failures.append("manifest reports no video frames")
    audio = payload.get("audio")
    if not isinstance(audio, Mapping):
        raise InputError("manifest is missing the audio section")
    rms = audio.get("rms")
    if not isinstance(rms, (int, float)) or not math.isfinite(float(rms)) or rms <= 0:
        failures.append("manifest audio RMS must be positive and finite")
    transport = payload.get("transport")
    if not isinstance(transport, Mapping):
        raise InputError("manifest is missing the transport section")
    for key in ("loss", "recovered", "unrecoverable"):
        value = transport.get(key)
        if not isinstance(value, int) or value < 0:
            raise InputError(f"manifest transport.{key} must be a non-negative int")
    if transport["unrecoverable"] > 0:
        failures.append(
            f"{transport['unrecoverable']} unrecoverable packets in manifest"
        )
    tracks = payload.get("tracks")
    if not isinstance(tracks, list) or not tracks:
        raise InputError("manifest tracks must be a non-empty list")
    for entry in tracks:
        if not isinstance(entry, Mapping):
            raise InputError("manifest track entries must be objects")
        status = entry.get("status")
        error = entry.get("max_error_ms")
        if status == "ok":
            if not isinstance(error, (int, float)) or not math.isfinite(float(error)):
                raise InputError(
                    f"track {entry.get('track_id')!r} has no usable max_error_ms"
                )
            if float(error) > MAX_ERROR_MS:
                failures.append(
                    f"{entry.get('track_id')}: alignment error {error} ms "
                    f"exceeds {MAX_ERROR_MS:g} ms"
                )
            if entry.get("complete") is not True:
                failures.append(
                    f"{entry.get('track_id')}: status ok but incomplete"
                )
        if entry.get("status") not in ("ok", "incomplete", "unrecoverable"):
            raise InputError(
                f"track {entry.get('track_id')!r} has unknown status "
                f"{status!r}"
            )
    declared = payload.get("status")
    if declared == "PASS" and failures:
        failures.append("manifest claims PASS but fails re-verification")
    return failures


def manifest_failures_to_error(failures: Sequence[str]) -> QualityGateError:
    return QualityGateError("; ".join(failures))


def format_summary(manifest: Manifest) -> list[str]:
    """Human-readable terminal lines shared by demo and verify."""

    payload = manifest.to_dict()
    transport = payload["transport"]
    lines = [
        f"target_bps={manifest.target_bps} actual_bps={manifest.actual_bps} "
        f"deviation_pct={payload['bitrate']['deviation_pct']:+.3f}",
        f"video_frames={manifest.video_frames}",
        f"audio_peak={manifest.audio.get('peak', 0.0):.6f} "
        f"audio_rms={manifest.audio.get('rms', 0.0):.6f}",
        f"loss={transport.get('loss', 0)} recovered={transport.get('recovered', 0)} "
        f"unrecoverable={transport.get('unrecoverable', 0)}",
        "loss/recovered/unrecoverable="
        f"{transport.get('loss', 0)}/{transport.get('recovered', 0)}/"
        f"{transport.get('unrecoverable', 0)}",
        f"manifest_max_error_ms={manifest.manifest_max_error_ms:.3f}",
        f"status={manifest.status()}",
    ]
    return lines


def verify_summary(payload: Mapping[str, Any], failures: Sequence[str]) -> list[str]:
    bitrate = payload["bitrate"]
    transport = payload["transport"]
    sync = payload.get("sync", {})
    max_error = sync.get("max_error_ms", 0.0)
    declared = payload.get("status", "UNKNOWN")
    lines = [
        f"target_bps={bitrate['target_bps']} actual_bps={bitrate['actual_bps']} "
        f"deviation_pct={bitrate.get('deviation_pct', 0.0):+.3f}",
        f"video_frames={payload['video']['frames']}",
        f"audio_rms={payload['audio'].get('rms', 0.0):.6f}",
        f"loss={transport['loss']} recovered={transport['recovered']} "
        f"unrecoverable={transport['unrecoverable']}",
        "loss/recovered/unrecoverable="
        f"{transport['loss']}/{transport['recovered']}/{transport['unrecoverable']}",
        f"manifest_max_error_ms={float(max_error):.3f}",
        f"status={'PASS' if not failures and declared == 'PASS' else 'FAIL'}",
    ]
    return lines
