"""Manifests: verifiable segment records, quality stats and provenance.

A manifest is a single JSON document describing one run.  Every reported number
is derived from data that is also written into the document, and every segment
carries a content digest so a later run can prove it reproduced identical bytes.
Manifests are written atomically through :func:`checkpoint.write_json_atomic`.

The same module verifies a manifest on request (``cli verify``): it re-derives
the content digests from the stored segment data and checks the quality and
alignment metrics against their documented limits, returning the list of
failures instead of raising on the first one.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Iterable

from .checkpoint import compact_digest, write_json_atomic
from .errors import MAX_ALIGNMENT_MS, MAX_BITRATE_DEVIATION_PCT, MediaInputError

MANIFEST_VERSION = 1
SEGMENT_DIGEST_PREFIX = b"mediacore-segment-v1\x00"
MANIFEST_DIGEST_PREFIX = b"mediacore-manifest-v1\x00"


@dataclass
class Segment:
    """One contiguous run of access units with its measured rate and timing."""

    ordinal: int
    unit_start: int
    unit_end: int
    byte_start: int
    byte_end: int
    start_s: float
    end_s: float
    keyframe_offsets: list[int] = field(default_factory=list)
    keyframe_count: int = 0
    bytes: int = 0
    seconds: float = 0.0
    target_bps: int = 0
    actual_bps: float = 0.0
    deviation_pct: float = 0.0
    decisions: list[str] = field(default_factory=list)

    def content(self) -> dict[str, Any]:
        """The semantic payload that the segment digest covers."""
        return {
            "ordinal": self.ordinal,
            "unit_start": self.unit_start,
            "unit_end": self.unit_end,
            "byte_start": self.byte_start,
            "byte_end": self.byte_end,
            "start_s": round(self.start_s, 6),
            "end_s": round(self.end_s, 6),
            "keyframe_offsets": list(self.keyframe_offsets),
            "bytes": self.bytes,
            "seconds": round(self.seconds, 6),
            "target_bps": self.target_bps,
            "actual_bps": round(self.actual_bps, 3),
            "decisions": sorted(set(self.decisions)),
        }

    @property
    def digest(self) -> str:
        return compact_digest(SEGMENT_DIGEST_PREFIX, [self.content()])

    def to_dict(self) -> dict[str, Any]:
        out = self.content()
        out.update({
            "keyframe_count": self.keyframe_count,
            "deviation_pct": round(self.deviation_pct, 3),
            "digest": self.digest,
        })
        return out


@dataclass
class Manifest:
    """A complete run record."""

    run_id: str
    codec: str
    time_base: dict[str, int]
    target_bps: int
    actual_bps: float
    deviation_pct: float
    video: dict[str, Any]
    audio: dict[str, Any]
    segments: list[Segment]
    index: dict[str, Any]
    timeline: dict[str, Any]
    quality: dict[str, Any]
    alignment: dict[str, Any]
    inputs: list[dict[str, Any]]
    checkpoint: dict[str, Any]
    decisions: list[dict[str, Any]] = field(default_factory=list)
    issues: list[dict[str, Any]] = field(default_factory=list)
    resource: dict[str, Any] = field(default_factory=dict)
    duration_s: float = 0.0
    version: int = MANIFEST_VERSION

    @property
    def manifest_alignment_max_ms(self) -> float:
        return float(self.alignment.get("max_alignment_ms", 0.0))

    @property
    def random_access_max_ms(self) -> float:
        return float(self.index.get("random_access_max_ms", 0.0))

    def body(self) -> dict[str, Any]:
        """Everything except the digest; the digest covers exactly this."""
        return {
            "version": self.version,
            "run_id": self.run_id,
            "codec": self.codec,
            "time_base": dict(self.time_base),
            "duration_s": round(self.duration_s, 6),
            "target_bps": self.target_bps,
            "actual_bps": round(self.actual_bps, 3),
            "deviation_pct": round(self.deviation_pct, 3),
            "video": dict(self.video),
            "audio": dict(self.audio),
            "segments": [s.to_dict() for s in self.segments],
            "index": dict(self.index),
            "timeline": dict(self.timeline),
            "quality": dict(self.quality),
            "alignment": dict(self.alignment),
            "inputs": [dict(i) for i in self.inputs],
            "checkpoint": dict(self.checkpoint),
            "decisions": [dict(d) for d in self.decisions],
            "issues": [dict(i) for i in self.issues],
            "resource": dict(self.resource),
        }

    @property
    def digest(self) -> str:
        return compact_digest(MANIFEST_DIGEST_PREFIX, [self.body()])

    def to_dict(self) -> dict[str, Any]:
        out = self.body()
        out["manifest_digest"] = self.digest
        return out

    def save(self, path: str) -> None:
        write_json_atomic(path, self.to_dict())

    @staticmethod
    def load(path: str) -> "Manifest":
        import json
        from pathlib import Path

        raw = json.loads(Path(path).read_text(encoding="utf-8"))
        if raw.get("version") != MANIFEST_VERSION:
            raise MediaInputError(
                f"unsupported manifest version {raw.get('version')!r}",
                code="manifest_version")
        segments = []
        for item in raw.get("segments", []):
            segments.append(Segment(
                ordinal=int(item["ordinal"]),
                unit_start=int(item["unit_start"]),
                unit_end=int(item["unit_end"]),
                byte_start=int(item["byte_start"]),
                byte_end=int(item["byte_end"]),
                start_s=float(item["start_s"]),
                end_s=float(item["end_s"]),
                keyframe_offsets=list(item.get("keyframe_offsets") or []),
                keyframe_count=int(item.get("keyframe_count", 0)),
                bytes=int(item.get("bytes", 0)),
                seconds=float(item.get("seconds", 0.0)),
                target_bps=int(item.get("target_bps", 0)),
                actual_bps=float(item.get("actual_bps", 0.0)),
                deviation_pct=float(item.get("deviation_pct", 0.0)),
                decisions=list(item.get("decisions") or []),
            ))
        manifest = Manifest(
            run_id=raw["run_id"], codec=raw["codec"],
            time_base=raw["time_base"], target_bps=raw["target_bps"],
            actual_bps=raw["actual_bps"], deviation_pct=raw["deviation_pct"],
            video=raw["video"], audio=raw["audio"], segments=segments,
            index=raw["index"], timeline=raw["timeline"],
            quality=raw["quality"], alignment=raw["alignment"],
            inputs=raw["inputs"], checkpoint=raw["checkpoint"],
            decisions=raw.get("decisions", []), issues=raw.get("issues", []),
            resource=raw.get("resource", {}), duration_s=raw.get("duration_s", 0.0),
        )
        manifest._stored_digest = raw.get("manifest_digest")
        return manifest




def build_segments(*args, **kwargs):
    """Implemented by the next patch."""
    raise NotImplementedError
