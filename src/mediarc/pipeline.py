"""Pipeline orchestration: parse -> transform -> verify, staged on disk.

Every stage persists a JSON record containing:

* the digest of the inputs it consumed (source hash, settings, upstream stage),
* the digest of the output it produced,
* any isolated errors.

On ``--resume`` a stage is reused only when its recorded input digest matches
the current one; a changed input is rejected with ``stage-hash-mismatch``
instead of silently mixing stale and fresh artefacts.

Segments are the unit of failure isolation: a segment that cannot be archived
is marked failed with its locatable error and the rest of the run continues.
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
from .probe import Keyframe, ProbeResult, StreamProbe, probe
from .timeline import Segment, Track, contiguous_segments, Timeline
from .util import atomic_write_json, load_json, sha256_file

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
    """On-disk stage records, the basis of safe resume."""

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
                "manifest-corrupt", f"stage record {name} is not an object", stage="archive"
            )
        recorded = payload.get("input_sha256")
        if recorded != expected_input:
            raise MediaError(
                "stage-hash-mismatch",
                f"stage {name} was produced from {str(recorded)[:16]}, current input "
                f"hashes to {expected_input[:16]}",
                stage="archive",
                path=str(record_path),
                detail={"recorded": recorded, "current": expected_input},
            )
        output = payload.get("output")
        if isinstance(output, dict):
            return output
        return None

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

    @property
    def healthy(self) -> int:
        return len(self.manifest.healthy_segments())

    @property
    def failed(self) -> int:
        return len(self.manifest.failed_segments())

    def summary_lines(self) -> list[str]:
        lines = [
            f"archive source={self.manifest.source['path']} format={self.manifest.source['format']}",
            f"segments_total={len(self.manifest.segments)} "
            f"segments_ok={self.healthy} segments_failed={self.failed}",
            f"isolated_failures={self.failed} recovered_segments={self.recovered_segments}",
        ]
        for name, status in self.stage_status.items():
            lines.append(f"stage {name} status={status}")
        if self.errors:
            offset = min(
                (e.offset for e in self.errors if e.offset is not None), default=None
            )
            lines.append(
                "error_offset=" + (str(offset) if offset is not None else "unknown")
            )
        return lines


def _mapped_streams(result: ProbeResult) -> list[StreamProbe]:
    return [s for s in result.streams if result.mapping.role_of(s.index) is not None]


def _stream_end_seconds(result: ProbeResult, stream: StreamProbe) -> float:
    if not stream.packets:
        return float(stream.start_raw or 0) / (stream.time_base.value or 1)
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
) -> list[MediaError]:
    """Create tracks and segments in the probe's integer tick base."""
    rate = result.time_base.rate.value
    errors: list[MediaError] = []
    end_ticks = result.time_base.to_ticks(Fraction(result.duration_us or 0, 1_000_000))
    tracks: list[Track] = []
    segments: list[Segment] = []
    for stream in _mapped_streams(result):
        if not stream.packets:
            errors.append(
                MediaError(
                    "missing-stream",
                    f"mapped {stream.kind} stream has no packets; it is isolated from this archive",
                    stage="timeline", path=result.path, stream_index=stream.index,
                )
            )
            continue
        end_seconds = _stream_end_seconds(result, stream)
        stream_end = result.time_base.to_ticks(Fraction(end_seconds))
        stream_start = result.time_base.to_ticks(
            Fraction(stream.start_raw or 0) * stream.time_base.value
        )
        if end_ticks == 0 or stream_end > end_ticks:
            end_ticks = stream_end
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
        segments.extend(
            _segments_for_stream(
                result, stream, stream_start, stream_end, limits, cut_every_seconds
            )
        )
    result.timeline = Timeline(  # type: ignore[attr-defined]
        result.time_base, end_ticks, tracks, segments
    )
    return errors


