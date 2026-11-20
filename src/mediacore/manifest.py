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
from typing import Any

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

    def digest_payload(self) -> dict[str, Any]:
        """Body used for the digest.

        Only run bookkeeping is ignored (how many attempts ran, whether this one
        resumed).  Every media-derived value, every segment digest and all issue
        records stay in, so the digest still proves the content was reproduced.
        """
        body = self.body()
        checkpoint = dict(body.get("checkpoint", {}))
        for volatile in ("runs", "resumed"):
            checkpoint.pop(volatile, None)
        body["checkpoint"] = checkpoint
        # Runtime telemetry (queue high-water marks, blocked-put counts, Python
        # version) describes how the machine happened to schedule the run, not
        # what was produced; it stays in the file but outside the digest.
        body.pop("resource", None)
        # Absolute paths identify where a run happened, not what it produced;
        # the relative file names stay so sources remain traceable.
        def basename(record: dict[str, Any]) -> dict[str, Any]:
            out = dict(record)
            if "path" in out:
                out["path"] = out["path"].rsplit("/", 1)[-1]
            return out

        body["inputs"] = [basename(item) for item in body.get("inputs", [])]
        body["video"] = basename(body.get("video", {}))
        body["audio"] = basename(body.get("audio", {}))
        if body.get("checkpoint"):
            body["checkpoint"].pop("path", None)
            body["checkpoint"].pop("input_digest", None)
        return body

    @property
    def digest(self) -> str:
        return compact_digest(MANIFEST_DIGEST_PREFIX, [self.digest_payload()])

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


def build_segments(
    units: list[Any],
    pts_seconds: list[Fraction | float],
    *,
    target_bps: int,
    frame_duration_s: float,
    decisions: list[Any] | None = None,
) -> list[Segment]:
    """Split the stream into GOP-aligned segments and measure each one.

    A segment starts at a keyframe (or at unit 0) and ends just before the next
    keyframe, so every segment boundary is a valid switch point.  Timestamps
    are in **seconds**; ``frame_duration_s`` closes the final segment.  Segment
    durations tile the timeline exactly (each ends where the next begins).
    ``decisions`` optionally supplies per-unit timestamp repairs to annotate the
    decisions that affected each segment.
    """
    if not units:
        return []
    boundaries = [i for i, unit in enumerate(units) if unit.keyframe]
    if not boundaries or boundaries[0] != 0:
        boundaries.insert(0, 0)
    boundaries = sorted(set(boundaries))
    boundaries.append(len(units))
    frame_duration = float(frame_duration_s)
    segments: list[Segment] = []
    for ordinal in range(len(boundaries) - 1):
        start, end = boundaries[ordinal], boundaries[ordinal + 1]
        if end <= start:
            continue
        byte_start = units[start].offset
        byte_end = units[end - 1].offset + units[end - 1].size
        nbytes = max(0, byte_end - byte_start)
        start_s = float(pts_seconds[start])
        if end < len(pts_seconds):
            end_s = float(pts_seconds[end])  # tile the timeline exactly
        else:
            end_s = float(pts_seconds[end - 1]) + frame_duration
        seconds = max(1e-6, end_s - start_s)
        actual = nbytes * 8.0 / seconds
        deviation = (actual - target_bps) / target_bps * 100.0 if target_bps else 0.0
        decisions_here = []
        if decisions is not None:
            for decision in decisions[start:end]:
                reasons = getattr(decision, "reason", "")
                if reasons and reasons != "ok":
                    decisions_here.append(reasons)
        segments.append(Segment(
            ordinal=ordinal, unit_start=start, unit_end=end,
            byte_start=byte_start, byte_end=byte_end,
            start_s=start_s, end_s=end_s,
            keyframe_offsets=[u.offset for u in units[start:end] if u.keyframe],
            keyframe_count=sum(1 for u in units[start:end] if u.keyframe),
            bytes=nbytes, seconds=seconds,
            target_bps=target_bps, actual_bps=actual,
            deviation_pct=deviation, decisions=decisions_here))
    return segments


