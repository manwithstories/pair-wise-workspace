"""Pipeline orchestration: parse -> transform -> verify, staged on disk.

Each stage writes a JSON record holding:

* ``input_sha256`` - digest of everything the stage consumed,
* ``output_sha256`` - digest of what it produced,
* the output payload itself.

On ``--resume`` a stage is reused only when its recorded input digest equals the
current one.  A changed input raises ``stage-hash-mismatch`` instead of silently
mixing stale artefacts with fresh ones.

Failure isolation happens at the segment level: a segment that cannot be
archived is recorded with its locatable error and the remaining segments still
archive, so one damaged GOP never costs a whole master.
"""

from __future__ import annotations

import os
import shutil
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Any, Iterable

import av

from .audio import AudioSegment, AudioSettings, extract_segment
from .color import ColorRecord, resolve as resolve_color
from .errors import MediaError, bridge
from .manifest import (
    Manifest,
    SegmentRecord,
    StageRecord,
    keyframe_digest,
    stage_input_digest,
)
from .probe import ProbeResult, StreamProbe, probe
from .timeline import Segment, Timeline, Track, contiguous_segments
from .util import atomic_write_json, iter_packets, load_json, sha256_file

STAGE_DIR = "stages"
SEGMENT_DIR = "segments"


@dataclass(slots=True)
class Limits:
    max_file_bytes: int = 512 * 1024 * 1024
    max_segment_seconds: float = 120.0
    jobs: int = 2

    def validate(self) -> None:
        if self.max_file_bytes <= 0:
            raise MediaError("limit-exceeded", "max file size must be positive", stage="archive")
        if self.max_segment_seconds <= 0:
            raise MediaError("limit-exceeded", "segment duration must be positive", stage="archive")
        if not 1 <= self.jobs <= 8:
            raise MediaError(
                "limit-exceeded", "parallel jobs must be between 1 and 8", stage="archive"
            )

    def to_dict(self) -> dict[str, Any]:
        return {
            "max_file_bytes": self.max_file_bytes,
            "max_segment_seconds": self.max_segment_seconds,
            "jobs": self.jobs,
        }


@dataclass(slots=True)
class StageStore:
    """On-disk stage records; the basis of safe resume."""

    root: Path
    resume: bool = False

    def prepare(self) -> None:
        if not self.resume and self.root.exists():
            shutil.rmtree(self.root)
        (self.root / STAGE_DIR).mkdir(parents=True, exist_ok=True)
        (self.root / SEGMENT_DIR).mkdir(parents=True, exist_ok=True)

    def path(self, name: str) -> Path:
        return self.root / STAGE_DIR / f"{name}.json"

    def load(self, name: str, expected_input: str) -> dict[str, Any] | None:
        if not self.resume:
            return None
        record_path = self.path(name)
        if not record_path.exists():
            return None
        payload = load_json(record_path, stage="archive")
        if not isinstance(payload, dict):
            raise MediaError(
                "manifest-corrupt", f"stage record {name} is not an object",
                stage="archive", path=str(record_path),
            )
        recorded = payload.get("input_sha256")
        if recorded != expected_input:
            raise MediaError(
                "stage-hash-mismatch",
                f"stage {name} was produced from {str(recorded)[:16]}, current input "
                f"hashes to {expected_input[:16]}",
                stage="archive", path=str(record_path),
                detail={"recorded": recorded, "current": expected_input},
            )
        output = payload.get("output")
        return output if isinstance(output, dict) else None

    def save(self, name: str, input_sha: str, output_sha: str, output: dict[str, Any]) -> None:
        atomic_write_json(
            self.path(name),
            {
                "stage": name,
                "input_sha256": input_sha,
                "output_sha256": output_sha,
                "output": output,
            },
        )


@dataclass(slots=True)
class ArchiveReport:
    manifest: Manifest
    errors: list[MediaError] = field(default_factory=list)
    recovered_segments: int = 0
    stage_status: dict[str, str] = field(default_factory=dict)
    resumed_stages: list[str] = field(default_factory=list)

    @property
    def healthy(self) -> int:
        return len(self.manifest.healthy_segments())

    @property
    def failed(self) -> int:
        return len(self.manifest.failed_segments())

    def summary_lines(self) -> list[str]:
        lines = [
            f"archive source={self.manifest.source['path']} "
            f"format={self.manifest.source['format']}",
            f"segments_total={len(self.manifest.segments)} "
            f"segments_ok={self.healthy} segments_failed={self.failed}",
            f"recovered_segments={self.recovered_segments}",
        ]
        for name, status in self.stage_status.items():
            suffix = " resumed=1" if name in self.resumed_stages else ""
            lines.append(f"stage {name} status={status}{suffix}")
        offset = min((e.offset for e in self.errors if e.offset is not None), default=None)
        lines.append("error_offset=" + (str(offset) if offset is not None else "unknown"))
        return lines