def _segments_for_stream(
    result: ProbeResult,
    stream: StreamProbe,
    start: int,
    end: int,
    limits: Limits,
    cut_every_seconds: float | None,
) -> list[Segment]:
    rate = result.time_base.rate.value
    limit_ticks = int(limits.max_segment_seconds * rate)
    if cut_every_seconds:
        limit_ticks = min(limit_ticks, int(cut_every_seconds * rate))
    if stream.kind == "video" and stream.keyframes:
        cuts = [
            k.ticks for k in stream.keyframes
            if start < k.ticks < end
        ]
    else:
        cuts = []
    # Enforce the per-segment duration ceiling even between keyframes.
    bounds = [start] + [c for c in cuts if c > start] + [end]
    enforced: list[int] = [bounds[0]]
    for bound in bounds[1:]:
        while bound - enforced[-1] > limit_ticks:
            enforced.append(enforced[-1] + limit_ticks)
        enforced.append(bound)
    segments = contiguous_segments(
        stream.index,
        enforced[1:-1],
        end,
        source_origin=0,
        prefix=f"{stream.kind[0]}{stream.index:02d}_",
    )
    for segment in segments:
        segment.source_start_ticks = segment.start_ticks
        segment.source_end_ticks = segment.end_ticks
        segment.mapping = {
            "kind": stream.kind,
            "codec": stream.codec,
            "role": result.mapping.role_of(stream.index),
        }
    return segments


