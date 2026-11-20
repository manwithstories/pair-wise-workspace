"""Shared segment manifest with cross-rendition alignment checks.

The manifest is the single contract every client reads: one boundary list, one
segment per representation per boundary, per-segment SHA-256, and the
colour/transition decisions that produced them.  It is written
deterministically (sorted keys, rational times) and verified by re-reading it
from disk and comparing segment inventories and alignments.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Iterable, Sequence

from .encode import EncodedSegment, Rendition
from .errors import Issue, MediaLocation
from .model import Rate, format_rational
from .normalize import ColorDecision
from .segment import SegmentPlan

MANIFEST_NAME = "manifest.json"
ALIGNMENT_TOLERANCE = Fraction(0)


@dataclass
class Manifest:
    version: int
    window: dict[str, str]
    video_rate: str
    audio_rate: int
    segment_duration: str
    renditions: list[dict[str, Any]]
    segments: list[dict[str, Any]]
    color: dict[str, Any] | None = None
    transitions: list[dict[str, Any]] = field(default_factory=list)
    inputs: list[dict[str, Any]] = field(default_factory=list)
    checksums: dict[str, str] = field(default_factory=dict)
    stats: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "window": self.window,
            "video_rate": self.video_rate,
            "audio_rate": self.audio_rate,
            "segment_duration": self.segment_duration,
            "renditions": self.renditions,
            "segments": self.segments,
            "color": self.color,
            "transitions": self.transitions,
            "inputs": self.inputs,
            "checksums": self.checksums,
            "stats": self.stats,
        }

    def serialize(self) -> str:
        return json.dumps(self.to_dict(), indent=2, sort_keys=True, ensure_ascii=False) + "\n"


def build_manifest(
    plan: SegmentPlan,
    segments_by_rendition: dict[str, list[EncodedSegment]],
    renditions: Sequence[Rendition],
    window: dict[str, str],
    segment_duration: Fraction,
    color: ColorDecision | None = None,
    transitions: Iterable[dict[str, Any]] = (),
    inputs: Iterable[dict[str, Any]] = (),
) -> Manifest:
    rendition_entries = [r.to_dict() for r in sorted(renditions, key=lambda r: r.name)]
    by_index: dict[int, dict[str, EncodedSegment]] = {}
    for name, encoded in segments_by_rendition.items():
        for item in encoded:
            by_index.setdefault(item.index, {})[name] = item
    segments: list[dict[str, Any]] = []
    checksums: dict[str, str] = {}
    for planned in plan.segments:
        item: dict[str, Any] = {
            "index": planned.index,
            "start": format_rational(planned.start),
            "end": format_rational(planned.end),
            "duration": format_rational(planned.duration),
            "video_start_frame": planned.video_start_frame,
            "audio_start_sample": planned.audio_start_sample,
            "video_frames": planned.video_frames,
            "audio_samples": planned.audio_samples,
            "is_short": planned.is_short,
            "kind": planned.kind,
            "renditions": {},
        }
        for rendition in sorted(segments_by_rendition):
            encoded = by_index.get(planned.index, {}).get(rendition)
            if encoded is None:
                continue
            item["renditions"][rendition] = {
                "path": os.path.basename(encoded.path),
                "sha256": encoded.sha256,
                "size": encoded.size,
                "actual_video_frames": encoded.actual_video_frames,
                "actual_audio_samples": encoded.actual_audio_samples,
            }
        checksums[f"segment:{planned.index}"] = _segment_checksum(item)
        segments.append(item)
    manifest = Manifest(
        version=1,
        window=window,
        video_rate=str(plan.video_rate),
        audio_rate=plan.audio_rate,
        segment_duration=format_rational(segment_duration),
        renditions=rendition_entries,
        segments=segments,
        color=color.to_dict() if color is not None else None,
        transitions=list(transitions),
        inputs=list(inputs),
        checksums=checksums,
        stats=_stats(segments),
    )
    manifest.checksums["manifest"] = _manifest_checksum(manifest)
    return manifest


def _segment_checksum(item: dict[str, Any]) -> str:
    import hashlib

    payload = json.dumps(item, sort_keys=True).encode("utf-8")
    return hashlib.sha256(payload).hexdigest()


def _manifest_checksum(manifest: Manifest) -> str:
    import hashlib

    payload = json.dumps(
        {
            "segments": manifest.segments,
            "renditions": manifest.renditions,
            "window": manifest.window,
            "video_rate": manifest.video_rate,
            "audio_rate": manifest.audio_rate,
            "segment_duration": manifest.segment_duration,
        },
        sort_keys=True,
    ).encode("utf-8")
    return hashlib.sha256(payload).hexdigest()


def _stats(segments: list[dict[str, Any]]) -> dict[str, Any]:
    total = len(segments)
    short = [s for s in segments if s["is_short"]]
    durations = {s["duration"] for s in segments}
    return {
        "segment_count": total,
        "short_segment_count": len(short),
        "distinct_durations": len(durations),
        "total_duration": format_rational(
            sum((Fraction(s["duration"]) for s in segments), Fraction(0))
        ),
    }


def write_manifest(manifest: Manifest, workdir: str) -> str:
    """Atomically write the manifest, byte-identical for identical content."""
    from .normalize import atomic_temp

    os.makedirs(workdir, exist_ok=True)
    target = os.path.join(workdir, MANIFEST_NAME)
    temp = atomic_temp(workdir, ".json")
    try:
        with open(temp, "w", encoding="utf-8") as handle:
            handle.write(manifest.serialize())
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, target)
    finally:
        if os.path.exists(temp):
            os.unlink(temp)
    return target


def load_manifest(path: str) -> Manifest:
    with open(path, "r", encoding="utf-8") as handle:
        payload = json.load(handle)
    if int(payload.get("version", 0)) != 1:
        raise ValueError(f"unsupported manifest version {payload.get('version')!r}")
    return Manifest(
        version=int(payload["version"]),
        window=payload["window"],
        video_rate=payload["video_rate"],
        audio_rate=int(payload["audio_rate"]),
        segment_duration=payload["segment_duration"],
        renditions=payload["renditions"],
        segments=payload["segments"],
        color=payload.get("color"),
        transitions=payload.get("transitions", []),
        inputs=payload.get("inputs", []),
        checksums=payload.get("checksums", {}),
        stats=payload.get("stats", {}),
    )


def check_alignment(
    manifest: Manifest,
    expected_renditions: Sequence[str] | None = None,
    video_rate: Rate | None = None,
) -> list[Issue]:
    """Verify every rendition carries the same boundaries and segment inventory."""
    issues: list[Issue] = []
    names = expected_renditions or [r["name"] for r in manifest.renditions]
    rate = video_rate or Rate.parse(manifest.video_rate)
    step = rate.frame_duration
    audio_rate = manifest.audio_rate
    for segment in manifest.segments:
        index = segment["index"]
        start = Fraction(segment["start"])
        expected_frame = _frame_index(start, step)
        if segment["video_start_frame"] != expected_frame:
            issues.append(
                Issue(
                    code="frame_mismatch",
                    message=(
                        f"segment {index} start frame {segment['video_start_frame']} "
                        f"does not match {expected_frame}"
                    ),
                    location=MediaLocation(pts=start),
                )
            )
        expected_sample = _sample_index(start, audio_rate)
        if segment["audio_start_sample"] != expected_sample:
            issues.append(
                Issue(
                    code="sample_mismatch",
                    message=(
                        f"segment {index} start sample {segment['audio_start_sample']} "
                        f"does not match {expected_sample}"
                    ),
                    location=MediaLocation(pts=start),
                )
            )
        frame_bytes = Fraction(segment["video_start_frame"]) * step
        sample_time = Fraction(segment["audio_start_sample"], audio_rate)
        if abs(frame_bytes - start) > ALIGNMENT_TOLERANCE or abs(sample_time - start) > ALIGNMENT_TOLERANCE:
            issues.append(
                Issue(
                    code="boundary_error",
                    message=(
                        f"segment {index} boundary {start} is represented as "
                        f"{frame_bytes} (frame) and {sample_time} (sample)"
                    ),
                    location=MediaLocation(pts=start),
                )
            )
        present = set(segment["renditions"])
        missing = [name for name in names if name not in present]
        extra = sorted(present - set(names))
        if missing:
            issues.append(
                Issue(
                    code="missing_rendition",
                    message=f"segment {index} is missing rendition(s) {missing}",
                    location=MediaLocation(pts=start),
                    context={"missing": missing},
                )
            )
        if extra:
            issues.append(
                Issue(
                    code="unexpected_rendition",
                    message=f"segment {index} has undeclared rendition(s) {extra}",
                    location=MediaLocation(pts=start),
                )
            )
        for name, entry in segment["renditions"].items():
            if entry["actual_video_frames"] != segment["video_frames"]:
                issues.append(
                    Issue(
                        code="encoded_frame_count",
                        message=(
                            f"segment {index} {name} encodes {entry['actual_video_frames']} "
                            f"frames, planned {segment['video_frames']}"
                        ),
                        location=MediaLocation(pts=start),
                        context={"rendition": name},
                    )
                )
            planned_samples = segment["audio_samples"]
            actual_samples = entry["actual_audio_samples"]
            # AAC pads to whole blocks; allow one block of padding, but a
            # *short* tail must stay short.
            if segment["is_short"] and actual_samples > planned_samples + 1024:
                issues.append(
                    Issue(
                        code="short_segment_padded",
                        message=(
                            f"segment {index} {name} is a short tail but encoded "
                            f"{actual_samples} samples against {planned_samples}"
                        ),
                        location=MediaLocation(pts=start),
                        context={"rendition": name, "short": True},
                    )
                )
            elif not segment["is_short"] and actual_samples < planned_samples:
                issues.append(
                    Issue(
                        code="encoded_sample_count",
                        message=(
                            f"segment {index} {name} encodes {actual_samples} samples, "
                            f"planned {planned_samples}"
                        ),
                        location=MediaLocation(pts=start),
                        context={"rendition": name},
                    )
                )
    # Boundary continuity: segment N's end equals segment N+1's start.
    for previous, current in zip(manifest.segments, manifest.segments[1:]):
        if previous["end"] != current["start"]:
            issues.append(
                Issue(
                    code="boundary_gap",
                    message=(
                        f"segment {previous['index']} ends at {previous['end']} but "
                        f"segment {current['index']} starts at {current['start']}"
                    ),
                    location=MediaLocation(pts=Fraction(current["start"])),
                )
            )
    return issues


def boundary_errors(plan: SegmentPlan) -> dict[int, Fraction]:
    """Per-boundary absolute error between the rational time and both representations."""
    errors: dict[int, Fraction] = {}
    step = plan.video_rate.frame_duration
    for segment in plan.segments:
        frame_time = Fraction(segment.video_start_frame) * step
        sample_time = Fraction(segment.audio_start_sample, plan.audio_rate)
        errors[segment.index] = max(abs(frame_time - segment.start), abs(sample_time - segment.start))
    return errors


def _frame_index(time: Fraction, step: Fraction) -> int:
    units = time / step
    lower = units.numerator // units.denominator
    remainder = units - lower
    if remainder > Fraction(1, 2):
        return lower + 1
    if remainder == Fraction(1, 2):
        return lower if lower % 2 == 0 else lower + 1
    return lower


def _sample_index(time: Fraction, rate: int) -> int:
    units = time * rate
    lower = units.numerator // units.denominator
    remainder = units - lower
    if remainder > Fraction(1, 2):
        return lower + 1
    if remainder == Fraction(1, 2):
        return lower if lower % 2 == 0 else lower + 1
    return lower


def manifest_checksum_matches(manifest: Manifest) -> bool:
    expected = _manifest_checksum(manifest)
    return manifest.checksums.get("manifest") == expected