# ---------------------------------------------------------------------------
# Timeline construction
# ---------------------------------------------------------------------------


def _stream_end_seconds(stream: StreamProbe) -> float:
    last = stream.packets[-1]
    if last.pts is None:
        return 0.0
    end = Fraction(last.pts) * stream.time_base.value
    if last.duration:
        end += Fraction(last.duration) * stream.time_base.value
    return float(end)


def build_timeline(
    result: ProbeResult,
    limits: Limits,
    *,
    cut_every_seconds: float | None = None,
) -> tuple[Timeline, list[MediaError]]:
    """Tracks and segments in the probe's integer tick base.

    Video segments split on keyframes plus the duration ceiling; audio segments
    split on the same tick positions so the two stay aligned.
    """
    tb = result.time_base
    rate = tb.rate.value
    errors: list[MediaError] = []
    mapped = [s for s in result.streams if result.mapping.role_of(s.index) is not None]
    declared_end = tb.to_ticks(Fraction(result.duration_us or 0, 1_000_000))
    end_ticks = declared_end
    tracks: list[Track] = []
    per_stream_cuts: dict[int, list[int]] = {}
    for stream in mapped:
        if not stream.packets:
            errors.append(
                MediaError(
                    "missing-stream",
                    f"mapped {stream.kind} stream has no packets; isolated from this archive",
                    stage="timeline", path=result.path, stream_index=stream.index,
                )
            )
            continue
        stream_end = tb.to_ticks(Fraction(_stream_end_seconds(stream)))
        stream_start = tb.to_ticks(Fraction(stream.start_raw or 0) * stream.time_base.value)
        end_ticks = max(end_ticks, stream_end)
        tracks.append(
            Track(
                index=stream.index,
                kind=stream.kind,
                codec=stream.codec,
                time_base=stream.time_base,
                start_ticks=stream_start,
                duration_ticks=max(0, stream_end - stream_start),
                language=stream.language,
            )
        )
        per_stream_cuts[stream.index] = _cut_points(
            stream, stream_start, stream_end, limits, cut_every_seconds, rate
        )
    segments: list[Segment] = []
    for stream in mapped:
        if stream.index not in per_stream_cuts:
            continue
        track = next(t for t in tracks if t.index == stream.index)
        end = track.start_ticks + (track.duration_ticks or 0)
        segments.extend(
            _segments_for(
                stream, track.start_ticks, end, per_stream_cuts[stream.index]
            )
        )
    return Timeline(tb, end_ticks, tracks, segments), errors


def _cut_points(
    stream: StreamProbe,
    start: int,
    end: int,
    limits: Limits,
    cut_every_seconds: float | None,
    rate: int,
) -> list[int]:
    ceiling = int(limits.max_segment_seconds * rate)
    if cut_every_seconds:
        ceiling = min(ceiling, int(cut_every_seconds * rate))
    if stream.kind == "video" and stream.keyframes:
        cuts = sorted({k.ticks for k in stream.keyframes if start < k.ticks < end})
    else:
        cuts = []
    bounds = [start, *cuts, end]
    enforced: list[int] = [bounds[0]]
    for bound in bounds[1:]:
        while bound - enforced[-1] > ceiling:
            enforced.append(enforced[-1] + ceiling)
        if bound > enforced[-1]:
            enforced.append(bound)
    return enforced[1:-1]


def _segments_for(
    stream: StreamProbe, start: int, end: int, cuts: list[int]
) -> list[Segment]:
    segments = contiguous_segments(
        stream.index, cuts, end,
        source_origin=start,
        prefix=f"{stream.kind[0]}{stream.index:02d}_",
    )
    for segment in segments:
        segment.mapping = {
            "kind": stream.kind,
            "codec": stream.codec,
            "role": "video" if stream.kind == "video" else "audio",
        }
    return segments


# ---------------------------------------------------------------------------
# Segment transforms
# ---------------------------------------------------------------------------


def _color_values(record: ColorRecord) -> list[tuple[str, int]]:
    values: list[tuple[str, int]] = []
    for name, attr in (
        ("primaries", "color_primaries"),
        ("transfer", "color_trc"),
        ("space", "colorspace"),
        ("range", "color_range"),
    ):
        entry = record.fields.get(name)
        if entry is not None and entry.value is not None and entry.supported:
            values.append((attr, int(entry.value)))
    return values