def _color_values(record: ColorRecord) -> list[tuple[str, int]]:
    values = []
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
    tags = {}
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
    """Stream-copy one video segment into an MP4 without re-encoding."""
    rate = result.time_base.rate.value
    start_seconds = float(Fraction(segment.source_start_ticks or 0) / rate)
    end_seconds = float(Fraction(segment.source_end_ticks or segment.end_ticks) / rate)
    container = av.open(result.path)
    output = None
    try:
        input_stream = container.streams[stream.index]
        if start_seconds > 0:
            try:
                container.seek(int(start_seconds * 1_000_000), stream=input_stream, backward=True)
            except Exception:  # noqa: BLE001
                pass
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
        for packet in container.demux(input_stream):
            if packet.pts is None and packet.dts is None:
                continue
            pts = packet.pts
            if pts is None:
                pts = packet.dts
            when = float(Fraction(pts) * input_stream.time_base)
            duration = float(Fraction(packet.duration or 0) * input_stream.time_base)
            if when + duration <= start_seconds + 1e-9:
                continue
            if when >= end_seconds - 1e-9:
                break
            if base is None:
                base = packet.dts if packet.dts is not None else pts
            if packet.pts is not None:
                packet.pts -= base
            if packet.dts is not None:
                packet.dts -= base
            packet.stream = output_stream
            output.mux(packet)
            count += 1
            if packet.is_keyframe and packet.pts is not None:
                ticks = result.time_base.to_ticks(
                    Fraction(packet.pts + base) * stream.time_base.value
                )
                keyframes.append(ticks)
        if count == 0:
            raise MediaError(
                "corrupt-packet",
                f"segment {segment.segment_id} contains no decodable packets",
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
            f"segment {segment.segment_id} references a stream the source lost",
            stage="archive", path=result.path, stream_index=segment.track_index,
        )
    output_path = str(segment_dir / f"{segment.segment_id}.mp4")
    try:
        if stream.kind == "video":
            return remux_video_segment(result, stream, segment, output_path, color), None
        if stream.kind == "audio":
            audio_path = str(segment_dir / f"{segment.segment_id}.wav")
            audio = extract_segment(result, stream, segment, settings, audio_path)
            record = SegmentRecord(
                segment_id=segment.segment_id,
                track_index=stream.index,
                start_ticks=segment.start_ticks,
                end_ticks=segment.end_ticks,
                path=audio_path,
                sha256=audio.sha256,
                audio=audio,
            )
            return record, None
        return None, MediaError(
            "type-mismatch",
            f"segment {segment.segment_id} has unsupported kind {stream.kind}",
            stage="archive", path=result.path, stream_index=stream.index,
        )
    except MediaError as exc:
        return None, exc
    except Exception as exc:  # noqa: BLE001
        return None, bridge(
            "corrupt-packet", exc, stage="archive",
            path=result.path, stream_index=stream.index,
        )


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

    # -- stage: probe ------------------------------------------------------
    source_sha = sha256_file(input_path)
    probe_input = stage_input_digest(
        path=os.path.abspath(input_path),
        sha256=source_sha,
        mapping=list(mapping),
        limits=limits.to_dict(),
    )
    cached = store.load("probe", probe_input)
    if cached is not None:
        result = _probe_from_dict(cached)
    else:
        result = probe(
            input_path,
            mapping,
            limits={"max_file_bytes": limits.max_file_bytes},
        )
        store.save(
            "probe", probe_input,
            stage_input_digest(probe=result.source_sha256, errors=len(result.errors)),
            result.to_dict(),
        )

    report = ArchiveReport(manifest=Manifest.create(result, Timeline(result.time_base)))
    report.stage_status["probe"] = "ok"

    # -- stage: timeline ---------------------------------------------------
    timeline_input = stage_input_digest(
        probe=result.source_sha256, limits=limits.to_dict(),
        cut_every_seconds=cut_every_seconds,
    )
    cached = store.load("timeline", timeline_input)
    if cached is not None:
        timeline = Timeline.from_dict(cached["timeline"])
        timeline_errors = [MediaError.from_dict(e) for e in cached.get("errors", [])]
    else:
        timeline_errors = build_timeline(result, limits, cut_every_seconds=cut_every_seconds)
        timeline = result.timeline  # type: ignore[attr-defined]
        store.save(
            "timeline", timeline_input,
            stage_input_digest(timeline=timeline.to_dict()),
            {"timeline": timeline.to_dict(),
             "errors": [e.to_dict() for e in timeline_errors]},
        )
    timeline_errors.extend(timeline.validate(path=result.path))
    report.errors.extend(timeline_errors)
    report.stage_status["timeline"] = "partial" if timeline_errors else "ok"

    color: ColorRecord | None = None
    if result.video is not None:
        color = resolve_color(result.video, path=result.path)
        report.errors.extend(color.warnings)
    manifest = Manifest.create(result, timeline, color=color)
    manifest.errors = []  # type: ignore[attr-defined]
    report.manifest = manifest
    report.manifest.notes.extend(e.render() for e in timeline_errors)

    # -- stage: transform (video remux + audio normalise) ------------------
    transform_input = stage_input_digest(
        timeline=stage_input_digest(timeline=timeline.to_dict()),
        rate=rate, layout=layout, lufs=lufs, limits=limits.to_dict(),
        source=result.source_sha256,
    )
    cached = store.load("transform", transform_input)
    if cached is not None:
        records = _segments_from_dict(cached.get("segments", []))
    else:
        records = _run_transform(result, timeline, settings, color, segment_dir, limits)
        store.save(
            "transform", transform_input,
            stage_input_digest(segments=[r.to_dict() for r in records]),
            {"segments": [r.to_dict() for r in records]},
        )
    manifest.segments = records
    failed = [r for r in records if r.status != "ok"]
    report.errors.extend(e for r in failed for e in r.errors)
    report.stage_status["transform"] = "partial" if failed else "ok"

    # -- stage: archive (manifest on disk) --------------------------------
    manifest.record_stage(
        StageRecord(
            "probe", "ok", probe_input,
            stage_input_digest(probe=result.source_sha256),
            errors=list(result.errors),
            detail={"streams": len(result.streams), "format": result.format_name},
        )
    )
    manifest.record_stage(
        StageRecord(
            "timeline", report.stage_status["timeline"], timeline_input,
            stage_input_digest(timeline=timeline.to_dict()),
            errors=timeline_errors,
            detail={"tracks": len(timeline.tracks), "segments": len(timeline.segments)},
        )
    )
    manifest.record_stage(
        StageRecord(
            "transform", report.stage_status["transform"], transform_input,
            stage_input_digest(segments=[r.to_dict() for r in records]),
            errors=[e for r in failed for e in r.errors],
            detail={"segments": len(records), "failed": len(failed)},
        )
    )
    manifest.record_stage(
        StageRecord(
            "archive", "ok" if records else "failed",
            stage_input_digest(segments=[r.sha256 for r in records]),
            stage_input_digest(manifest=manifest.to_dict()),
            detail={"manifest": str(manifest_path)},
        )
    )
    report.stage_status["archive"] = "ok" if records else "failed"
    manifest.write(manifest_path)
    if not records:
        raise MediaError(
            "verification-failed", "no segment could be archived",
            stage="archive", path=result.path,
        )
    return report


