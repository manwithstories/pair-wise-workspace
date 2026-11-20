"""Round-trip archive manifest.

The manifest is the artefact an archivist reads in ten years, so it has three
properties that the code enforces rather than hopes for:

1. **Lossless round-trip.** ``Manifest.from_dict(json.loads(m.dumps()))`` is
   equal to ``m``; :func:`dump`/``load`` are covered by tests.
2. **Deterministic bytes.** Keys are sorted and floats are rounded to a fixed
   precision before serialisation, so re-running an unchanged archive yields a
   byte-identical manifest (and therefore a comparable digest).
3. **Self-describing stages.** Each stage lists the digest of the inputs it
   consumed, which is what makes an interrupted run safe to resume: a stage
   whose recorded input digest no longer matches is refused, not reused.
"""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass, field
from typing import Any, Iterable

from . import __version__
from .audio import AudioSegment
from .color import ColorRecord
from .errors import MediaError
from .probe import MappingRecord, ProbeResult
from .timeline import Rational, TimeBase, Timeline
from .util import canonical_json, sha256_bytes

SCHEMA = "mediarc.manifest/v1"
#: Bump when a field changes meaning; readers reject unknown majors.
SCHEMA_VERSION = 1

#: Stages that are *recorded* in a manifest, in execution order.  Audio
#: and colour are sub-steps of ``transform`` and appear in error records, not
#: as their own stage.
STAGE_ORDER = ("probe", "timeline", "transform", "archive", "verify")


@dataclass(slots=True)
class StageRecord:
    """One pipeline stage's persisted output and input digest."""

    name: str
    status: str  # "ok" | "partial" | "failed" | "skipped"
    input_sha256: str
    output_sha256: str
    upstream_sha256: str = ""
    errors: list[MediaError] = field(default_factory=list)
    detail: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "status": self.status,
            "input_sha256": self.input_sha256,
            "output_sha256": self.output_sha256,
            "upstream_sha256": self.upstream_sha256,
            "errors": [e.to_dict() for e in self.errors],
            "detail": _round_numbers(self.detail),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "StageRecord":
        name = str(data["name"])
        if name not in STAGE_ORDER:
            raise MediaError(
                "manifest-schema", f"unknown stage {name!r}", stage="archive"
            )
        status = str(data.get("status", "ok"))
        if status not in ("ok", "partial", "failed", "skipped"):
            raise MediaError(
                "manifest-schema", f"unknown stage status {status!r}", stage="archive"
            )
        return cls(
            name=name,
            status=status,
            input_sha256=str(data["input_sha256"]),
            output_sha256=str(data["output_sha256"]),
            upstream_sha256=str(data.get("upstream_sha256", "")),
            errors=[MediaError.from_dict(e) for e in data.get("errors", [])],
            detail=dict(data.get("detail") or {}),
        )


@dataclass(slots=True)
class SegmentRecord:
    """One video segment's verification data and its audio counterpart."""

    segment_id: str
    track_index: int
    start_ticks: int
    end_ticks: int
    path: str
    sha256: str
    keyframe_ticks: list[int] = field(default_factory=list)
    keyframe_hash: str = ""
    audio: AudioSegment | None = None
    status: str = "ok"
    errors: list[MediaError] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "segment_id": self.segment_id,
            "track_index": self.track_index,
            "start_ticks": self.start_ticks,
            "end_ticks": self.end_ticks,
            "path": self.path,
            "sha256": self.sha256,
            "keyframe_ticks": list(self.keyframe_ticks),
            "keyframe_hash": self.keyframe_hash,
            "audio": None if self.audio is None else self.audio.to_dict(),
            "status": self.status,
            "errors": [e.to_dict() for e in self.errors],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "SegmentRecord":
        return cls(
            segment_id=str(data["segment_id"]),
            track_index=int(data["track_index"]),
            start_ticks=int(data["start_ticks"]),
            end_ticks=int(data["end_ticks"]),
            path=str(data["path"]),
            sha256=str(data["sha256"]),
            keyframe_ticks=[int(t) for t in data.get("keyframe_ticks", [])],
            keyframe_hash=str(data.get("keyframe_hash", "")),
            audio=None if data.get("audio") is None else AudioSegment.from_dict(data["audio"]),
            status=str(data.get("status", "ok")),
            errors=[MediaError.from_dict(e) for e in data.get("errors", [])],
        )