def _color_tags(record: ColorRecord) -> dict[str, str]:
    tags: dict[str, str] = {}
    for name, key in (
        ("primaries", "COLOR_PRIMARIES"),
        ("transfer", "COLOR_TRANSFER"),
        ("space", "COLOR_SPACE"),
        ("range", "COLOR_RANGE"),
    ):
        entry = record.fields.get(name)
        if entry is not None and entry.label:
            tags[key] = entry.label
    return tags


def remux_video_segment(
    result: ProbeResult,
    stream: StreamProbe,
    segment: Segment,
    output_path: str,
    color: ColorRecord,
) -> SegmentRecord:
    """Stream-copy one segment to MP4, preserving colour and source offsets."""
    rate = result.time_base.rate.value
    start_ticks = segment.source_start_ticks if segment.source_start_ticks is not None else segment.start_ticks
    end_ticks = segment.source_end_ticks if segment.source_end_ticks is not None else segment.end_ticks
    start_seconds = float(Fraction(start_ticks) / rate)
    end_seconds = float(Fraction(end_ticks) / rate)
    container = av.open(result.path)
    output = None
    try:
        input_stream = container.streams[stream.index]
        packets = iter_packets(container, input_stream, Fraction(start_ticks) / rate)
        output = av.open(output_path, "w", format="mp4")
        output.bit_exact = True
        output_stream = output.add_stream(template=input_stream)
        for attr, value in _color_values(color):
            try:
                setattr(output_stream.codec_context, attr, value)
            except Exception:  # noqa: BLE001 - best-effort metadata carry
                pass
        for key, value in _color_tags(color).items():
            output_stream.metadata[key] = value
        base: int | None = None
        keyframes: list[int] = []
        count = 0
        for packet in packets:
            if packet.pts is None and packet.dts is None:
                continue
            pts = packet.pts if packet.pts is not None else packet.dts
            when = float(Fraction(pts) * input_stream.time_base)
            duration = float(Fraction(packet.duration or 0) * input_stream.time_base)
            if when + duration <= start_seconds + 1e-9:
                continue
            if when >= end_seconds - 1e-9:
                break
            if base is None:
                base = packet.dts if packet.dts is not None else pts
            source_pts = packet.pts
            source_dts = packet.dts if packet.dts is not None else packet.pts
            # Some demuxers omit DTS on the first packet only.  The MP4 muxer
            # warns and guesses when DTS is absent, so fall back to PTS for that
            # packet rather than leaving the field unset.
            if packet.pts is not None:
                packet.pts -= base
            if source_dts is not None:
                packet.dts = source_dts - base
            packet.stream = output_stream
            output.mux(packet)
            count += 1
            if packet.is_keyframe and source_pts is not None:
                keyframes.append(
                    result.time_base.to_ticks(
                        Fraction(source_pts) * stream.time_base.value
                    )
                )
        if count == 0:
            raise MediaError(
                "corrupt-packet",
                f"segment {segment.segment_id} contains no muxable packets",
                stage="archive", path=result.path, stream_index=stream.index,
            )
    finally:
        if output is not None:
            output.close()
        container.close()
    return SegmentRecord(
        segment_id=segment.segment_id,
        track_index=stream.index,
        start_ticks=segment.start_ticks,
        end_ticks=segment.end_ticks,
        path=output_path,
        sha256=sha256_file(output_path),
        keyframe_ticks=keyframes,
        keyframe_hash=keyframe_digest(keyframes),
        audio=None,
    )


def _archive_segment(
    result: ProbeResult,
    segment: Segment,
    settings: AudioSettings,
    color: ColorRecord,
    segment_dir: Path,
) -> tuple[SegmentRecord | None, MediaError | None]:
    stream = result.stream(segment.track_index)
    if stream is None:
        return None, MediaError(
            "type-mismatch",
            f"segment {segment.segment_id} references a stream the source lacks",
            stage="archive", path=result.path, stream_index=segment.track_index,
        )
    try:
        if stream.kind == "video":
            return remux_video_segment(
                result, stream, segment, str(segment_dir / f"{segment.segment_id}.mp4"), color
            ), None
        if stream.kind == "audio":
            audio_path = str(segment_dir / f"{segment.segment_id}.wav")
            audio = extract_segment(result, stream, segment, settings, audio_path)
            return SegmentRecord(
                segment_id=segment.segment_id,
                track_index=stream.index,
                start_ticks=segment.start_ticks,
                end_ticks=segment.end_ticks,
                path=audio_path,
                sha256=audio.sha256,
                audio=audio,
            ), None
        return None, MediaError(
            "type-mismatch",
            f"segment {segment.segment_id} has unsupported kind {stream.kind}",
            stage="archive", path=result.path, stream_index=stream.index,
        )
    except MediaError as exc:
        return None, exc
    except Exception as exc:  # noqa: BLE001 - keep the other segments archiving
        return None, bridge(
            "corrupt-packet", exc, stage="archive",
            path=result.path, stream_index=stream.index,
        )


