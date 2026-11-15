"""Shared result models used by parsers, rules, storage and the CLI."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

SEVERITIES = ("error", "warning", "info")
SEVERITY_RANK = {name: rank for rank, name in enumerate(SEVERITIES)}


@dataclass(slots=True)
class Finding:
    rule_id: str
    severity: str
    message: str
    evidence: str
    track_index: int | None = None
    offset: int | None = None

    def __post_init__(self) -> None:
        if self.severity not in SEVERITY_RANK:
            raise ValueError(f"invalid severity: {self.severity}")

    def to_dict(self) -> dict[str, Any]:
        return {
            "rule_id": self.rule_id,
            "severity": self.severity,
            "message": self.message,
            "evidence": self.evidence,
            "track_index": self.track_index,
            "offset": self.offset,
        }


@dataclass(slots=True)
class Sample:
    index: int
    dts: int
    duration: int
    offset: int
    size: int
    cts: int
    sync: bool
    chunk: int


@dataclass(slots=True)
class Track:
    index: int
    track_id: int
    kind: str
    codec: str = "unknown"
    timescale: int = 0
    duration: int = 0
    duration_ms: float = 0.0
    presentation_duration_ms: float = 0.0
    presentation_start_ms: float = 0.0
    sample_count: int = 0
    width: int | None = None
    height: int | None = None
    sample_rate: int | None = None
    channels: int | None = None
    sample_entry: str = ""
    codec_details: dict[str, Any] = field(default_factory=dict)
    edits: list[dict[str, int]] = field(default_factory=list)
    samples: list[Sample] = field(default_factory=list)
    samples_truncated: bool = False
    chunk_ranges: list[tuple[int, int, int]] = field(default_factory=list)
    sync_samples: list[int] = field(default_factory=list)
    table_facts: dict[str, Any] = field(default_factory=dict)
    parse_errors: list[str] = field(default_factory=list)

    def public_dict(self) -> dict[str, Any]:
        return {
            "track_index": self.index,
            "track_id": self.track_id,
            "kind": self.kind,
            "codec": self.codec,
            "timescale": self.timescale,
            "duration_ms": round(self.duration_ms, 3),
            "presentation_duration_ms": round(self.presentation_duration_ms, 3),
            "presentation_start_ms": round(self.presentation_start_ms, 3),
            "sample_count": self.sample_count,
            "width": self.width,
            "height": self.height,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "sample_entry": self.sample_entry,
            "codec_details": self.codec_details,
            "samples_truncated": self.samples_truncated,
        }


@dataclass(slots=True)
class ProbeResult:
    path: str
    container: str
    file_size: int
    tracks: list[Track] = field(default_factory=list)
    findings: list[Finding] = field(default_factory=list)
    mdat_ranges: list[tuple[int, int]] = field(default_factory=list)
    movie_timescale: int = 0
    status: str = "ok"
    error: str | None = None
    metadata: dict[str, Any] = field(default_factory=dict)

    def add_finding(self, finding: Finding) -> None:
        self.findings.append(finding)

    @property
    def has_error(self) -> bool:
        return any(item.severity == "error" for item in self.findings)

    def track_duration(self, kind: str) -> float | None:
        for track in self.tracks:
            if track.kind == kind:
                return track.presentation_duration_ms
        return None

    @property
    def drift_ms(self) -> float | None:
        video = self.track_duration("video")
        audio = self.track_duration("audio")
        if video is None or audio is None:
            return None
        return round(audio - video, 3)

    def public_dict(self) -> dict[str, Any]:
        return {
            "path": self.path,
            "container": self.container,
            "file_size": self.file_size,
            "status": self.status,
            "error": self.error,
            "tracks": [track.public_dict() for track in self.tracks],
            "tracks_total": len(self.tracks),
            "video_duration_ms": _rounded(self.track_duration("video")),
            "audio_duration_ms": _rounded(self.track_duration("audio")),
            "drift_ms": self.drift_ms,
            "findings": [finding.to_dict() for finding in self.findings],
            "findings_total": len(self.findings),
            "metadata": self.metadata,
        }


def _rounded(value: float | None) -> float | None:
    return None if value is None else round(value, 3)


def finding_counts(findings: list[Finding]) -> dict[str, int]:
    counts = {severity: 0 for severity in SEVERITIES}
    for finding in findings:
        counts[finding.severity] += 1
    return counts