def _run_transform(
    result: ProbeResult,
    timeline: Timeline,
    settings: AudioSettings,
    color: ColorRecord | None,
    segment_dir: Path,
    limits: Limits,
) -> list[SegmentRecord]:
    color = color or _empty_color(result)
    tasks = list(timeline.segments)
    if limits.jobs == 1:
        outcomes = [
            _archive_segment(result, segment, settings, color, segment_dir)
            for segment in tasks
        ]
    else:
        with ThreadPoolExecutor(max_workers=limits.jobs) as pool:
            outcomes = list(
                pool.map(
                    lambda seg: _archive_segment(result, seg, settings, color, segment_dir),
                    tasks,
                )
            )
    records: list[SegmentRecord] = []
    for segment, (record, error) in zip(tasks, outcomes):
        if record is None:
            record = SegmentRecord(
                segment_id=segment.segment_id,
                track_index=segment.track_index,
                start_ticks=segment.start_ticks,
                end_ticks=segment.end_ticks,
                path="",
                sha256="",
                status="failed",
                errors=[] if error is None else [error],
            )
        records.append(record)
    return records


def _empty_color(result: ProbeResult) -> ColorRecord:
    return ColorRecord(stream_index=-1)


def _probe_from_dict(data: dict[str, Any]) -> ProbeResult:
    from .probe import MappingRecord, StreamProbe

    streams = []
    for raw in data.get("streams", []):
        stream = StreamProbe(
            index=int(raw["index"]),
            kind=str(raw["kind"]),
            codec=str(raw["codec"]),
            time_base=__import__("mediarc.timeline", fromlist=["Rational"]).Rational.parse(
                raw["time_base"]
            ),
            start_raw=raw.get("start_raw"),
            duration_raw=raw.get("duration_raw"),
            sample_rate=raw.get("sample_rate"),
            channels=raw.get("channels"),
            layout=raw.get("layout"),
            width=raw.get("width"),
            height=raw.get("height"),
            language=raw.get("language"),
            metadata=dict(raw.get("metadata") or {}),
            color_raw=dict(raw.get("color_raw") or {}),
        )
        from .probe import Keyframe, PacketMeta

        stream.keyframes = [Keyframe.from_dict(k) for k in raw.get("keyframes", [])]
        count = int(raw.get("packet_count", 0))
        last_offset = raw.get("last_offset")
        for index in range(count):
            stream.packets.append(
                PacketMeta(index, None, None, None, 0,
                           last_offset if index == count - 1 else None, False, False)
            )
        streams.append(stream)
    return ProbeResult(
        path=str(data["path"]),
        size=int(data["size"]),
        format_name=str(data["format_name"]),
        duration_us=data.get("duration_us"),
        time_base=__import__("mediarc.timeline", fromlist=["TimeBase"]).TimeBase(
            __import__("mediarc.timeline", fromlist=["Rational"]).Rational.parse(
                data["time_base"]
            )
        ),
        streams=streams,
        mapping=MappingRecord.from_dict(data["mapping"]),
        source_sha256=str(data.get("source_sha256", "")),
        errors=[MediaError.from_dict(e) for e in data.get("errors", [])],
        logs=[str(x) for x in data.get("logs", [])],
    )


def _segments_from_dict(data: list[dict[str, Any]]) -> list[SegmentRecord]:
    return [SegmentRecord.from_dict(raw) for raw in data]