def _failed_record(segment: Segment, error: MediaError | None) -> SegmentRecord:
    return SegmentRecord(
        segment_id=segment.segment_id,
        track_index=segment.track_index,
        start_ticks=segment.start_ticks,
        end_ticks=segment.end_ticks,
        path="",
        sha256="",
        status="failed",
        errors=[] if error is None else [error],
    )


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def archive(
    input_path: str,
    manifest_path: str,
    *,
    rate: int = 48_000,
    layout: str = "stereo",
    lufs: float | None = None,
    mapping: Iterable[str] = (),
    work_dir: str = ".tmp/work",
    resume: bool = False,
    limits: Limits | None = None,
    cut_every_seconds: float | None = None,
) -> ArchiveReport:
    """Run the archive pipeline; isolated failures do not abort the run."""
    limits = limits or Limits()
    limits.validate()
    settings = AudioSettings(rate=rate, layout=layout, target_lufs=lufs)
    settings.validate()

    store = StageStore(Path(work_dir), resume=resume)
    store.prepare()
    segment_dir = Path(work_dir) / SEGMENT_DIR
    resumed: list[str] = []

    # -- stage: probe ------------------------------------------------------
    source_sha = sha256_file(input_path)
    probe_input = stage_input_digest(
        path=os.path.abspath(input_path),
        sha256=source_sha,
        mapping=list(mapping),
        max_file_bytes=limits.max_file_bytes,
    )
    cached = store.load("probe", probe_input)
    if cached is not None:
        result = ProbeResult.from_dict(cached)
        resumed.append("probe")
    else:
        result = probe(
            input_path, mapping,
            limits={"max_file_bytes": limits.max_file_bytes},
        )
        store.save(
            "probe", probe_input,
            stage_input_digest(probe=result.source_sha256, errors=len(result.errors)),
            result.to_dict(),
        )

    # -- stage: timeline ---------------------------------------------------
    timeline_input = stage_input_digest(
        probe=result.source_sha256,
        max_segment_seconds=limits.max_segment_seconds,
        cut_every_seconds=cut_every_seconds,
    )
    cached = store.load("timeline", timeline_input)
    if cached is not None:
        timeline = Timeline.from_dict(cached["timeline"])
        timeline_errors = [MediaError.from_dict(e) for e in cached.get("errors", [])]
        resumed.append("timeline")
    else:
        timeline, timeline_errors = build_timeline(
            result, limits, cut_every_seconds=cut_every_seconds
        )
        store.save(
            "timeline", timeline_input,
            stage_input_digest(timeline=timeline.to_dict()),
            {
                "timeline": timeline.to_dict(),
                "errors": [e.to_dict() for e in timeline_errors],
            },
        )
    isolated_timeline = _isolate_timeline_errors(timeline_errors, timeline)
    all_timeline_errors = timeline_errors + timeline.validate(path=result.path)
    all_timeline_errors.extend(isolated_timeline)

    # -- stage: color ------------------------------------------------------
    color: ColorRecord | None = None
    if result.video is not None:
        color = resolve_color(result.video, path=result.path)

    manifest = Manifest.create(result, timeline, color=color)
    report = ArchiveReport(manifest=manifest, resumed_stages=resumed)
    report.errors.extend(all_timeline_errors)
    if color is not None:
        report.errors.extend(color.warnings)
    report.errors.extend(result.errors)
    report.stage_status["probe"] = "ok" if not result.errors else "partial"
    report.stage_status["timeline"] = "partial" if all_timeline_errors else "ok"
    report.stage_status["color"] = "partial" if color and color.warnings else "ok"

    # -- stage: transform -------------------------------------------------
    failed_ids = {
        e.detail.get("segment_id")
        for e in all_timeline_errors
        if e.detail.get("segment_id")
    }
    transform_input = stage_input_digest(
        timeline=stage_input_digest(timeline=timeline.to_dict()),
        rate=rate, layout=layout, lufs=lufs,
        source=result.source_sha256,
        isolated=sorted(str(x) for x in failed_ids),
    )
    cached = store.load("transform", transform_input)
    if cached is not None:
        records = [SegmentRecord.from_dict(r) for r in cached.get("segments", [])]
        report.recovered_segments = int(cached.get("recovered_segments", 0))
        resumed.append("transform")
    else:
        records, recovered = _run_transform(
            result, timeline, settings, color, segment_dir, limits, failed_ids
        )
        report.recovered_segments = recovered
        store.save(
            "transform", transform_input,
            stage_input_digest(segments=[r.to_dict() for r in records]),
            {
                "segments": [r.to_dict() for r in records],
                "recovered_segments": recovered,
            },
        )
    manifest.segments = records
    failed = [r for r in records if r.status != "ok"]
    report.errors.extend(e for r in failed for e in r.errors)
    report.stage_status["transform"] = "partial" if failed else "ok"

    # -- stage: archive (manifest on disk) --------------------------------
    probe_output = stage_input_digest(probe=result.source_sha256)
    timeline_output = stage_input_digest(timeline=timeline.to_dict())
    transform_output = stage_input_digest(segments=[r.to_dict() for r in records])
    manifest.record_stage(StageRecord(
        "probe", report.stage_status["probe"], probe_input, probe_output,
        errors=list(result.errors),
        detail={"streams": len(result.streams), "format": result.format_name},
    ))
    manifest.record_stage(StageRecord(
        "timeline", report.stage_status["timeline"], timeline_input, timeline_output,
        errors=all_timeline_errors,
        detail={"tracks": len(timeline.tracks), "segments": len(timeline.segments)},
    ))
    manifest.record_stage(StageRecord(
        "transform", report.stage_status["transform"], transform_input, transform_output,
        errors=[e for r in failed for e in r.errors],
        detail={
            "segments": len(records),
            "failed": len(failed),
            "recovered_segments": report.recovered_segments,
        },
    ))
    manifest.record_stage(StageRecord(
        "archive",
        "ok" if records else "failed",
        transform_output,
        stage_input_digest(manifest=manifest.to_dict()),
        detail={"manifest": os.path.abspath(manifest_path)},
    ))
    report.stage_status["archive"] = manifest.stage("archive").status
    manifest.write(manifest_path)
    if not records:
        raise MediaError(
            "verification-failed", "no segment could be archived",
            stage="archive", path=result.path,
        )
    return report


