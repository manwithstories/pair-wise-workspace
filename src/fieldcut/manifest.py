"""Generate and validate the multi-bitrate delivery manifest.

The manifest is the proof that every representation describes the same media
intervals: one shared ordered segment list, and per-representation entries
whose start/end times and sequence numbers must match. Validation detects
time jumps, duplicate timestamps and missing segments, and reports the *first*
inconsistent interval so the CLI can fail deterministically.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field

from .errors import ErrorLog, Recovery, Stage
from .ladder import RepresentationResult
from .segment import SegmentationPlan
from .timebase import TICKS_PER_MS

# Cross-representation boundary divergence above this is a hard failure.
ALIGNMENT_LIMIT_MS = 40.0


@dataclass
class ManifestIssue:
    kind: str                       # jump | duplicate | missing | misaligned
    segment_index: int
    camera: str | None
    detail: str
    interval_ms: tuple[float, float] | None = None


@dataclass
class Manifest:
    shared_segments: list[dict] = field(default_factory=list)
    representations: list[dict] = field(default_factory=list)
    issues: list[ManifestIssue] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.issues

    def to_dict(self) -> dict:
        return {
            "shared_segments": self.shared_segments,
            "representations": self.representations,
            "issues": [i.__dict__ for i in self.issues],
        }


def _ms(ticks: int) -> float:
    return round(ticks / TICKS_PER_MS, 3)


def build_manifest(
    plan: SegmentationPlan,
    reps: list[RepresentationResult],
    log: ErrorLog,
) -> Manifest:
    """Assemble the manifest and run every cross-representation check."""
    manifest = Manifest()

    # One authoritative shared segment timeline (sequence + start/end).
    for seg in plan.segments:
        manifest.shared_segments.append({
            "index": seg.index,
            "start_ms": _ms(seg.start_ticks),
            "end_ms": _ms(seg.end_ticks),
            "duration_ms": _ms(seg.duration_ticks),
            "damaged": seg.damaged,
            "short_tail": seg.is_short_tail,
        })

    # Group encoded reps by camera so each camera's rungs form a ladder.
    by_camera: dict[str, list[RepresentationResult]] = {}
    for rep in reps:
        by_camera.setdefault(rep.camera, []).append(rep)

    for camera, camera_reps in sorted(by_camera.items()):
        for rep in sorted(camera_reps, key=lambda r: r.representation):
            entry = _representation_entry(camera, rep, plan, manifest, log)
            manifest.representations.append(entry)

    _validate_shared_axis(manifest, log)
    return manifest


def _representation_entry(camera: str, rep: RepresentationResult,
                          plan: SegmentationPlan,
                          manifest: Manifest,
                          log: ErrorLog) -> dict:
    seg_entries = []
    encoded = {m.index: m for m in rep.segments}
    for seg in plan.segments:
        metric = encoded.get(seg.index)
        if metric is None or metric.actual_bps == 0:
            manifest.issues.append(ManifestIssue(
                kind="missing", segment_index=seg.index, camera=camera,
                detail=f"representation {rep.representation} has no usable "
                       "encoded segment",
                interval_ms=(_ms(seg.start_ticks), _ms(seg.end_ticks))))
            log.add(Stage.MANIFEST, camera,
                    f"missing segment {seg.index} in representation "
                    f"{rep.representation}",
                    media_time_ticks=seg.start_ticks,
                    recovery=Recovery.UNRECOVERABLE)
            continue
        # Boundary alignment vs the shared plan (encoder landing accuracy).
        start_delta = abs(metric.duration_ms
                          - (seg.end_ticks - seg.start_ticks) / TICKS_PER_MS)
        seg_entries.append({
            "index": seg.index,
            "file": os.path.relpath(metric.path, start=os.path.dirname(
                os.path.dirname(metric.path))),
            "start_ms": _ms(seg.start_ticks),
            "end_ms": _ms(seg.end_ticks),
            "duration_ms": metric.duration_ms,
            "bitrate_target_bps": metric.target_bps,
            "bitrate_actual_bps": metric.actual_bps,
            "bitrate_peak_bps": metric.peak_bps,
            "bitrate_error_pct": metric.bitrate_error_pct,
            "psnr_db": metric.psnr_db,
            "psnr_reason": metric.psnr_reason,
            "frames": metric.frames,
        })
    return {
        "camera": camera,
        "representation": rep.representation,
        "target_bps": rep.target_bps,
        "resolution": f"{rep.width}x{rep.height}",
        "segments": seg_entries,
    }


def _validate_shared_axis(manifest: Manifest, log: ErrorLog) -> None:
    """Detect jumps/duplicates on the shared axis and cross-rep misalignment."""
    shared = manifest.shared_segments
    prev_end = None
    for seg in shared:
        if seg["end_ms"] <= seg["start_ms"]:
            manifest.issues.append(ManifestIssue(
                "duplicate", seg["index"], None,
                "non-positive segment duration",
                (seg["start_ms"], seg["end_ms"])))
        if prev_end is not None:
            gap = round(seg["start_ms"] - prev_end, 3)
            if abs(gap) > 1.0:
                kind = "jump" if gap > 0 else "duplicate"
                manifest.issues.append(ManifestIssue(
                    kind, seg["index"], None,
                    f"{kind} of {gap} ms between adjacent segments",
                    (prev_end, seg["start_ms"])))
        prev_end = seg["end_ms"]

    # Every representation must cover the same sequence count and boundaries.
    expected_count = len(shared)
    for rep in manifest.representations:
        segs = rep["segments"]
        if len(segs) != expected_count:
            first_bad = next((s["index"] for s in segs), 0)
            manifest.issues.append(ManifestIssue(
                "missing", first_bad, rep["camera"],
                f"representation {rep['representation']} has {len(segs)} "
                f"segments vs shared {expected_count}", None))
        for shared_seg, seg in zip(shared, segs):
            if seg["index"] != shared_seg["index"]:
                manifest.issues.append(ManifestIssue(
                    "misaligned", seg["index"], rep["camera"],
                    f"sequence number mismatch "
                    f"({seg['index']} != {shared_seg['index']})",
                    (seg["start_ms"], seg["end_ms"])))
                break
            start_delta = abs(seg["start_ms"] - shared_seg["start_ms"])
            end_delta = abs(seg["end_ms"] - shared_seg["end_ms"])
            if max(start_delta, end_delta) > ALIGNMENT_LIMIT_MS:
                manifest.issues.append(ManifestIssue(
                    "misaligned", seg["index"], rep["camera"],
                    f"boundary diverges {max(start_delta, end_delta):.2f} ms "
                    f"(limit {ALIGNMENT_LIMIT_MS} ms)",
                    (shared_seg["start_ms"], shared_seg["end_ms"])))
                log.add(Stage.MANIFEST, rep["camera"],
                        f"segment {seg['index']} misaligned across ladder",
                        recovery=Recovery.UNRECOVERABLE)
                break


def first_inconsistency(manifest: Manifest) -> dict | None:
    """Return the first issue as a structured record, or None when clean."""
    if not manifest.issues:
        return None
    issue = manifest.issues[0]
    return {
        "kind": issue.kind,
        "segment_index": issue.segment_index,
        "camera": issue.camera,
        "detail": issue.detail,
        "interval_ms": issue.interval_ms,
    }


def write_manifest(manifest: Manifest, path: str) -> None:
    """Atomically write the manifest JSON next to the segment tree."""
    directory = os.path.dirname(path) or "."
    os.makedirs(directory, exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(manifest.to_dict(), f, indent=2, ensure_ascii=False)
    os.replace(tmp, path)
