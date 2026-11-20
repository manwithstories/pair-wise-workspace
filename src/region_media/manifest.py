"""Manifest generation and bitrate/alignment metrics.

The manifest is the audit trail the duty engineer reconciles against the
source packet timeline: it records, per chunk, the source sequence range, the
source and output media ranges, the measured loudness/peak, the error codes
and the chunk hash.  ``verify`` recomputes the top-level numbers from the
manifest plus the actual files, so the two can never silently diverge.
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from .checkpoint import ChunkRecord
from .errors import Code, Diagnostic

MANIFEST_VERSION = 1
MANIFEST_NAME = "manifest.json"


def bitrate_deviation_pct(actual_kbps: float | None, target_kbps: float) -> float:
    if not target_kbps or actual_kbps is None or not math.isfinite(actual_kbps):
        return 0.0
    return (actual_kbps - target_kbps) / target_kbps * 100.0


def _finite(value: float | None, fallback: float = 0.0) -> float:
    if value is None or not math.isfinite(value):
        return fallback
    return value


@dataclass
class ChunkManifest:
    """One chunk entry inside the manifest."""

    record: ChunkRecord
    source_rate: int
    target_rate: int
    start_seconds: float
    end_seconds: float
    source_start_seconds: float
    source_end_seconds: float
    media_offset_samples: int = 0

    def to_json(self) -> dict[str, Any]:
        body = self.record.to_json()
        body.update(
            {
                "start_seconds": round(self.start_seconds, 9),
                "end_seconds": round(self.end_seconds, 9),
                "duration_seconds": round(self.end_seconds - self.start_seconds, 9),
                "source_start_seconds": round(self.source_start_seconds, 9),
                "source_end_seconds": round(self.source_end_seconds, 9),
                "media_offset_samples": self.media_offset_samples,
            }
        )
        return body


@dataclass
class Manifest:
    version: int = MANIFEST_VERSION
    input_dir: str = ""
    fingerprint: str = ""
    parameters: dict[str, Any] = field(default_factory=dict)
    source: dict[str, Any] = field(default_factory=dict)
    video: dict[str, Any] = field(default_factory=dict)
    chunks: list[ChunkManifest] = field(default_factory=list)
    errors: dict[str, int] = field(default_factory=dict)
    diagnostics: list[dict[str, Any]] = field(default_factory=list)
    intervals: list[dict[str, Any]] = field(default_factory=list)
    stopped_early: bool = False
    resumed: bool = False
    reused_chunks: int = 0
    quarantined_chunks: list[dict[str, Any]] = field(default_factory=list)

    # --- aggregate metrics ------------------------------------------------
    @property
    def total_samples(self) -> int:
        return sum(c.record.samples for c in self.chunks)

    @property
    def actual_kbps(self) -> float:
        total_bytes = sum(c.record.size_bytes for c in self.chunks)
        seconds = self.total_samples / self.target_rate if self.target_rate else 0.0
        if seconds <= 0:
            return 0.0
        return total_bytes * 8.0 / seconds / 1000.0

    @property
    def target_rate(self) -> int:
        return int(self.parameters.get("rate", 0))

    @property
    def target_kbps(self) -> float:
        return float(self.parameters.get("target_kbps", 0.0))

    @property
    def integrated_lufs(self) -> float | None:
        powers = [c.record.loudness for c in self.chunks if c.record.loudness is not None]
        if not powers:
            return None
        # The per-chunk values are integrated LUFS; combine by mean power.
        mean_power = sum(10.0 ** (v / 10.0) for v in powers) / len(powers)
        return 10.0 * math.log10(mean_power)

    @property
    def true_peak_dbfs(self) -> float | None:
        peaks = [c.record.peak_dbfs for c in self.chunks if c.record.peak_dbfs is not None]
        if not peaks:
            return None
        return max(peaks)

    @property
    def clipped_samples(self) -> int:
        return sum(c.record.clipped_samples for c in self.chunks)

    @property
    def silence_chunks(self) -> int:
        return sum(1 for c in self.chunks if c.record.silent)

    @property
    def short_chunks(self) -> int:
        return sum(1 for c in self.chunks if c.record.short)

    def to_json(self) -> dict[str, Any]:
        target = self.target_kbps
        actual = self.actual_kbps
        return {
            "version": self.version,
            "input_dir": self.input_dir,
            "fingerprint": self.fingerprint,
            "parameters": dict(self.parameters),
            "source": dict(self.source),
            "video": dict(self.video),
            "chunks": [c.to_json() for c in self.chunks],
            "errors": dict(self.errors),
            "diagnostics": list(self.diagnostics),
            "intervals": list(self.intervals),
            "stopped_early": self.stopped_early,
            "resumed": self.resumed,
            "reused_chunks": self.reused_chunks,
            "quarantined_chunks": list(self.quarantined_chunks),
            "metrics": {
                "chunks": len(self.chunks),
                "samples": self.total_samples,
                "target_kbps": target,
                "actual_kbps": round(actual, 4),
                "bitrate_deviation_pct": round(
                    bitrate_deviation_pct(actual, target), 4
                ),
                "integrated_lufs": _round_or_none(self.integrated_lufs),
                "true_peak_dbfs": _round_or_none(self.true_peak_dbfs),
                "clipped_samples": self.clipped_samples,
                "silent_chunks": self.silence_chunks,
                "short_chunks": self.short_chunks,
            },
        }

    def write(self, path: Path) -> None:
        from .checkpoint import atomic_write_json

        atomic_write_json(path, self.to_json())


def _round_or_none(value: float | None) -> float | None:
    if value is None or not math.isfinite(value):
        return None
    return round(value, 4)


def load_manifest(path: Path) -> dict[str, Any]:
    with path.open("r", encoding="utf-8") as handle:
        data = json.load(handle)
    if not isinstance(data, dict) or data.get("version") != MANIFEST_VERSION:
        raise ValueError(f"{path} is not a version-{MANIFEST_VERSION} manifest")
    return data


def build_intervals(
    chunks: Iterable[ChunkManifest], *, tolerance_ms: float = 0.0
) -> list[dict[str, Any]]:
    """Continuity check: each chunk must start where the previous one ended."""
    intervals = []
    previous_end: float | None = None
    for chunk in sorted(chunks, key=lambda c: c.record.index):
        start = chunk.start_seconds
        end = chunk.end_seconds
        gap_ms = 0.0
        if previous_end is not None:
            gap_ms = (start - previous_end) * 1000.0
        intervals.append(
            {
                "index": chunk.record.index,
                "start_seconds": round(start, 9),
                "end_seconds": round(end, 9),
                "gap_ms": round(gap_ms, 6),
                "within_tolerance": abs(gap_ms) <= tolerance_ms,
            }
        )
        previous_end = end
    return intervals


def alignment_error_ms(
    manifest_chunks: list[dict[str, Any]],
    decoded_ranges: list[tuple[int, int]],
    rate: int,
    *,
    source_ranges: list[tuple[int, int]] | None = None,
) -> float:
    """Worst absolute difference between manifest and measured chunk starts.

    Both are expressed in output samples; the returned value is milliseconds.
    """
    worst = 0.0
    for entry, (decoded_start, _decoded_end) in zip(manifest_chunks, decoded_ranges):
        expected = int(entry["start_sample"])
        diff_ms = abs(decoded_start - expected) / rate * 1000.0
        worst = max(worst, diff_ms)
    return worst


def manifest_alignment_ms(manifest: dict[str, Any]) -> float:
    """Largest start-time error implied by the manifest's own intervals."""
    worst = 0.0
    for interval in manifest.get("intervals", []):
        worst = max(worst, abs(float(interval.get("gap_ms", 0.0))))
    return worst