@dataclass(slots=True)
class Manifest:
    source: dict[str, Any]
    mapping: MappingRecord
    timeline: Timeline
    stages: list[StageRecord] = field(default_factory=list)
    segments: list[SegmentRecord] = field(default_factory=list)
    color: ColorRecord | None = None
    notes: list[str] = field(default_factory=list)

    # -- construction ------------------------------------------------------
    @classmethod
    def create(
        cls,
        probe_result: ProbeResult,
        timeline: Timeline,
        *,
        color: ColorRecord | None = None,
        notes: Iterable[str] = (),
    ) -> "Manifest":
        track_entries = [
            {
                "index": s.index,
                "kind": s.kind,
                "codec": s.codec,
                "time_base": str(s.time_base),
                "start_raw": s.start_raw,
                "duration_raw": s.duration_raw,
                "sample_rate": s.sample_rate,
                "channels": s.channels,
                "layout": s.layout,
                "width": s.width,
                "height": s.height,
                "framerate": None if s.framerate is None else str(s.framerate),
                "language": s.language,
                "metadata": dict(s.metadata),
                "packet_count": s.packet_count,
                "keyframes": [k.to_dict() for k in s.keyframes],
            }
            for s in probe_result.streams
            if probe_result.mapping.role_of(s.index) is not None
        ]
        return cls(
            source={
                "tool": f"mediarc/{__version__}",
                "path": probe_result.path,
                "size": probe_result.size,
                "format": probe_result.format_name,
                "duration_us": probe_result.duration_us,
                "sha256": probe_result.source_sha256,
                "streams": track_entries,
                "probe_errors": [e.to_dict() for e in probe_result.errors],
                "probe_logs": list(probe_result.logs),
            },
            mapping=probe_result.mapping,
            timeline=timeline,
            color=color,
            notes=list(notes),
        )

    # -- stage bookkeeping -------------------------------------------------
    def stage(self, name: str) -> StageRecord | None:
        return next((s for s in self.stages if s.name == name), None)

    def record_stage(self, record: StageRecord) -> None:
        self.stages = [s for s in self.stages if s.name != record.name]
        self.stages.append(record)
        self.stages.sort(key=lambda s: STAGE_ORDER.index(s.name))

    def require_stage_input(self, name: str, expected: str) -> StageRecord:
        """Refuse to reuse a stage whose recorded input no longer matches."""
        record = self.stage(name)
        if record is None:
            raise MediaError(
                "stage-missing-input",
                f"stage {name} has not been recorded; its output cannot be trusted",
                stage="archive",
            )
        if record.input_sha256 != expected:
            raise MediaError(
                "stage-hash-mismatch",
                f"stage {name} was produced from {record.input_sha256[:16]}, "
                f"current input hashes to {expected[:16]}",
                stage="archive",
                detail={"recorded": record.input_sha256, "current": expected},
            )
        return record

    def failed_segments(self) -> list[SegmentRecord]:
        return [s for s in self.segments if s.status != "ok"]

    def healthy_segments(self) -> list[SegmentRecord]:
        return [s for s in self.segments if s.status == "ok"]

    def verify_chain(self, upto: str = "archive") -> list[MediaError]:
        """Check the recorded stage chain is complete and self-consistent.

        Each stage must be present up to ``upto`` and its output digest must
        equal the next stage's *recorded* input digest.  That makes an
        interrupted run detectable from the manifest alone: a stage whose input
        does not match its predecessor's output cannot have produced the bytes
        the archive claims.
        """
        from .errors import MediaError as _Error

        errors: list[_Error] = []
        wanted = STAGE_ORDER[: STAGE_ORDER.index(upto) + 1]
        recorded = {stage.name: stage for stage in self.stages}
        for name in wanted:
            if name not in recorded:
                errors.append(
                    _Error(
                        "stage-missing-input",
                        f"manifest has no record for stage {name}",
                        stage="verify",
                    )
                )
        chain = [recorded[name] for name in wanted if name in recorded]
        for previous, following in zip(chain, chain[1:]):
            # A partial stage records the error that broke the chain; only a
            # successful predecessor is required to line up with its successor.
            if previous.status != "ok":
                continue
            if not following.upstream_sha256:
                continue
            if following.upstream_sha256 != previous.output_sha256:
                errors.append(
                    _Error(
                        "stage-hash-mismatch",
                        f"stage {following.name} was built on upstream "
                        f"{following.upstream_sha256[:16]}, but stage "
                        f"{previous.name} produced {previous.output_sha256[:16]}",
                        stage="verify",
                        detail={
                            "upstream_recorded": following.upstream_sha256,
                            "upstream_actual": previous.output_sha256,
                        },
                    )
                )
        return errors

    # -- digests -----------------------------------------------------------
    def digest(self) -> str:
        return sha256_bytes(canonical_json(self.to_dict()))

    # -- serialisation -----------------------------------------------------
    def to_dict(self) -> dict[str, Any]:
        return {
            "schema": SCHEMA,
            "schema_version": SCHEMA_VERSION,
            "source": _round_numbers(self.source),
            "mapping": self.mapping.to_dict(),
            "timeline": self.timeline.to_dict(),
            "color": None if self.color is None else self.color.to_dict(),
            "stages": [s.to_dict() for s in self.stages],
            "segments": [s.to_dict() for s in self.segments],
            "notes": list(self.notes),
        }

    def dumps(self, *, indent: int | None = 2) -> str:
        payload = self.to_dict()
        if indent is None:
            return canonical_json(payload).decode("utf-8")
        return json.dumps(payload, sort_keys=True, indent=indent, ensure_ascii=False) + "\n"

    def write(self, path: str) -> str:
        from .util import atomic_write_bytes

        data = (
            json.dumps(self.to_dict(), sort_keys=True, indent=2, ensure_ascii=False)
            + "\n"
        ).encode("utf-8")
        atomic_write_bytes(path, data)
        return hashlib.sha256(data).hexdigest()

    @classmethod
    def loads(cls, text: str) -> "Manifest":
        try:
            data = json.loads(text)
        except json.JSONDecodeError as exc:
            raise MediaError(
                "manifest-corrupt", f"manifest is not valid JSON: {exc}", stage="archive"
            ) from exc
        return cls.from_dict(data)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Manifest":
        _check_object(data, "manifest")
        schema = data.get("schema")
        if schema != SCHEMA:
            raise MediaError(
                "manifest-schema", f"unsupported schema {schema!r}", stage="archive"
            )
        version = data.get("schema_version")
        if not isinstance(version, int) or version > SCHEMA_VERSION:
            raise MediaError(
                "manifest-schema",
                f"manifest version {version!r} is newer than this tool supports",
                stage="archive",
            )
        source = _check_object(data.get("source"), "source")
        for key in ("path", "sha256", "streams"):
            if key not in source:
                raise MediaError(
                    "manifest-schema", f"source.{key} is missing", stage="archive"
                )
        if not isinstance(source["streams"], list):
            raise MediaError(
                "manifest-schema", "source.streams must be a list", stage="archive"
            )
        mapping = _check_object(data.get("mapping"), "mapping")
        timeline = _check_object(data.get("timeline"), "timeline")
        for key in ("time_base", "tracks", "segments"):
            if key not in timeline:
                raise MediaError(
                    "manifest-schema", f"timeline.{key} is missing", stage="archive"
                )
        manifest = cls(
            source=source,
            mapping=MappingRecord.from_dict(mapping),
            timeline=Timeline.from_dict(timeline),
            stages=[StageRecord.from_dict(s) for s in data.get("stages", [])],
            segments=[SegmentRecord.from_dict(s) for s in data.get("segments", [])],
            color=None if data.get("color") is None else ColorRecord.from_dict(data["color"]),
            notes=[str(n) for n in data.get("notes", [])],
        )
        return manifest

    # -- convenience -------------------------------------------------------
    def summary(self) -> dict[str, Any]:
        total = len(self.segments)
        ok = len(self.healthy_segments())
        return {
            "source": self.source.get("path"),
            "format": self.source.get("format"),
            "segments": total,
            "healthy": ok,
            "failed": total - ok,
            "stage_status": {s.name: s.status for s in self.stages},
            "digest": self.digest()[:16],
        }