def _isolate_timeline_errors(
    errors: list[MediaError], timeline: Timeline
) -> list[MediaError]:
    """Attach the affected segment id to timeline errors, for isolation."""
    stamped: list[MediaError] = []
    for error in errors:
        if error.code in ("transition-overlap", "transition-mismatch"):
            continue
        candidates = [
            s for s in timeline.segments
            if error.stream_index is None or s.track_index == error.stream_index
        ]
        if len(candidates) == 1:
            error.detail.setdefault("segment_id", candidates[0].segment_id)
        stamped.append(error)
    return stamped


def _run_transform(
    result: ProbeResult,
    timeline: Timeline,
    settings: AudioSettings,
    color: ColorRecord | None,
    segment_dir: Path,
    limits: Limits,
    failed_ids: set[str],
) -> tuple[list[SegmentRecord], int]:
    from .color import ColorRecord as _ColorRecord

    colors = color or _ColorRecord(stream_index=-1)
    tasks = [s for s in timeline.segments if s.segment_id not in failed_ids]
    skipped = [s for s in timeline.segments if s.segment_id in failed_ids]
    if limits.jobs == 1 or len(tasks) <= 1:
        outcomes = [
            _archive_segment(result, segment, settings, colors, segment_dir)
            for segment in tasks
        ]
    else:
        with ThreadPoolExecutor(max_workers=limits.jobs) as pool:
            outcomes = list(
                pool.map(
                    lambda seg: _archive_segment(result, seg, settings, colors, segment_dir),
                    tasks,
                )
            )
    records: list[SegmentRecord] = []
    recovered = 0
    for segment, (record, error) in zip(tasks, outcomes):
        if record is None:
            records.append(_failed_record(segment, error))
            continue
        records.append(record)
    for segment in skipped:
        error = MediaError(
            "edit-out-of-range",
            f"segment {segment.segment_id} was isolated by timeline validation",
            stage="timeline",
            path=result.path,
            stream_index=segment.track_index,
            detail={"segment_id": segment.segment_id},
        )
        records.append(_failed_record(segment, error))
    records.sort(key=lambda r: (r.track_index, r.start_ticks))
    return records, recovered