def quality_summary(manifest: dict[str, Any]) -> dict[str, Any]:
    metrics = manifest.get("metrics", {})
    errors = manifest.get("errors", {})
    return {
        "chunks": int(metrics.get("chunks", 0)),
        "samples": int(metrics.get("samples", 0)),
        "target_kbps": float(metrics.get("target_kbps", 0.0)),
        "actual_kbps": float(metrics.get("actual_kbps", 0.0)),
        "bitrate_deviation_pct": float(metrics.get("bitrate_deviation_pct", 0.0)),
        "integrated_lufs": metrics.get("integrated_lufs"),
        "true_peak_dbfs": metrics.get("true_peak_dbfs"),
        "clipped_samples": int(metrics.get("clipped_samples", 0)),
        "lost_packets": int(errors.get(Code.LOST_PACKET.value, 0)),
        "reordered_packets": int(errors.get(Code.REORDERED_PACKET.value, 0)),
        "duplicate_packets": int(errors.get(Code.DUPLICATE_PACKET.value, 0)),
        "truncated_packets": int(errors.get(Code.TRUNCATED_PACKET.value, 0)),
        "nonfinite_samples": int(errors.get(Code.NONFINITE_SAMPLE.value, 0)),
        "metadata_conflicts": int(errors.get(Code.METADATA_CONFLICT.value, 0)),
        "metadata_missing": int(errors.get(Code.METADATA_MISSING.value, 0)),
        "silent_chunks": int(metrics.get("silent_chunks", 0)),
        "short_chunks": int(metrics.get("short_chunks", 0)),
        "video_time_shift": int(errors.get(Code.VIDEO_TIME_SHIFT.value, 0)),
    }


__all__ = [
    "ChunkManifest",
    "MANIFEST_NAME",
    "MANIFEST_VERSION",
    "Manifest",
    "alignment_error_ms",
    "bitrate_deviation_pct",
    "build_intervals",
    "load_manifest",
    "manifest_alignment_ms",
    "quality_summary",
]