def verify_record_path(manifest_path: str | os.PathLike[str]) -> "Path":
    """Sidecar that records the verification stage for a manifest."""
    from pathlib import Path

    path = Path(manifest_path)
    return path.with_name(path.name + ".verify.json")


def stage_input_digest(**parts: Any) -> str:
    """Deterministic digest of a stage's inputs (paths, hashes, settings)."""
    return sha256_bytes(canonical_json(_round_numbers(parts)))


def _check_object(value: Any, name: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise MediaError(
            "manifest-schema", f"{name} must be a JSON object", stage="archive"
        )
    return value


def _round_numbers(value: Any, places: int = 6) -> Any:
    """Round floats so JSON stays byte-stable between runs."""
    if isinstance(value, float):
        if value != value or value in (float("inf"), float("-inf")):
            return None
        return round(value, places)
    if isinstance(value, dict):
        return {str(k): _round_numbers(v, places) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_round_numbers(v, places) for v in value]
    return value


def keyframe_digest(ticks: Iterable[int]) -> str:
    """Stable digest of a keyframe index (the thing verify re-checks)."""
    return sha256_bytes(canonical_json(sorted(int(t) for t in ticks)))


def time_base_of(manifest: Manifest) -> TimeBase:
    return TimeBase(Rational.parse(manifest.timeline.to_dict()["time_base"]))