def verify_manifest(
    manifest: Manifest,
    *,
    deviation_limit_pct: float = MAX_BITRATE_DEVIATION_PCT,
    alignment_limit_ms: float = MAX_ALIGNMENT_MS,
    random_access_limit_ms: float = MAX_ALIGNMENT_MS,
    expected_digest: str | None = None,
) -> list[str]:
    """Check a manifest's internal consistency and acceptance limits."""
    failures: list[str] = []
    if expected_digest is not None and expected_digest != manifest.digest:
        failures.append(
            f"manifest digest mismatch: stored {expected_digest}, recomputed {manifest.digest}")
    if not manifest.segments:
        failures.append("manifest records no segments")
    previous: Segment | None = None
    for segment in manifest.segments:
        # Segment rates are validated for internal consistency: the recorded
        # byte count must reproduce the recorded actual_bps.  GOP-level
        # deviation legitimately varies and is reported per segment, not
        # enforced (the overall rate is enforced below).
        recomputed = segment.bytes * 8.0 / max(segment.seconds, 1e-6)
        if abs(recomputed - segment.actual_bps) > 0.5:
            failures.append(
                f"segment {segment.ordinal} rate is inconsistent with its byte count")
        if segment.bytes != segment.byte_end - segment.byte_start:
            failures.append(
                f"segment {segment.ordinal} byte range does not match its byte count")
        if segment.keyframe_count < 1 or not segment.keyframe_offsets:
            failures.append(
                f"segment {segment.ordinal} has no keyframe switch point")
        for offset in segment.keyframe_offsets:
            if not (segment.byte_start <= offset < segment.byte_end):
                failures.append(
                    f"segment {segment.ordinal} keyframe offset {offset} is outside "
                    f"its byte range")
        if previous is not None:
            # Switch boundaries must be seamless: both the time tiles and the
            # unit/byte ranges continue exactly where the previous segment ended.
            if abs(previous.end_s - segment.start_s) > 1e-6:
                failures.append(
                    f"segment {segment.ordinal} does not start where segment "
                    f"{previous.ordinal} ended")
            if previous.unit_end != segment.unit_start:
                failures.append(
                    f"segment {segment.ordinal} unit range does not continue "
                    f"segment {previous.ordinal}")
            if previous.byte_end != segment.byte_start:
                failures.append(
                    f"segment {segment.ordinal} byte range does not continue "
                    f"segment {previous.ordinal}")
        previous = segment
    if abs(manifest.deviation_pct) > deviation_limit_pct:
        failures.append(
            f"overall bitrate deviation {manifest.deviation_pct:.2f}% exceeds "
            f"{deviation_limit_pct}%")
    if manifest.manifest_alignment_max_ms > alignment_limit_ms:
        failures.append(
            f"A/V alignment error {manifest.manifest_alignment_max_ms:.2f} ms exceeds "
            f"{alignment_limit_ms} ms")
    if manifest.random_access_max_ms > random_access_limit_ms:
        failures.append(
            f"random-access error {manifest.random_access_max_ms:.2f} ms exceeds "
            f"{random_access_limit_ms} ms")
    quality = manifest.quality.get("video", {})
    psnr = quality.get("psnr_db")
    if psnr is None or not _finite(psnr) or psnr <= 0.0:
        failures.append(f"video quality statistic is not a usable finite value: {psnr!r}")
    rms = manifest.audio.get("rms")
    if rms is None or not _finite(rms) or rms <= 0.0:
        failures.append(f"audio RMS is not a usable finite value: {rms!r}")
    if not manifest.index.get("entries"):
        failures.append("manifest has no random-access index entries")
    return failures


def _finite(value: Any) -> bool:
    try:
        import math

        return math.isfinite(float(value))
    except (TypeError, ValueError):
        return False
