"""Structured results shared by the detector, store, exporter and CLI.

All sample positions are integer sample indices on the *sample grid of the
file they came from*.  ``end_sample`` is exclusive, matching Python slice
semantics, so ``sample_count == end_sample - start_sample`` always holds.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any, Optional

if TYPE_CHECKING:  # pragma: no cover - typing only
    from .wavio import WavInfo


@dataclass(frozen=True)
class Segment:
    """One contiguous region classified as meaningful audio."""

    index: int
    start_sample: int
    end_sample: int
    peak: float
    rms: float
    sample_rate: int

    @property
    def sample_count(self) -> int:
        return self.end_sample - self.start_sample

    @property
    def start_seconds(self) -> float:
        return self.start_sample / self.sample_rate

    @property
    def end_seconds(self) -> float:
        return self.end_sample / self.sample_rate

    @property
    def duration_seconds(self) -> float:
        return self.sample_count / self.sample_rate

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "sample_count": self.sample_count,
            "start_seconds": self.start_seconds,
            "end_seconds": self.end_seconds,
            "duration_seconds": self.duration_seconds,
            "peak": self.peak,
            "rms": self.rms,
        }


@dataclass(frozen=True)
class SilenceRegion:
    """One contiguous region of silence; the exact complement of segments."""

    index: int
    start_sample: int
    end_sample: int
    sample_rate: int

    @property
    def sample_count(self) -> int:
        return self.end_sample - self.start_sample

    @property
    def start_seconds(self) -> float:
        return self.start_sample / self.sample_rate

    @property
    def end_seconds(self) -> float:
        return self.end_sample / self.sample_rate

    @property
    def duration_seconds(self) -> float:
        return self.sample_count / self.sample_rate

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "start_sample": self.start_sample,
            "end_sample": self.end_sample,
            "sample_count": self.sample_count,
            "start_seconds": self.start_seconds,
            "end_seconds": self.end_seconds,
            "duration_seconds": self.duration_seconds,
        }


@dataclass
class DetectionConfig:
    """All tunables for the detector.

    Durations are milliseconds, levels are dBFS.  ``threshold_db`` is the
    absolute floor; ``relative_margin_db`` raises it above the estimated
    noise floor so quiet rooms are not reported as content.
    """

    frame_ms: float = 20.0
    hop_ms: float = 10.0
    min_segment_ms: float = 80.0
    min_silence_ms: float = 120.0
    padding_ms: float = 40.0
    threshold_db: float = -42.0
    relative_margin_db: float = 6.0
    hysteresis_db: float = 6.0
    noise_percentile: float = 15.0
    smooth_ms: float = 30.0
    ramp_window_ms: float = 250.0
    ramp_max_db: float = 20.0
    ramp_margin_db: float = 3.0
    max_pulse_ms: float = 50.0

    def validate(self, sample_rate: int) -> None:
        if sample_rate <= 0:
            raise ValueError("sample_rate must be positive")
        if not self.frame_ms > 0 or not self.hop_ms > 0:
            raise ValueError("frame_ms and hop_ms must be positive")
        if self.hop_ms > self.frame_ms:
            raise ValueError("hop_ms must not exceed frame_ms")
        for name in ("min_segment_ms", "min_silence_ms", "padding_ms"):
            if getattr(self, name) < 0:
                raise ValueError(f"{name} must not be negative")
        if not 0 <= self.noise_percentile <= 100:
            raise ValueError("noise_percentile must be within [0, 100]")
        if self.hysteresis_db < 0:
            raise ValueError("hysteresis_db must not be negative")
        if self.max_pulse_ms > self.min_segment_ms:
            raise ValueError("max_pulse_ms must not exceed min_segment_ms")
        if sample_rate * self.hop_ms / 1000.0 < 1:
            raise ValueError("hop_ms too small for this sample rate")

    def to_dict(self) -> dict[str, Any]:
        return {
            "frame_ms": self.frame_ms,
            "hop_ms": self.hop_ms,
            "min_segment_ms": self.min_segment_ms,
            "min_silence_ms": self.min_silence_ms,
            "padding_ms": self.padding_ms,
            "threshold_db": self.threshold_db,
            "relative_margin_db": self.relative_margin_db,
            "hysteresis_db": self.hysteresis_db,
            "noise_percentile": self.noise_percentile,
            "smooth_ms": self.smooth_ms,
            "ramp_window_ms": self.ramp_window_ms,
            "ramp_max_db": self.ramp_max_db,
            "ramp_margin_db": self.ramp_margin_db,
            "max_pulse_ms": self.max_pulse_ms,
        }


@dataclass
class FileAnalysis:
    """Detector output for one file plus persistence/display metadata."""

    path: str
    sha256: str
    status: str  # "ok" | "error"
    info: Optional["WavInfo"] = None
    result: Optional["DetectionResult"] = None
    error_code: Optional[str] = None
    error_message: Optional[str] = None

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "path": self.path,
            "sha256": self.sha256,
            "status": self.status,
        }
        if self.info is not None:
            payload["sample_rate"] = self.info.sample_rate
            payload["channels"] = self.info.channels
            payload["sample_width"] = self.info.sample_width
            payload["total_samples"] = self.info.nframes
            payload["duration_seconds"] = self.info.duration_seconds
        if self.error_code:
            payload["error_code"] = self.error_code
            payload["error_message"] = self.error_message
        if self.result is not None:
            payload["noise_floor_db"] = self.result.noise_floor_db
            payload["enter_threshold_db"] = self.result.enter_threshold_db
            payload["exit_threshold_db"] = self.result.exit_threshold_db
            payload["segments"] = [s.to_dict() for s in self.result.segments]
            payload["silences"] = [s.to_dict() for s in self.result.silences]
            payload["total_silence_seconds"] = self.result.total_silence_seconds
        return payload


@dataclass
class DetectionResult:
    path: str
    sample_rate: int
    total_samples: int
    noise_floor_db: float
    enter_threshold_db: float
    exit_threshold_db: float
    segments: list[Segment] = field(default_factory=list)
    silences: list[SilenceRegion] = field(default_factory=list)

    @property
    def total_segment_samples(self) -> int:
        return sum(s.sample_count for s in self.segments)

    @property
    def total_silence_samples(self) -> int:
        return sum(s.sample_count for s in self.silences)

    @property
    def total_silence_seconds(self) -> float:
        return self.total_silence_samples / self.sample_rate

    @property
    def duration_seconds(self) -> float:
        return self.total_samples / self.sample_rate

    def to_dict(self) -> dict[str, Any]:
        return {
            "path": self.path,
            "sample_rate": self.sample_rate,
            "total_samples": self.total_samples,
            "duration_seconds": self.duration_seconds,
            "noise_floor_db": self.noise_floor_db,
            "enter_threshold_db": self.enter_threshold_db,
            "exit_threshold_db": self.exit_threshold_db,
            "segments": [s.to_dict() for s in self.segments],
            "silences": [s.to_dict() for s in self.silences],
            "total_silence_seconds": self.total_silence_seconds,
        }
